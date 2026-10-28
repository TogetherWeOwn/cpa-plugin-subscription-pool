package cloudflare

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

const testAccount = "0123456789abcdef0123456789abcdef"

func analyticsBody(rows string) string {
	return `{"data":{"viewer":{"accounts":[{"accountTag":"` + testAccount + `","aiInferenceAdaptiveGroups":` + rows + `}]}}}`
}

func TestParseAnalytics(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		want      float64
		bad       bool
	}{
		{"neurons not request count", analyticsBody(`[{"count":99999,"sum":{"totalNeurons":1234.5}}]`), 1234.5, false},
		{"explicit zero", analyticsBody(`[{"sum":{"totalNeurons":0}}]`), 0, false},
		{"empty dataset", analyticsBody(`[]`), 0, false},
		{"multiple aggregates", analyticsBody(`[{"sum":{"totalNeurons":1}},{"sum":{"totalNeurons":2}}]`), 0, true},
		{"null neurons", analyticsBody(`[{"sum":{"totalNeurons":null}}]`), 0, true},
		{"missing neurons", analyticsBody(`[{"count":1}]`), 0, true},
		{"negative", analyticsBody(`[{"sum":{"totalNeurons":-1}}]`), 0, true},
		{"null dataset", analyticsBody(`null`), 0, true},
		{"missing account", `{"data":{"viewer":{"accounts":[]}}}`, 0, true},
		{"wrong account", strings.ReplaceAll(analyticsBody(`[]`), testAccount, strings.Repeat("f", 32)), 0, true},
		{"partial errors", `{"errors":[{"message":"secret upstream text"}],"data":{"viewer":{"accounts":[]}}}`, 0, true},
		{"invalid JSON", `{`, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseAnalytics([]byte(tc.raw), testAccount)
			if (err != nil) != tc.bad || got != tc.want {
				t.Fatalf("got %v %v", got, err)
			}
			if err != nil && strings.Contains(err.Error(), "secret") {
				t.Fatal("upstream text leaked")
			}
		})
	}
}

type doerFunc func(*http.Request) (*http.Response, error)

func (f doerFunc) Do(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchAnalyticsScopeAndDate(t *testing.T) {
	now := time.Date(2026, 10, 24, 23, 59, 0, 0, time.UTC)
	client := doerFunc(func(r *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(r.Body)
		for _, term := range []string{`aiInferenceAdaptiveGroups`, `totalNeurons`, `date_geq`, `date_leq`, `2026-10-24`, testAccount} {
			if !strings.Contains(string(body), term) {
				t.Errorf("query missing %s", term)
			}
		}
		if strings.Contains(string(body), "modelName") || strings.Contains(string(body), "dimensions") {
			t.Fatal("query must aggregate all models and keys")
		}
		if r.URL.String() != analyticsURL || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer test-only-token" {
			t.Fatal("incorrect API request")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(analyticsBody(`[{"sum":{"totalNeurons":42}}]`)))}, nil
	})
	got, err := fetchAnalytics(context.Background(), client, testAccount, "test-only-token", now)
	if err != nil || got != 42 {
		t.Fatalf("%v %v", got, err)
	}
}

func TestAnalyticsErrorsNeverLeakBodyOrTransportSecrets(t *testing.T) {
	for _, client := range []doerFunc{
		func(*http.Request) (*http.Response, error) { return nil, fmt.Errorf("Bearer test-only-token") },
		func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 403, Body: io.NopCloser(strings.NewReader("test-only-token"))}, nil
		},
		func(*http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(strings.Repeat("x", maxAnalyticsResponse+1)))}, nil
		},
	} {
		_, err := fetchAnalytics(context.Background(), client, testAccount, "test-only-token", time.Now())
		if err == nil || strings.Contains(err.Error(), "test-only-token") {
			t.Fatalf("unsafe error %v", err)
		}
	}
}
