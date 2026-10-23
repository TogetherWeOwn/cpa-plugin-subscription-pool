package zai

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"sort"
	"strings"
)

const (
	zaiAnthropicBaseURL = "https://api.z.ai/api/anthropic"
	zaiOpenAIBaseURL    = "https://api.z.ai/api/coding/paas/v4"
	zaiCompatName       = "zai-coding-plan"
)

var authenticationHeaderNames = map[string]struct{}{
	"api-key":       {},
	"authorization": {},
	"x-api-key":     {},
	"x-zai-api-key": {},
}

type account struct {
	Identity                string `json:"-"`
	Name                    string `json:"name"`
	KeySuffix               string `json:"key_suffix"`
	Plan                    string `json:"plan"`
	Disabled                bool   `json:"disabled,omitempty"`
	FiveHourCredits         int64  `json:"five_hour_credits"`
	WeeklyCredits           int64  `json:"weekly_credits"`
	ClaudeAuthID            string `json:"claude_auth_id"`
	OpenAIAuthID            string `json:"openai_auth_id"`
	claudeAuthIDs           []string
	key                     string
	planExplicit            bool
	fiveHourCreditsExplicit bool
	weeklyCreditsExplicit   bool
}

type pairCandidate struct {
	key              string
	claudeCount      int
	openAIEntryCount int
}

type stableIDGenerator struct {
	counters map[string]int
}

func newStableIDGenerator() *stableIDGenerator {
	return &stableIDGenerator{counters: make(map[string]int)}
}

func (g *stableIDGenerator) next(kind string, parts ...string) string {
	short := stableAuthID(kind, parts...)[:12]
	key := kind + ":" + short
	index := g.counters[key]
	g.counters[key] = index + 1
	if index > 0 {
		short = fmt.Sprintf("%s-%d", short, index)
	}
	return kind + ":" + short
}

// stableAuthID deliberately reproduces CLIProxyAPI's non-security
// interoperability identifier. The host contract requires these exact bytes.
func stableAuthID(kind string, parts ...string) string {
	encoded := make([]byte, 0, len(kind)+len(parts)*16)
	encoded = append(encoded, kind...)
	for _, part := range parts {
		encoded = append(encoded, 0)
		encoded = append(encoded, strings.TrimSpace(part)...)
	}
	return sha256Hex(encoded)
}

// sha256Hex is retained solely for the upstream stable auth-ID contract.
//
//go:noinline
func sha256Hex(value []byte) string {
	digest := sha256.Sum256(value) // codeql[go/weak-sensitive-data-hashing] Upstream non-security ID contract.
	return hex.EncodeToString(digest[:])
}

