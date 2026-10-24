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
	// Only 402 is an explicit payment boundary and latches until an operator
	// clears it. Ordinary RPM 429 bodies also say "quota" or "daily", so a
	// worded 429 gets a durable, bounded backoff instead of a permanent latch:
	// a one-minute refill is still never treated as a free-credit grant. The
	// body is inspected only for classification and never saved or logged.
	body := r.Failure.Body
	if len(body) > 16384 {
		body = body[:16384]
	}
	body = strings.ToLower(body)
	if code == 402 {
		m.state.FreeExhausted = true
	} else if code == 429 && delay < quotaBackoff && (strings.Contains(body, "credit") || strings.Contains(body, "quota") || strings.Contains(body, "trial") || strings.Contains(body, "daily") || strings.Contains(body, "monthly")) {
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
