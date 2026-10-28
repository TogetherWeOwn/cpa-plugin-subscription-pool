package openrouterfree

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const (
	catalogURL       = "https://openrouter.ai/api/v1/models"
	keyURL           = "https://openrouter.ai/api/v1/key"
	maxResponseBytes = 8 << 20
	requestTimeout   = 20 * time.Second
	freeSuffix       = ":free"
)

func newHTTPClient() *http.Client {
	return &http.Client{Timeout: requestTimeout, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func doJSON(ctx context.Context, client providermodule.HTTPDoer, method, target, bearer string, body []byte) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = strings.NewReader(string(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return nil, 0, fmt.Errorf("request_failed")
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	res, err := client.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("transport_failed")
	}
	if res == nil || res.Body == nil {
		return nil, 0, fmt.Errorf("empty_response")
	}
	defer func() { _ = res.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(res.Body, maxResponseBytes+1))
	if err != nil || len(data) > maxResponseBytes {
		return nil, res.StatusCode, fmt.Errorf("response_unreadable")
	}
	return data, res.StatusCode, nil
}

// isFreeModel is deliberately stricter than "prompt and completion are 0":
// every price OpenRouter lists for the model must parse and be zero, and the
// id must carry the :free suffix. Anything unparseable is not free.
func isFreeModel(id string, pricing map[string]json.RawMessage) bool {
	if !strings.HasSuffix(strings.ToLower(id), freeSuffix) || strings.ContainsAny(id, "\r\n\t ") {
		return false
	}
	for _, required := range []string{"prompt", "completion"} {
		if _, ok := pricing[required]; !ok {
			return false
		}
	}
	for _, raw := range pricing {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			var n float64
			if json.Unmarshal(raw, &n) != nil || n != 0 {
				return false
			}
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || v != 0 {
			return false
		}
	}
	return true
}

// fetchFreeModels returns the sorted, de-duplicated ids of OpenRouter's
// current free text models. An empty or malformed catalog is an error so an
// outage can never be read as "every free model was removed".
func fetchFreeModels(ctx context.Context, client providermodule.HTTPDoer) ([]string, error) {
	body, status, err := doJSON(ctx, client, http.MethodGet, catalogURL, "", nil)
	if err != nil {
		return nil, fmt.Errorf("catalog_%w", err)
	}
	if status != http.StatusOK {
		return nil, fmt.Errorf("catalog_http_%d", status)
	}
	models, err := parseCatalog(body)
	if err != nil {
		return nil, err
	}
	// A catalog that parses but lists no free model is far likelier a schema
	// change than OpenRouter retiring every free model; keep the previous list.
	if len(models) == 0 {
		return nil, fmt.Errorf("catalog_no_free_models")
	}
	return models, nil
}

func parseCatalog(body []byte) ([]string, error) {
	var envelope struct {
		Data []struct {
			ID           string                     `json:"id"`
			Pricing      map[string]json.RawMessage `json:"pricing"`
			Architecture *struct {
				Output []string `json:"output_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || len(envelope.Data) == 0 {
		return nil, fmt.Errorf("catalog_malformed")
	}
	seen := map[string]bool{}
	out := []string{}
	for _, m := range envelope.Data {
		id := strings.TrimSpace(m.ID)
		if id == "" || seen[id] || !isFreeModel(id, m.Pricing) || !textOutput(m.Architecture) {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	sort.Strings(out)
	return out, nil
}

func textOutput(arch *struct {
	Output []string `json:"output_modalities"`
}) bool {
	if arch == nil || len(arch.Output) == 0 {
		return true
	}
	for _, o := range arch.Output {
		if strings.EqualFold(o, "text") {
			return true
		}
	}
	return false
}

// fetchDailyLimit uses the reported free-model ceiling, defaulting to the
// conservative lowest allowance when the field is absent or invalid.
func fetchDailyLimit(ctx context.Context, client providermodule.HTTPDoer, apiKey string) (int64, error) {
	body, status, err := doJSON(ctx, client, http.MethodGet, keyURL, apiKey, nil)
	if err != nil {
		return 0, fmt.Errorf("key_%w", err)
	}
	if status != http.StatusOK {
		return 0, fmt.Errorf("key_http_%d", status)
	}
	var envelope struct {
		Data *struct {
			FreeModelDailyRequests *struct {
				Limit *int64 `json:"limit"`
			} `json:"free_model_daily_requests"`
		} `json:"data"`
	}
	if json.Unmarshal(body, &envelope) != nil || envelope.Data == nil {
		return 0, fmt.Errorf("key_malformed")
	}
	if f := envelope.Data.FreeModelDailyRequests; f != nil && f.Limit != nil && *f.Limit > 0 && *f.Limit <= 1000 {
		return *f.Limit, nil
	}
	return lowestDailyLimit, nil
}
