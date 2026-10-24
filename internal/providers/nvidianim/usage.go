package nvidianim

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

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
	until := now.Add(retryAfter(r.ResponseHeaders, now))
	if until.After(m.state.CooldownUntil) {
		m.state.CooldownUntil = until
	}
	// A rate-limit response may explicitly identify credit/trial exhaustion.
	// Never treat a one-minute RPM refill as a free-credit grant. The body is
	// inspected only for classification and is never saved, echoed or logged.
	body := r.Failure.Body
	if len(body) > 16384 {
		body = body[:16384]
	}
	body = strings.ToLower(body)
	if code == 402 || (code == 429 && (strings.Contains(body, "credit") || strings.Contains(body, "quota") || strings.Contains(body, "trial") || strings.Contains(body, "daily") || strings.Contains(body, "monthly"))) {
		m.state.FreeExhausted = true
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
