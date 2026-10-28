package openrouterfree

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestUnknownOrNonzeroPricesAreNeverFree(t *testing.T) {
	for _, price := range []string{`null`, `"1e-9999"`, `1e-9999`, `"-1e-9999"`, `"NaN"`, `""`, `{}`} {
		t.Run(price, func(t *testing.T) {
			pricing := map[string]json.RawMessage{"prompt": json.RawMessage(`"0"`), "completion": json.RawMessage(`"0"`), "request": json.RawMessage(price)}
			if isFreeModel(idA, pricing) {
				t.Fatalf("unverified request price %s admitted as free", price)
			}
		})
	}
	for _, price := range []string{`0`, `0.0`, `"0"`, `" 0.000 "`, `"-0.000e-9999"`, `0e9999`} {
		t.Run("zero_"+price, func(t *testing.T) {
			pricing := map[string]json.RawMessage{"prompt": json.RawMessage(price), "completion": json.RawMessage(`"0"`)}
			if !isFreeModel(idA, pricing) {
				t.Fatalf("verified zero price %s refused", price)
			}
		})
	}
}

func TestReconcileAcceptsEverySuccessfulStatus(t *testing.T) {
	keyFile := filepath.Join(t.TempDir(), "mgmt.key")
	if err := os.WriteFile(keyFile, []byte("test-only-management-key"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, code := range []int{200, 201, 202, 204, 206, 299, 300, 401, 500} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			f := newFake(idA)
			f.patchOK = code
			m := setup(t, hostWith(t, f, entry()), `{"management-key-file":"`+keyFile+`"}`)
			syncNow(t, m)
			s := status(t, m)
			if code >= http.StatusOK && code < http.StatusMultipleChoices {
				if s.HostSync != reconcileSynced || s.HostSyncedAt == nil {
					t.Fatalf("successful PATCH %d reported as %s", code, s.HostSync)
				}
			} else if s.HostSync != fmt.Sprintf("management_http_%d", code) || s.HostSyncedAt != nil {
				t.Fatalf("failed PATCH %d reported as %s", code, s.HostSync)
			}
		})
	}
}

func TestQuotaLookupNeverUsesAKeyFromAnUnverifiedEndpoint(t *testing.T) {
	for _, withValidSibling := range []bool{false, true} {
		t.Run(fmt.Sprint(withValidSibling), func(t *testing.T) {
			f := newFake(idA)
			wrong := entry(idA)
			wrong.BaseURL = "https://other-provider.invalid/api/v1"
			wrong.APIKeyEntries[0].APIKey = "test-only-other-provider-key"
			host := hostWith(t, f, wrong)
			if withValidSibling {
				host.OpenAICompatibility = append(host.OpenAICompatibility, entry(idA))
			}
			m := setup(t, host, `{}`)
			syncNow(t, m)
			keyRequests := 0
			for _, c := range f.calls {
				if c.url == keyURL {
					keyRequests++
					if !withValidSibling || c.auth != "Bearer test-only-openrouter-key" {
						t.Fatal("quota lookup used a credential from an unverified endpoint")
					}
				}
			}
			if withValidSibling && keyRequests != 1 {
				t.Fatalf("valid sibling quota requests = %d, want 1", keyRequests)
			}
		})
	}
}

func TestCredentialMaterialAndRoutingIDsNeverLeaveModuleState(t *testing.T) {
	f := newFake(idA)
	host := hostWith(t, f, entry(idA))
	m := setup(t, host, `{}`)
	var logged []string
	m.logf = func(format string, args ...any) { logged = append(logged, fmt.Sprintf(format, args...)) }
	syncNow(t, m)
	assertPick(t, m, req(m, aliasPrefix+idA), true)
	ids := m.OwnedAuthIDs(context.Background())
	record := req(m, aliasPrefix+idA)
	// A response body may echo a key or internal ID. Only its classification
	// may affect durable cooldown state, never the raw text.
	usage := pluginapi.UsageRecord{AuthID: ids[0], Failed: true}
	usage.Failure.StatusCode = http.StatusUnauthorized
	usage.Failure.Body = host.OpenAICompatibility[0].APIKeyEntries[0].APIKey + " " + ids[0]
	if err := m.HandleUsage(context.Background(), usage); err != nil {
		t.Fatal(err)
	}
	if got := m.UsageAccount(usage); got != "openrouter-free#1" {
		t.Fatalf("usage attribution = %q", got)
	}
	assertPick(t, m, record, false)
	raw, err := m.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	durable, err := os.ReadFile(filepath.Join(host.AuthDir, "providers", providerID, "allowance.state"))
	if err != nil {
		t.Fatal(err)
	}
	for _, output := range []string{string(raw), string(durable), strings.Join(logged, "\n")} {
		for _, secret := range []string{host.OpenAICompatibility[0].APIKeyEntries[0].APIKey, ids[0], strings.TrimPrefix(ids[0], "openai-compatibility:"+providerID+":")} {
			if strings.Contains(output, secret) {
				t.Fatalf("credential material or routing fingerprint leaked")
			}
		}
	}
}