func formatSortedHeaders(headers map[string]string) string {
	if len(headers) == 0 {
		return ""
	}
	keys := make([]string, 0, len(headers))
	for key := range headers {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var encoded strings.Builder
	for _, key := range keys {
		encoded.WriteString(key)
		encoded.WriteByte(0)
		encoded.WriteString(headers[key])
		encoded.WriteByte(0)
	}
	return encoded.String()
}

func discoverAccounts(cpa cpaConfigProjection, cfg pluginConfig) ([]account, error) {
	pairs := make(map[string]*pairCandidate)

	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		_, recognized := recognizedBaseURL(entry.BaseURL, zaiAnthropicBaseURL)
		if !recognized {
			continue
		}
		if key == "" {
			return nil, fmt.Errorf("z.ai Anthropic entry has no API key")
		}
		if header, found := authenticationHeader(entry.Headers); found {
			return nil, fmt.Errorf("z.ai Anthropic entry contains authentication-affecting custom header %s", header)
		}
		pair := pairs[key]
		if pair == nil {
			pair = &pairCandidate{key: key}
			pairs[key] = pair
		}
		pair.claudeCount++
	}

	for i := range cpa.OpenAICompatibility {
		compat := cpa.OpenAICompatibility[i]
		if strings.ToLower(strings.TrimSpace(compat.Name)) != zaiCompatName {
			continue
		}
		if compat.Disabled {
			return nil, fmt.Errorf("zai-coding-plan provider must be enabled")
		}
		if _, recognized := recognizedBaseURL(compat.BaseURL, zaiOpenAIBaseURL); !recognized {
			return nil, fmt.Errorf("zai-coding-plan provider has invalid base URL")
		}
		if header, found := authenticationHeader(compat.Headers); found {
			return nil, fmt.Errorf("zai-coding-plan provider contains authentication-affecting custom header %s", header)
		}
		if len(compat.APIKeyEntries) == 0 {
			return nil, fmt.Errorf("zai-coding-plan provider has no API key entries")
		}
		for j := range compat.APIKeyEntries {
			entry := compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			if key == "" {
				return nil, fmt.Errorf("zai-coding-plan provider contains an empty API key entry")
			}
			pair := pairs[key]
			if pair == nil {
				pair = &pairCandidate{key: key}
				pairs[key] = pair
			}
			pair.openAIEntryCount++
		}
	}

	if len(pairs) == 0 {
		// No Z.ai credential exists in the host config at all: the zai module
		// is inactive. It owns no auth IDs and recognizes no candidates, so
		// the other provider modules hosted by the coordinator keep working
		// for operators who do not (or no longer) hold a Z.ai plan. A partial
		// or malformed Z.ai configuration still fails closed below, and so
		// does an accounts override: it names an account the operator
		// expects, so silently dropping it would hide a lost credential.
		if len(cfg.Accounts) > 0 {
			return nil, fmt.Errorf("accounts[0]: key-suffix matches no account (the host config has no Z.ai credential; remove providers.zai.accounts if the Z.ai plan is gone)")
		}
		return nil, nil
	}
	if err := validatePairs(pairs); err != nil {
		return nil, err
	}

	legacyClaudeIDs := make(map[string]string, len(pairs))
	currentClaudeIDs := make(map[string]string, len(pairs))
	legacyIDGen := newStableIDGenerator()
	currentIDGen := newStableIDGenerator()
	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		if key == "" {
			continue
		}
		legacyID := legacyIDGen.next(
			"claude:apikey",
			key,
			strings.TrimSpace(entry.BaseURL),
		)
		currentID := currentIDGen.next(
			"claude:apikey",
			key,
			strings.TrimSpace(entry.BaseURL),
			strings.TrimSpace(entry.ProxyURL),
			strings.TrimSpace(entry.Prefix),
			formatSortedHeaders(entry.Headers),
		)
		baseURL, err := normalizedBaseURL(entry.BaseURL)
		if err == nil {
			if pair := pairs[key]; pair != nil && pair.claudeCount == 1 && pair.openAIEntryCount == 1 && baseURL == zaiAnthropicBaseURL {
				legacyClaudeIDs[key] = legacyID
				currentClaudeIDs[key] = currentID
			}
		}
	}

	openAIIDs := make(map[string]string, len(pairs))
	openAIIDGen := newStableIDGenerator()
	for i := range cpa.OpenAICompatibility {
		compat := cpa.OpenAICompatibility[i]
		if compat.Disabled {
			continue
		}
		providerName := strings.ToLower(strings.TrimSpace(compat.Name))
		if providerName == "" {
			providerName = "openai-compatibility"
		}
		baseURL, baseRecognized := recognizedBaseURL(compat.BaseURL, zaiOpenAIBaseURL)
		for j := range compat.APIKeyEntries {
			entry := compat.APIKeyEntries[j]
			key := strings.TrimSpace(entry.APIKey)
			if key == "" {
				continue
			}
			idKind := "openai-compatibility:" + providerName
			id := openAIIDGen.next(idKind, key, strings.TrimSpace(compat.BaseURL), strings.TrimSpace(entry.ProxyURL))
			if baseRecognized && providerName == zaiCompatName && baseURL == zaiOpenAIBaseURL {
				openAIIDs[key] = id
			}
		}
	}

	ordered := make([]account, 0, len(pairs))
	for i := range cpa.ClaudeKeys {
		entry := cpa.ClaudeKeys[i]
		key := strings.TrimSpace(entry.APIKey)
		pair := pairs[key]
		if pair == nil || pair.claudeCount != 1 || pair.openAIEntryCount != 1 {
			continue
		}
		item := account{
			Identity:      accountIdentity(key),
			KeySuffix:     displaySuffix(key),
			ClaudeAuthID:  legacyClaudeIDs[key],
			OpenAIAuthID:  openAIIDs[key],
			claudeAuthIDs: uniqueStrings(legacyClaudeIDs[key], currentClaudeIDs[key]),
			key:           key,
		}
		if len(item.claudeAuthIDs) == 0 || item.OpenAIAuthID == "" {
			return nil, fmt.Errorf("paired account %s has incomplete auth IDs", item.KeySuffix)
		}
		ordered = append(ordered, item)
	}

	if err := validateAccountAuthIDs(ordered); err != nil {
		return nil, err
	}
	if err := applyAccountOverrides(ordered, cfg); err != nil {
		return nil, err
	}
	return ordered, nil
}

func validateAccountAuthIDs(accounts []account) error {
	owners := make(map[string]string, len(accounts)*3)
	for _, item := range accounts {
		for _, authID := range accountAuthIDs(item) {
			if owner := owners[authID]; owner != "" && owner != item.Identity {
				return fmt.Errorf("host auth ID collision between validated accounts")
			}
			owners[authID] = item.Identity
		}
	}
	return nil
}

func uniqueStrings(values ...string) []string {
	unique := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		unique = append(unique, value)
	}
	return unique
}

func authenticationHeader(headers map[string]string) (string, bool) {
	for name := range headers {
		normalized := strings.ToLower(strings.TrimSpace(name))
		if _, forbidden := authenticationHeaderNames[normalized]; forbidden {
			return normalized, true
		}
	}
	return "", false
}

