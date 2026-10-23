package synthetic

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestParseQuotaShapes(t *testing.T) {
	cases := []struct {
		name      string
		raw       string
		remaining float64
		shape     string
		regen     *float64
	}{
		{"current plan", `{"planWeeklyLimit":{"percentRemaining":72.5,"nextRegenAt":"2026-10-23T09:00:00.000Z","nextRegenPercent":2}}`, 72.5, "planWeeklyLimit", floatPtr(2)},
		{"legacy token plan", `{"weeklyTokenLimit":{"percentRemaining":40,"nextRegenAt":"2026-10-23T09:00:00Z","maxCredits":"$20","remainingCredits":"$8","nextRegenCredits":"$0.40"},"rollingFiveHourLimit":{"remaining":1,"max":2,"nextTickAt":"2026-10-23T09:00:00Z","tickPercent":0.05}}`, 40, "weeklyTokenLimit", nil},
		{"legacy request pack", `{"rollingFiveHourLimit":{"remaining":150,"max":500,"nextTickAt":"2026-10-23T09:00:00Z","tickPercent":0.05}}`, 30, "rollingFiveHourLimit", floatPtr(5)},
		{"documented example", `{"subscription":{"limit":135,"requests":27,"renewsAt":"2025-09-21T14:36:14.288Z"}}`, 80, "subscription", nil},
	}
	for _, tc := range cases {
		reading, err := parseQuota([]byte(tc.raw))
		if err != nil {
			t.Fatalf("%s: parseQuota error = %v", tc.name, err)
		}
		if reading.PercentRemaining == nil || *reading.PercentRemaining != tc.remaining || reading.Shape != tc.shape {
			t.Fatalf("%s: reading = %+v", tc.name, reading)
		}
		if reading.NextRegenAt.IsZero() {
			t.Fatalf("%s: next regen time not parsed", tc.name)
		}
		if (tc.regen == nil) != (reading.NextRegenPercent == nil) || (tc.regen != nil && *tc.regen != *reading.NextRegenPercent) {
			t.Fatalf("%s: next regen percent = %v, want %v", tc.name, reading.NextRegenPercent, tc.regen)
		}
	}
}

func TestParseQuotaUtilization(t *testing.T) {
	reading, err := parseQuota([]byte(`{"planWeeklyLimit":{"percentRemaining":3,"nextRegenAt":"2026-10-23T09:00:00Z","nextRegenPercent":2}}`))
	if err != nil {
		t.Fatal(err)
	}
	if got := *reading.utilization(); got < 0.969 || got > 0.971 {
		t.Fatalf("utilization = %v, want 0.97", got)
	}
}

func TestParseQuotaRejectsUnknown(t *testing.T) {
	for _, raw := range []string{`{}`, `{"planWeeklyLimit":{}}`, `[]`, `not json`, `{"subscription":{"limit":0,"requests":0}}`} {
		if _, err := parseQuota([]byte(raw)); err == nil {
			t.Fatalf("parseQuota(%s) accepted an unrecognized payload", raw)
		}
	}
}

func TestQuotaFromHeadersIsCaseInsensitive(t *testing.T) {
	headers := http.Header{"x-synthetic-quotas": {`{"planWeeklyLimit":{"percentRemaining":50,"nextRegenAt":"2026-10-23T09:00:00Z","nextRegenPercent":2}}`}}
	reading, ok := quotaFromHeaders(headers)
	if !ok || *reading.PercentRemaining != 50 {
		t.Fatalf("quotaFromHeaders = %+v, %v", reading, ok)
	}
	if _, ok := quotaFromHeaders(http.Header{"X-Other": {"1"}}); ok {
		t.Fatal("quotaFromHeaders accepted a missing header")
	}
}

func TestFetchQuota(t *testing.T) {
	var sawAuth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/v2/quotas":
			_, _ = w.Write([]byte(`{"planWeeklyLimit":{"percentRemaining":64,"nextRegenAt":"2026-10-23T09:00:00Z","nextRegenPercent":2}}`))
		case "/denied":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"Invalid API Key."}`))
		case "/huge":
			_, _ = w.Write([]byte(strings.Repeat("x", maxQuotaResponse+10)))
		default:
			_, _ = w.Write([]byte(`{"unexpected":true}`))
		}
	}))
	defer server.Close()
	ctx := context.Background()
	reading, err := fetchQuota(ctx, server.Client(), server.URL+"/v2/quotas", "secret-key-value")
	if err != nil || *reading.PercentRemaining != 64 {
		t.Fatalf("fetchQuota = %+v, %v", reading, err)
	}
	if sawAuth != "Bearer secret-key-value" {
		t.Fatalf("authorization header = %q", sawAuth)
	}
	for _, path := range []string{"/denied", "/huge", "/other"} {
		_, err := fetchQuota(ctx, server.Client(), server.URL+path, "secret-key-value")
		if err == nil {
			t.Fatalf("fetchQuota(%s) succeeded", path)
		}
		if strings.Contains(err.Error(), "secret-key-value") || strings.Contains(err.Error(), "Invalid API Key") {
			t.Fatalf("fetchQuota(%s) leaked key or body: %v", path, err)
		}
	}
	if _, err := fetchQuota(ctx, server.Client(), "", "k"); err == nil {
		t.Fatal("fetchQuota without endpoint succeeded")
	}
	_ = time.Second
}
