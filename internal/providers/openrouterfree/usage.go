package openrouterfree

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const authBackoff = time.Hour

// HandleUsage turns upstream refusals into durable cooldowns. A 429 naming the
// daily free-model allowance rests the provider until 00:00 UTC; any other 429
// honours Retry-After; 401/402/403 rest it for an hour because they mean the
// key, not the model, is the problem. The body is inspected for classification
// only and never saved or logged.
func (m *Module) HandleUsage(_ context.Context, r pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return nil
	}
	if _, ok := m.view.lanes[r.AuthID]; !ok || !r.Failed {
		return nil
	}
	code := r.Failure.StatusCode
	if code != 429 && code != 402 && code != 401 && code != 403 {
		return nil
	}
	now := m.now().UTC()
	until := now.Add(retryAfter(r.ResponseHeaders, now))
	body := r.Failure.Body
	if len(body) > 16384 {
		body = body[:16384]
	}
	body = strings.ToLower(body)
	switch {
	case code == 429 && strings.Contains(body, "free-models-per-day"):
		until = nextUTCMidnight(now)
		limit, _ := m.dailyLimit()
		m.state.Used = max(m.state.Used, limit)
	case code != 429:
		if floor := now.Add(authBackoff); until.Before(floor) {
			until = floor
		}
	}
	if until.After(m.state.CooldownUntil) {
		m.state.CooldownUntil = until
	}
	return m.save()
}

func nextUTCMidnight(now time.Time) time.Time {
	y, mo, d := now.UTC().Date()
	return time.Date(y, mo, d+1, 0, 0, 0, 0, time.UTC)
}

func retryAfter(h http.Header, now time.Time) time.Duration {
	value := ""
	for k, v := range h {
		if strings.EqualFold(k, "Retry-After") && len(v) > 0 {
			value = v[0]
			break
		}
	}
	delay := time.Minute
	if seconds, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64); err == nil && seconds >= 0 {
		delay = time.Duration(min(seconds, 86400)) * time.Second
	} else if at, err := http.ParseTime(value); err == nil && at.After(now) {
		delay = at.Sub(now)
	}
	return min(max(delay, time.Second), 24*time.Hour)
}