func validatePairs(pairs map[string]*pairCandidate) error {
	for _, pair := range pairs {
		reference := accountReference(pair.key)
		switch {
		case pair.claudeCount > 1:
			return fmt.Errorf("duplicate Z.ai Anthropic entry for account %s", reference)
		case pair.openAIEntryCount > 1:
			return fmt.Errorf("duplicate zai-coding-plan entry for account %s", reference)
		case pair.claudeCount == 0:
			return fmt.Errorf("missing Z.ai Anthropic sibling for account %s", reference)
		case pair.openAIEntryCount == 0:
			return fmt.Errorf("missing zai-coding-plan sibling for account %s", reference)
		}
	}
	return nil
}

func applyAccountOverrides(accounts []account, cfg pluginConfig) error {
	matched := make(map[int]int, len(cfg.Accounts))
	for accountIndex := range accounts {
		matches := make([]int, 0, 1)
		for overrideIndex := range cfg.Accounts {
			if strings.HasSuffix(accounts[accountIndex].key, cfg.Accounts[overrideIndex].KeySuffix) {
				matches = append(matches, overrideIndex)
			}
		}
		if len(matches) > 1 {
			return fmt.Errorf("account suffix %s matches multiple overrides", accounts[accountIndex].KeySuffix)
		}
		var override *accountOverride
		if len(matches) == 1 {
			index := matches[0]
			matched[index]++
			override = &cfg.Accounts[index]
		}
		if err := applyAccountOverride(&accounts[accountIndex], override, cfg.DefaultPlan, accountIndex); err != nil {
			return err
		}
	}
	for i := range cfg.Accounts {
		switch matched[i] {
		case 0:
			return fmt.Errorf("accounts[%d]: key-suffix matches no account", i)
		case 1:
		default:
			return fmt.Errorf("accounts[%d]: key-suffix matches multiple accounts", i)
		}
	}
	seenNames := make(map[string]struct{}, len(accounts))
	for i := range accounts {
		key := strings.ToLower(accounts[i].Name)
		if _, exists := seenNames[key]; exists {
			return fmt.Errorf("duplicate account name %q", accounts[i].Name)
		}
		seenNames[key] = struct{}{}
	}
	return nil
}

func applyAccountOverride(target *account, override *accountOverride, defaultPlan string, index int) error {
	plan := defaultPlan
	if override != nil && override.Plan != "" {
		plan = override.Plan
	}
	if plan == "" {
		return fmt.Errorf("account %s has no plan", target.KeySuffix)
	}
	buckets, knownPlan := planBuckets[plan]
	if plan == "custom" {
		knownPlan = true
	}
	if !knownPlan {
		return fmt.Errorf("account %s has invalid plan %q", target.KeySuffix, plan)
	}
	if override != nil {
		if override.Name != "" {
			target.Name = override.Name
		}
		target.Disabled = override.Disabled
		target.planExplicit = override.Plan != ""
		target.fiveHourCreditsExplicit = override.FiveHourCredits > 0
		target.weeklyCreditsExplicit = override.WeeklyCredits > 0
		if target.fiveHourCreditsExplicit {
			buckets.FiveHour = override.FiveHourCredits
		}
		if target.weeklyCreditsExplicit {
			buckets.Weekly = override.WeeklyCredits
		}
	}
	if !validCreditBucket(buckets.FiveHour) || !validCreditBucket(buckets.Weekly) {
		return fmt.Errorf("account %s has non-positive or out-of-range credit bucket", target.KeySuffix)
	}
	if target.Name == "" {
		target.Name = fmt.Sprintf("zai-%s-%d", plan, index+1)
	}
	target.Plan = plan
	target.FiveHourCredits = buckets.FiveHour
	target.WeeklyCredits = buckets.Weekly
	return nil
}

func recognizedBaseURL(raw, expected string) (string, bool) {
	normalized, err := normalizedBaseURL(raw)
	return normalized, err == nil && normalized == expected
}

func normalizedBaseURL(raw string) (string, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", fmt.Errorf("base-url is required")
	}
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil {
		return "", fmt.Errorf("invalid base-url")
	}
	if parsed.RawQuery != "" || parsed.Fragment != "" {
		return "", fmt.Errorf("base-url must not include query or fragment")
	}
	parsed.Scheme = strings.ToLower(parsed.Scheme)
	parsed.Host = strings.ToLower(parsed.Host)
	parsed.Path = strings.TrimRight(parsed.Path, "/")
	parsed.RawPath = ""
	return parsed.String(), nil
}

// zaiIdentityNamespace is deliberately decoupled from the coordinator's
// plugin ID: renaming the plugin must never change existing account
// identity hashes.
const zaiIdentityNamespace = "zai-coding-plan:account-identity:v1"

func accountIdentity(key string) string {
	mac := hmac.New(sha256.New, []byte(zaiIdentityNamespace))
	_, _ = mac.Write([]byte(strings.TrimSpace(key)))
	return hex.EncodeToString(mac.Sum(nil))
}

func accountReference(key string) string {
	identity := accountIdentity(key)
	return identity[:12]
}

func displaySuffix(key string) string {
	trimmed := strings.TrimSpace(key)
	const suffixLength = 6
	if len(trimmed) <= suffixLength {
		return "redacted"
	}
	return trimmed[len(trimmed)-suffixLength:]
}
