package zai

import (
	"crypto/sha256"
	"encoding/hex"
	"slices"
	"strings"
	"testing"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func exactPairFixture(key string) cpaConfigProjection {
	return cpaConfigProjection{
		AuthDir: "/auth",
		ClaudeKeys: []sdkconfig.ClaudeKey{{
			APIKey:  key,
			BaseURL: zaiAnthropicBaseURL,
			Prefix:  "zai",
		}},
		OpenAICompatibility: []sdkconfig.OpenAICompatibility{{
			Name:    zaiCompatName,
			BaseURL: zaiOpenAIBaseURL,
			APIKeyEntries: []sdkconfig.OpenAICompatibilityAPIKey{{
				APIKey: key,
			}},
		}},
	}
}

func TestDiscoverAccountsExactPair(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture(fixtureKey), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
	account := accounts[0]
	if account.Plan != "pro" || account.FiveHourCredits != 12_000 || account.WeeklyCredits != 60_000 {
		t.Fatalf("unexpected account: %#v", account)
	}
	if strings.Contains(account.Name, fixtureKey) || strings.Contains(account.KeySuffix, fixtureKey) {
		t.Fatal("account output contains full key")
	}
}

func TestDiscoverAccountsWithoutCredentialsIsInactive(t *testing.T) {
	accounts, err := discoverAccounts(cpaConfigProjection{AuthDir: "/auth"}, pluginConfig{DefaultPlan: "pro"})
	if err != nil || len(accounts) != 0 {
		t.Fatalf("discoverAccounts(no credentials, no overrides) = %v, %v; want inactive (no accounts, no error)", accounts, err)
	}
}

func TestDiscoverAccountsOverridesWithoutCredentialsFailClosed(t *testing.T) {
	for name, override := range map[string]accountOverride{
		"enabled":  {KeySuffix: "abc123", Name: "zai-pro-1", Plan: "pro"},
		"disabled": {KeySuffix: "abc123", Name: "zai-pro-1", Plan: "pro", Disabled: true},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := pluginConfig{DefaultPlan: "pro", Accounts: []accountOverride{override}}
			_, err := discoverAccounts(cpaConfigProjection{AuthDir: "/auth"}, cfg)
			if err == nil || !strings.Contains(err.Error(), "accounts[0]: key-suffix matches no account") {
				t.Fatalf("discoverAccounts(no credentials, override) error = %v; want the same fail-closed error as with credentials present", err)
			}
			// The identical override with a credential present fails the same way.
			_, err = discoverAccounts(exactPairFixture(fixtureKey), cfg)
			if err == nil || !strings.Contains(err.Error(), "accounts[0]: key-suffix matches no account") {
				t.Fatalf("discoverAccounts(credential, unmatched override) error = %v", err)
			}
		})
	}
}

func TestDiscoverAccountsMultipleKeys(t *testing.T) {
	fixture := exactPairFixture("key-one-111111")
	fixture.ClaudeKeys = append(fixture.ClaudeKeys, sdkconfig.ClaudeKey{APIKey: "key-two-222222", BaseURL: zaiAnthropicBaseURL})
	fixture.OpenAICompatibility[0].APIKeyEntries = append(fixture.OpenAICompatibility[0].APIKeyEntries, sdkconfig.OpenAICompatibilityAPIKey{APIKey: "key-two-222222"})
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "lite"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 2 || accounts[0].Name != "zai-lite-1" || accounts[1].Name != "zai-lite-2" {
		t.Fatalf("unexpected accounts: %#v", accounts)
	}
}

