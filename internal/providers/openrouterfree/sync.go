package openrouterfree

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const maxKeyFileBytes = 4096

// Reconcile outcomes reported in status. A closed vocabulary: never upstream text.
const (
	reconcileInSync        = "in_sync"
	reconcileSynced        = "synced"
	reconcileNoEntry       = "no_provider_entry"
	reconcileNoKey         = "no_management_key"
	reconcileCatalogFailed = "catalog_unavailable"
)

func logPrintf(format string, args ...any) {
	log.Printf("subscription-pool openrouter-free: "+format, args...)
}

func (m *Module) syncLoop(ctx context.Context) {
	defer close(m.done)
	for {
		m.syncOnce(ctx)
		m.mu.Lock()
		interval := m.cfg.SyncInterval
		m.mu.Unlock()
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-m.kick:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// syncOnce reads OpenRouter's catalog and key tier, records the verified free
// list (which immediately gates admission), then reconciles the host provider.
func (m *Module) syncOnce(ctx context.Context) {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	m.mu.Lock()
	if !m.active() {
		m.mu.Unlock()
		return
	}
	client, apiKey := m.client, m.view.apiKey
	m.mu.Unlock()

	models, catalogErr := fetchFreeModels(ctx, client)
	var limit int64
	limitErr := errors.New("key_unbound")
	if apiKey != "" {
		limit, limitErr = fetchDailyLimit(ctx, client, apiKey)
	}

	m.mu.Lock()
	if m.closed {
		m.mu.Unlock()
		return
	}
	now := m.now().UTC()
	m.sync.LastAttempt = now
	m.sync.LastError = ""
	var logs []string
	switch {
	case catalogErr != nil:
		m.sync.LastError, m.sync.Reconcile = catalogErr.Error(), reconcileCatalogFailed
	default:
		logs = m.applyCatalogLocked(models, now)
	}
	if limitErr == nil {
		m.state.DailyLimit, m.state.LimitAt = limit, now
	} else {
		// An old elevated ceiling is not safe when the current key limit is unknown.
		if m.state.DailyLimit > lowestDailyLimit {
			m.state.DailyLimit, m.state.LimitAt = 0, time.Time{}
		}
		if m.sync.LastError == "" {
			m.sync.LastError = limitErr.Error()
		}
	}
	_ = m.save() // a failed write latches state_unavailable and blocks admission
	cfg, view := m.cfg, m.view
	m.mu.Unlock()
	for _, line := range logs {
		m.logf("%s", line)
	}
	if catalogErr != nil {
		m.logf("catalog sync failed: %s (previous free list stays in force until it goes stale)", catalogErr)
		return
	}
	outcome := m.reconcile(ctx, client, cfg, view, models)
	m.mu.Lock()
	m.sync.Reconcile = outcome
	if outcome == reconcileSynced {
		m.sync.PushedAt = now
	}
	m.mu.Unlock()
	if outcome != reconcileInSync && outcome != reconcileSynced {
		m.logf("host reconcile: %s", outcome)
	}
}

// applyCatalogLocked installs models as the verified free list, recording and
// returning one log line per added or removed model.
func (m *Module) applyCatalogLocked(models []string, now time.Time) []string {
	previous := map[string]bool{}
	for _, id := range m.state.Models {
		previous[id] = true
	}
	current := map[string]bool{}
	var lines []string
	for _, id := range models {
		current[id] = true
		if !previous[id] {
			lines = append(lines, "free model added: "+id)
			m.state.Changes = append(m.state.Changes, change{At: now, Action: "added", Model: id})
		}
	}
	for _, id := range m.state.Models {
		if !current[id] {
			lines = append(lines, "free model removed: "+id)
			m.state.Changes = append(m.state.Changes, change{At: now, Action: "removed", Model: id})
		}
	}
	if over := len(m.state.Changes) - maxChanges; over > 0 {
		m.state.Changes = append([]change(nil), m.state.Changes[over:]...)
	}
	next := m.state
	next.Models, next.CatalogAt = append([]string(nil), models...), now
	m.setState(next)
	return lines
}

func desiredModels(models []string) []providermodule.OpenAICompatibilityModel {
	out := make([]providermodule.OpenAICompatibilityModel, 0, len(models))
	for _, id := range models {
		out = append(out, providermodule.OpenAICompatibilityModel{Name: id, Alias: aliasPrefix + id})
	}
	return out
}

func sameModels(a, b []providermodule.OpenAICompatibilityModel) bool {
	key := func(list []providermodule.OpenAICompatibilityModel) []string {
		out := make([]string, 0, len(list))
		for _, x := range list {
			out = append(out, x.Name+"\x00"+x.Alias)
		}
		sort.Strings(out)
		return out
	}
	ka, kb := key(a), key(b)
	if len(ka) != len(kb) {
		return false
	}
	for i := range ka {
		if ka[i] != kb[i] {
			return false
		}
	}
	return true
}

// reconcile makes the host's openrouter-free entry carry exactly the free
// models. It patches only `models` through the host's own management API, so
// credentials in the entry are never read or rewritten by this plugin and the
// host does the persist and reload.
func (m *Module) reconcile(ctx context.Context, client providermodule.HTTPDoer, cfg moduleConfig, view hostView, models []string) string {
	if !view.present {
		return reconcileNoEntry
	}
	desired := desiredModels(models)
	if sameModels(view.models, desired) {
		return reconcileInSync
	}
	if cfg.ManagementKeyFile == "" {
		return reconcileNoKey
	}
	key, err := readKeyFile(cfg.ManagementKeyFile)
	if err != nil {
		return reconcileNoKey
	}
	payload, err := json.Marshal(map[string]any{
		"name":  providerID,
		"value": map[string]any{"models": modelsJSON(desired)},
	})
	if err != nil {
		return "management_request_failed"
	}
	_, status, err := doJSON(ctx, client, http.MethodPatch, cfg.ManagementURL+"/v0/management/openai-compatibility", key, payload)
	switch {
	case err != nil:
		return "management_" + err.Error()
	case status == http.StatusOK || status == http.StatusNoContent:
		return reconcileSynced
	default:
		return "management_http_" + strconv.Itoa(status)
	}
}

func modelsJSON(models []providermodule.OpenAICompatibilityModel) []map[string]string {
	out := make([]map[string]string, 0, len(models))
	for _, x := range models {
		out = append(out, map[string]string{"name": x.Name, "alias": x.Alias})
	}
	return out
}

func readKeyFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	raw, err := io.ReadAll(io.LimitReader(f, maxKeyFileBytes+1))
	if err != nil {
		return "", err
	}
	key := strings.TrimSpace(string(raw))
	if key == "" || len(raw) > maxKeyFileBytes || strings.ContainsAny(key, "\r\n") {
		return "", errors.New("invalid key file")
	}
	return key, nil
}
