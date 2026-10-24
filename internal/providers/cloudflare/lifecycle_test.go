package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStorageWriteFailureNeverAdmitsAndDoesNotLeak(t *testing.T) {
	m, _, _ := configured(t, `{}`)
	m.pollOnce(context.Background(), true)
	if err := m.store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("non-durable reservation admitted")
	}
	raw, err := m.Status(context.Background())
	if err != nil || !strings.Contains(string(raw), "state_unavailable") || strings.Contains(string(raw), "test-token") {
		t.Fatalf("unsafe write-error status %s %v", raw, err)
	}
}

func TestPollStartedYesterdayCannotUnlockToday(t *testing.T) {
	m, now, h := configured(t, `{}`)
	*now = utcDay(*now).Add(24*time.Hour - time.Second)
	h.HTTPClient = doerFunc(func(*http.Request) (*http.Response, error) {
		*now = now.Add(2 * time.Second)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(analyticsBody(`[ {"sum":{"totalNeurons":0}} ]`)))}, nil
	})
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	m.pollOnce(context.Background(), true)
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("previous-day response unlocked today")
	}
}

func TestReconfigureDiscardsInflightOldCredentialPoll(t *testing.T) {
	m, _, h := configured(t, `{}`)
	entered, release := make(chan struct{}), make(chan struct{})
	h.HTTPClient = doerFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-release
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(analyticsBody(`[{"sum":{"totalNeurons":0}}]`)))}, nil
	})
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { m.pollOnce(context.Background(), true); close(done) }()
	<-entered
	if err := m.Reconfigure(context.Background(), h, json.RawMessage(`{"safety-margin-neurons":1000}`)); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	if _, err := m.Pick(context.Background(), pickRequest(m, 0)); err == nil {
		t.Fatal("obsolete poll unlocked reconfigured module")
	}
}

func TestCloseCancelsActiveAnalyticsRequest(t *testing.T) {
	m := NewModule()
	entered := make(chan struct{})
	h := testHost(t.TempDir())
	h.HTTPClient = doerFunc(func(r *http.Request) (*http.Response, error) {
		close(entered)
		<-r.Context().Done()
		return nil, r.Context().Err()
	})
	if err := m.Reconfigure(context.Background(), h, nil); err != nil {
		t.Fatal(err)
	}
	<-entered
	closed := make(chan struct{})
	go func() { _ = m.Close(context.Background()); close(closed) }()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel analytics")
	}
}