func TestDiscoverAccountsRejectsAuthenticationCustomHeaders(t *testing.T) {
	tests := []struct {
		name   string
		header string
		mutate func(*cpaConfigProjection, string)
	}{
		{name: "claude authorization", header: " Authorization ", mutate: func(c *cpaConfigProjection, value string) {
			c.ClaudeKeys[0].Headers = map[string]string{" Authorization ": value}
		}},
		{name: "claude api key", header: " X-API-KEY ", mutate: func(c *cpaConfigProjection, value string) {
			c.ClaudeKeys[0].Headers = map[string]string{" X-API-KEY ": value}
		}},
		{name: "compat authorization", header: " authorization ", mutate: func(c *cpaConfigProjection, value string) {
			c.OpenAICompatibility[0].Headers = map[string]string{" authorization ": value}
		}},
		{name: "compat pinned host equivalent", header: " X-ZAI-API-KEY ", mutate: func(c *cpaConfigProjection, value string) {
			c.OpenAICompatibility[0].Headers = map[string]string{" X-ZAI-API-KEY ": value}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const headerValue = "header-secret-must-not-leak"
			fixture := exactPairFixture(fixtureKey)
			tt.mutate(&fixture, headerValue)
			_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
			if err == nil || !strings.Contains(err.Error(), "authentication-affecting custom header") {
				t.Fatalf("error = %v, want authentication header rejection", err)
			}
			if strings.Contains(err.Error(), headerValue) {
				t.Fatalf("error leaked header value for %q: %v", tt.header, err)
			}
		})
	}
}

func TestDiscoverAccountsPairingErrors(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cpaConfigProjection)
		want   string
	}{
		{name: "missing sibling", mutate: func(c *cpaConfigProjection) { c.OpenAICompatibility = nil }, want: "missing zai-coding-plan sibling"},
		{name: "duplicate claude", mutate: func(c *cpaConfigProjection) { c.ClaudeKeys = append(c.ClaudeKeys, c.ClaudeKeys[0]) }, want: "duplicate Z.ai Anthropic"},
		{name: "duplicate compat", mutate: func(c *cpaConfigProjection) {
			c.OpenAICompatibility[0].APIKeyEntries = append(c.OpenAICompatibility[0].APIKeyEntries, c.OpenAICompatibility[0].APIKeyEntries[0])
		}, want: "duplicate zai-coding-plan"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := exactPairFixture(fixtureKey)
			tt.mutate(&fixture)
			_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestPairingErrorDoesNotExposeKeySuffix(t *testing.T) {
	const key = "prefix-secret-finalbytes"
	fixture := exactPairFixture(key)
	fixture.OpenAICompatibility = nil
	_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err == nil {
		t.Fatal("expected pairing error")
	}
	if strings.Contains(err.Error(), "finalbytes") || strings.Contains(err.Error(), key) {
		t.Fatalf("pairing error exposed key bytes: %v", err)
	}
	if !strings.Contains(err.Error(), accountReference(key)) {
		t.Fatalf("pairing error = %v, want non-key account reference", err)
	}
}

func TestShortKeyIsFullyRedacted(t *testing.T) {
	accounts, err := discoverAccounts(exactPairFixture("short"), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].KeySuffix != "redacted" {
		t.Fatalf("key suffix = %q, want redacted", accounts[0].KeySuffix)
	}
}

func TestDiscoverAccountsDoesNotPairBySuffix(t *testing.T) {
	fixture := exactPairFixture("prefix-a-shared")
	fixture.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "prefix-b-shared"
	_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("error = %v, want missing exact sibling", err)
	}
}

func TestAccountOverridesAndAmbiguity(t *testing.T) {
	fixture := exactPairFixture("key-one-abcdef")
	cfg := pluginConfig{Accounts: []accountOverride{{
		KeySuffix:       "abcdef",
		Name:            "primary",
		Plan:            "custom",
		Disabled:        true,
		FiveHourCredits: 100,
		WeeklyCredits:   500,
	}}}
	accounts, err := discoverAccounts(fixture, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if accounts[0].Name != "primary" || !accounts[0].Disabled || accounts[0].FiveHourCredits != 100 {
		t.Fatalf("override not applied: %#v", accounts[0])
	}

	cfg.Accounts = append(cfg.Accounts, accountOverride{KeySuffix: "def", Plan: "pro"})
	_, err = discoverAccounts(fixture, cfg)
	if err == nil || !strings.Contains(err.Error(), "multiple overrides") {
		t.Fatalf("error = %v, want ambiguous suffix", err)
	}
}

func TestRenameAndKeyRotationIdentity(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	keySuffix := fixtureKey[len(fixtureKey)-8:]
	base, err := discoverAccounts(fixture, pluginConfig{Accounts: []accountOverride{{KeySuffix: keySuffix, Name: "old", Plan: "pro"}}})
	if err != nil {
		t.Fatal(err)
	}
	renamed, err := discoverAccounts(fixture, pluginConfig{Accounts: []accountOverride{{KeySuffix: keySuffix, Name: "new", Plan: "pro"}}})
	if err != nil {
		t.Fatal(err)
	}
	if base[0].Identity != renamed[0].Identity {
		t.Fatal("rename changed stable identity")
	}
	rotated, err := discoverAccounts(exactPairFixture(fixtureKey+"-rotated"), pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if base[0].Identity == rotated[0].Identity {
		t.Fatal("key rotation preserved identity")
	}
}

func TestDiscoverAccountsIgnoresUnrelatedEmptyBaseURL(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	fixture.ClaudeKeys = append([]sdkconfig.ClaudeKey{{APIKey: "unmanaged-key"}}, fixture.ClaudeKeys...)
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
}

func TestDiscoverAccountsRejectsDisabledSibling(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	fixture.OpenAICompatibility[0].Disabled = true
	_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err == nil || !strings.Contains(err.Error(), "must be enabled") {
		t.Fatalf("error = %v, want disabled provider rejection", err)
	}
}

func TestDiscoverAccountsRejectsMalformedNamedProvider(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*cpaConfigProjection)
		want   string
	}{
		{name: "wrong base", mutate: func(c *cpaConfigProjection) { c.OpenAICompatibility[0].BaseURL = "https://example.invalid/v1" }, want: "invalid base URL"},
		{name: "no entries", mutate: func(c *cpaConfigProjection) { c.OpenAICompatibility[0].APIKeyEntries = nil }, want: "no API key entries"},
		{name: "empty entry", mutate: func(c *cpaConfigProjection) { c.OpenAICompatibility[0].APIKeyEntries[0].APIKey = "" }, want: "empty API key entry"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fixture := exactPairFixture(fixtureKey)
			tt.mutate(&fixture)
			_, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want %q", err, tt.want)
			}
		})
	}
}

