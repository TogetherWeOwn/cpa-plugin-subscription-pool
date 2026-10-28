package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"time"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const (
	analyticsURL         = "https://api.cloudflare.com/client/v4/graphql"
	maxAnalyticsResponse = 1 << 20
)

// No model/key dimensions or filters: the daily allowance belongs to the
// account, not an API token. One aggregate has no grouping/pagination loss.
// Authentication: https://developers.cloudflare.com/analytics/graphql-api/getting-started/authentication/api-token-auth/
// Schema must be verified with the deployment's scoped token before activation;
// schema/permission/partial-data errors never turn into a zero-usage balance.
const neuronQuery = `query WorkersAINeurons($account: String!, $date: Date!) {
 viewer { accounts(filter: {accountTag: $account}) {
  accountTag
  aiInferenceAdaptiveGroups(limit: 1, filter: {date_geq: $date, date_leq: $date}) {
   sum { totalNeurons }
  }
 } }
}`

func newAnalyticsClient() *http.Client {
	return &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func fetchAnalytics(ctx context.Context, client providermodule.HTTPDoer, account, token string, now time.Time) (float64, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	payload, _ := json.Marshal(map[string]any{"query": neuronQuery, "variables": map[string]string{"account": account, "date": utcDay(now).Format("2006-01-02")}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, analyticsURL, bytes.NewReader(payload))
	if err != nil {
		return 0, fmt.Errorf("analytics_request_failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	res, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("analytics_transport_failed")
	}
	if res == nil || res.Body == nil {
		return 0, fmt.Errorf("analytics_empty_response")
	}
	// Read/parse errors are handled below. Closing an already-read response
	// has no quota semantics.
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("analytics_http_%d", res.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(res.Body, maxAnalyticsResponse+1))
	if err != nil || len(body) > maxAnalyticsResponse {
		return 0, fmt.Errorf("analytics_response_unreadable")
	}
	return parseAnalytics(body, account)
}

func parseAnalytics(raw []byte, expectedAccount string) (float64, error) {
	var envelope struct {
		Errors []json.RawMessage `json:"errors"`
		Data   *struct {
			Viewer *struct {
				Accounts []struct {
					AccountTag string `json:"accountTag"`
					Groups     *[]struct {
						Sum *struct {
							Neurons *float64 `json:"totalNeurons"`
						} `json:"sum"`
					} `json:"aiInferenceAdaptiveGroups"`
				} `json:"accounts"`
			} `json:"viewer"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &envelope) != nil {
		return 0, fmt.Errorf("analytics_invalid_json")
	}
	if len(envelope.Errors) > 0 {
		return 0, fmt.Errorf("analytics_graphql_error")
	}
	if envelope.Data == nil || envelope.Data.Viewer == nil || len(envelope.Data.Viewer.Accounts) != 1 {
		return 0, fmt.Errorf("analytics_account_missing")
	}
	account := envelope.Data.Viewer.Accounts[0]
	if account.AccountTag != expectedAccount || account.Groups == nil {
		return 0, fmt.Errorf("analytics_dataset_missing")
	}
	groups := *account.Groups
	if len(groups) == 0 {
		return 0, nil
	}
	if len(groups) != 1 || groups[0].Sum == nil || groups[0].Sum.Neurons == nil {
		return 0, fmt.Errorf("analytics_neurons_missing")
	}
	neurons := *groups[0].Sum.Neurons
	if math.IsNaN(neurons) || math.IsInf(neurons, 0) || neurons < 0 {
		return 0, fmt.Errorf("analytics_invalid_neurons")
	}
	return neurons, nil
}
