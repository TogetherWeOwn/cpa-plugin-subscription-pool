package zai

import (
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestParseRateLimitHintHeaders(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	tenMinutes := now.Add(10 * time.Minute)
	cases := []struct {
		name       string
		header     string
		value      string
		wantAt     time.Time
		wantSource string
	}{
		{"epoch seconds", "X-RateLimit-Reset", strconv.FormatInt(tenMinutes.Unix(), 10), tenMinutes, "x-ratelimit-reset header"},
		{"epoch milliseconds", "X-Rate-Limit-Reset", strconv.FormatInt(tenMinutes.UnixMilli(), 10), tenMinutes, "x-rate-limit-reset header"},
		{"delta seconds", "RateLimit-Reset", "90", now.Add(90 * time.Second), "ratelimit-reset header"},
		{"RFC3339", "X-RateLimit-Reset", tenMinutes.Format(time.RFC3339Nano), tenMinutes, "x-ratelimit-reset header"},
		{"retry-after seconds", "Retry-After", "45", now.Add(45 * time.Second), "retry-after header"},
		{"retry-after HTTP date", "Retry-After", tenMinutes.Format(http.TimeFormat), tenMinutes, "retry-after header"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := pluginapi.UsageRecord{ResponseHeaders: http.Header{}}
			record.ResponseHeaders.Set(tc.header, tc.value)
			resetAt, source, ok := parseRateLimitHint(record, now)
			if !ok || !resetAt.Equal(tc.wantAt) || source != tc.wantSource {
				t.Fatalf("hint = %v %q %t, want %v %q", resetAt, source, ok, tc.wantAt, tc.wantSource)
			}
		})
	}
}

// Each of these hints would either release an exhausted account before its
// allowance resets or park it far beyond any real Z.ai window, so they must be
// ignored and leave the caller on its conservative fallback.
func TestParseRateLimitHintRejectsUnsafeHeaders(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		header string
		value  string
	}{
		{"past epoch", "X-RateLimit-Reset", strconv.FormatInt(now.Add(-time.Minute).Unix(), 10)},
		{"reset equal to now", "X-RateLimit-Reset", strconv.FormatInt(now.Unix(), 10)},
		{"beyond eight days", "X-RateLimit-Reset", strconv.FormatInt(now.Add(9*24*time.Hour).Unix(), 10)},
		{"delta beyond eight days", "RateLimit-Reset", strconv.FormatInt(int64(9*24*time.Hour/time.Second), 10)},
		{"zero", "X-RateLimit-Reset", "0"},
		{"negative", "X-RateLimit-Reset", "-30"},
		{"retry-after zero", "Retry-After", "0"},
		{"retry-after negative", "Retry-After", "-5"},
		{"retry-after past date", "Retry-After", now.Add(-time.Hour).Format(http.TimeFormat)},
		{"garbage", "X-RateLimit-Reset", "soon"},
		{"oversized", "X-RateLimit-Reset", strings.Repeat("1", maxResetHintLength+1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := pluginapi.UsageRecord{ResponseHeaders: http.Header{}}
			record.ResponseHeaders.Set(tc.header, tc.value)
			if resetAt, source, ok := parseRateLimitHint(record, now); ok {
				t.Fatalf("accepted unsafe hint: %v from %q", resetAt, source)
			}
		})
	}
}

func TestParseRateLimitHintFallsThroughInvalidHeaderToBody(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	record := pluginapi.UsageRecord{
		ResponseHeaders: http.Header{"Retry-After": []string{"-1"}},
		Failure:         pluginapi.UsageFailure{Body: `{"retry_after":120}`},
	}
	resetAt, source, ok := parseRateLimitHint(record, now)
	if !ok || !resetAt.Equal(now.Add(2*time.Minute)) || source != "rate-limit response body" {
		t.Fatalf("hint = %v %q %t", resetAt, source, ok)
	}
}

func TestParseRateLimitHintBody(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	tenMinutes := now.Add(10 * time.Minute)
	cases := []struct {
		name   string
		body   string
		wantAt time.Time
	}{
		{"nested RFC3339 string", `{"error":{"code":"1308","reset_at":"` + tenMinutes.Format(time.RFC3339) + `"}}`, tenMinutes},
		{"inside array", `{"errors":[{"next-reset-time":` + strconv.FormatInt(tenMinutes.UnixMilli(), 10) + `}]}`, tenMinutes},
		{"key normalization", `{"Retry-After":30}`, now.Add(30 * time.Second)},
		{"retry_after_ms", `{"retry_after_ms":1500}`, now.Add(1500 * time.Millisecond)},
		{"epoch seconds string", `{"resetTime":"` + strconv.FormatInt(tenMinutes.Unix(), 10) + `"}`, tenMinutes},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := pluginapi.UsageRecord{Failure: pluginapi.UsageFailure{Body: tc.body}}
			resetAt, source, ok := parseRateLimitHint(record, now)
			if !ok || !resetAt.Equal(tc.wantAt) || source != "rate-limit response body" {
				t.Fatalf("hint = %v %q %t, want %v", resetAt, source, ok, tc.wantAt)
			}
		})
	}
}

func TestParseRateLimitHintRejectsUnsafeBodies(t *testing.T) {
	now := time.Date(2026, time.September, 8, 12, 0, 0, 0, time.UTC)
	tooDeep := `{"reset":60}`
	for range 7 {
		tooDeep = `{"a":` + tooDeep + `}`
	}
	cases := []struct {
		name string
		body string
	}{
		{"key outside allowlist", `{"timestamp":60}`},
		{"boolean value", `{"reset":true}`},
		{"object value", `{"reset":{"seconds":60}}`},
		{"retry_after_ms beyond eight days", `{"retry_after_ms":` + strconv.FormatInt(int64(9*24*time.Hour/time.Millisecond), 10) + `}`},
		{"past epoch", `{"reset_at":` + strconv.FormatInt(now.Add(-time.Minute).Unix(), 10) + `}`},
		{"trailing JSON value", `{"reset":60} {"reset":60}`},
		{"malformed JSON", `{"reset":60`},
		{"nested beyond depth limit", tooDeep},
		{"oversized body", `{"reset":60,"pad":"` + strings.Repeat("x", maxFailureBodyBytes) + `"}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			record := pluginapi.UsageRecord{Failure: pluginapi.UsageFailure{Body: tc.body}}
			if resetAt, source, ok := parseRateLimitHint(record, now); ok {
				t.Fatalf("accepted unsafe body hint: %v from %q", resetAt, source)
			}
		})
	}
}