func TestOverrideSuffixMustMatchExactlyOneAccount(t *testing.T) {
	fixture := exactPairFixture("key-one-shared")
	fixture.ClaudeKeys = append(fixture.ClaudeKeys, sdkconfig.ClaudeKey{APIKey: "key-two-shared", BaseURL: zaiAnthropicBaseURL})
	fixture.OpenAICompatibility[0].APIKeyEntries = append(fixture.OpenAICompatibility[0].APIKeyEntries, sdkconfig.OpenAICompatibilityAPIKey{APIKey: "key-two-shared"})
	_, err := discoverAccounts(fixture, pluginConfig{Accounts: []accountOverride{{KeySuffix: "shared", Plan: "pro"}}})
	if err == nil || !strings.Contains(err.Error(), "matches multiple accounts") {
		t.Fatalf("error = %v, want ambiguous account suffix", err)
	}
}

func TestStableAuthIDsUseHostTrimmedRawBaseURL(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	fixture.ClaudeKeys[0].BaseURL = "  HTTPS://API.Z.AI/api/anthropic/  "
	fixture.OpenAICompatibility[0].BaseURL = "  HTTPS://API.Z.AI/api/coding/paas/v4/  "
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := accounts[0].ClaudeAuthID, upstreamStableID("claude:apikey", fixtureKey, "HTTPS://API.Z.AI/api/anthropic/"); got != want {
		t.Fatalf("Claude auth ID = %q, want %q", got, want)
	}
	if got, want := accounts[0].OpenAIAuthID, upstreamStableID("openai-compatibility:zai-coding-plan", fixtureKey, "HTTPS://API.Z.AI/api/coding/paas/v4/", ""); got != want {
		t.Fatalf("OpenAI auth ID = %q, want %q", got, want)
	}
}

