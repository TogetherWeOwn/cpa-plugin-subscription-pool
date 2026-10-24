package nvidianim

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const quotaBackoff = time.Hour

func (m *Module) HandleUsage(_ context.Context, r pluginapi.UsageRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.active() {
		return nil
	}
	if _, ok := m.lanes[r.AuthID]; !ok || !r.Failed {
		return nil
	}
	code := r.Failure.StatusCode
	if code != 429 && code != 402 && code != 401 && code != 403 {
		return nil
	}
	now := m.now().UTC()
	delay := retryAfter(r.ResponseHeaders, now)
	// A 402, or a 429 that names credits or a trial, is a payment boundary and
	// latches until an operator re-verifies the entitlement: an automatic
	// resume could spend credit added later for other users of the account.
	// Ordinary RPM 429 bodies also say "quota" or "daily", so that wording
	// alone gets a durable, bounded backoff instead of the latch. The body is
	// inspected only for classification and never saved or logged.
	body := r.Failure.Body
	if len(body) > 16384 {
		body = body[:16384]
	}
	body = strings.ToLower(body)
	if code == 402 || (code == 429 && (strings.Contains(body, "credit") || strings.Contains(body, "trial"))) {
		m.state.FreeExhausted = true
	} else if code == 429 && delay < quotaBackoff && (strings.Contains(body, "quota") || strings.Contains(body, "daily") || strings.Contains(body, "monthly")) {
		delay = quotaBackoff
	}
	if until := now.Add(delay); until.After(m.state.CooldownUntil) {
		m.state.CooldownUntil = until
	}
	return m.save()
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
		if seconds > 86400 {
			seconds = 86400
		}
		delay = time.Duration(seconds) * time.Second
	} else if at, err := http.ParseTime(value); err == nil && at.After(now) {
		delay = at.Sub(now)
	}
	if delay < time.Second {
		return time.Second
	}
	if delay > 24*time.Hour {
		return 24 * time.Hour
	}
	return delay
}