func TestStableAuthIDsPreserveHostIterationOrder(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	fixture.ClaudeKeys = append([]sdkconfig.ClaudeKey{{
		APIKey:  "unmanaged-key",
		BaseURL: "https://api.anthropic.com",
	}}, fixture.ClaudeKeys...)
	fixture.OpenAICompatibility = append([]sdkconfig.OpenAICompatibility{{
		Name:    "other-provider",
		BaseURL: "https://other.example/v1",
		APIKeyEntries: []sdkconfig.OpenAICompatibilityAPIKey{{
			APIKey: "other-key",
		}},
	}}, fixture.OpenAICompatibility...)
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("len(accounts) = %d, want 1", len(accounts))
	}
	if got, want := accounts[0].ClaudeAuthID, upstreamStableID("claude:apikey", fixtureKey, zaiAnthropicBaseURL); got != want {
		t.Fatalf("Claude auth ID = %q, want %q", got, want)
	}
	if got, want := accounts[0].OpenAIAuthID, upstreamStableID("openai-compatibility:zai-coding-plan", fixtureKey, zaiOpenAIBaseURL, ""); got != want {
		t.Fatalf("OpenAI auth ID = %q, want %q", got, want)
	}
}

func TestStableClaudeAuthIDsMatchHostRecipes(t *testing.T) {
	fixture := exactPairFixture(fixtureKey)
	fixture.ClaudeKeys[0].ProxyURL = " https://proxy.example "
	fixture.ClaudeKeys[0].Prefix = " zai "
	fixture.ClaudeKeys[0].Headers = map[string]string{"X-Z": "last", "X-A": "first"}
	accounts, err := discoverAccounts(fixture, pluginConfig{DefaultPlan: "pro"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := formatSortedHeaders(fixture.ClaudeKeys[0].Headers), "X-A\x00first\x00X-Z\x00last\x00"; got != want {
		t.Fatalf("sorted headers = %q, want exact bytes %q", got, want)
	}
	want := []string{
		upstreamStableID("claude:apikey", fixtureKey, zaiAnthropicBaseURL),
		upstreamStableID("claude:apikey", fixtureKey, zaiAnthropicBaseURL, "https://proxy.example", "zai", "X-A\x00first\x00X-Z\x00last\x00"),
	}
	if got := accounts[0].claudeAuthIDs; !slices.Equal(got, want) {
		t.Fatalf("Claude auth IDs = %q, want host recipes %q", got, want)
	}
}

func TestStableAuthIDsMatchUpstreamFixtures(t *testing.T) {
	gen := newStableIDGenerator()
	tests := []struct {
		kind  string
		parts []string
		want  string
	}{
		{kind: "claude:apikey", parts: []string{fixtureKey, zaiAnthropicBaseURL}, want: "claude:apikey:e414498ddc81"},
		{kind: "claude:apikey", parts: []string{fixtureKey, zaiAnthropicBaseURL, "", "zai", ""}, want: "claude:apikey:be7b803aa7e1"},
		{kind: "claude:apikey", parts: []string{fixtureKey, zaiAnthropicBaseURL, "https://proxy.example", "zai", "X-A\x00first\x00X-Z\x00last\x00"}, want: "claude:apikey:f1cf8a3b10cb"},
		{kind: "openai-compatibility:zai-coding-plan", parts: []string{fixtureKey, zaiOpenAIBaseURL, ""}, want: "openai-compatibility:zai-coding-plan:f09c6735a12a"},
	}
	for _, tt := range tests {
		if got := gen.next(tt.kind, tt.parts...); got != tt.want {
			t.Fatalf("stable ID = %q, want %q", got, tt.want)
		}
	}
	first := gen.next("claude:apikey", "same", zaiAnthropicBaseURL)
	second := gen.next("claude:apikey", "same", zaiAnthropicBaseURL)
	if first == second || !strings.HasSuffix(second, "-1") {
		t.Fatalf("duplicate counters = %q, %q", first, second)
	}
}

func upstreamStableID(kind string, parts ...string) string {
	hasher := sha256.New()
	_, _ = hasher.Write([]byte(kind))
	for _, part := range parts {
		_, _ = hasher.Write([]byte{0})
		_, _ = hasher.Write([]byte(strings.TrimSpace(part)))
	}
	return kind + ":" + hex.EncodeToString(hasher.Sum(nil))[:12]
}
