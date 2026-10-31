// Package providermodule defines the boundary between the subscription-pool
// coordinator and each provider it hosts. The coordinator owns lifecycle,
// the scheduler slot, management routes, secure storage placement, and
// dispatch; each Module owns its own accounts, quota, pollers, health,
// cursors, settings, and status.
package providermodule

import (
	"context"
	"encoding/json"
	"net/http"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

// Clock abstracts time for deterministic module tests.
type Clock interface {
	Now() time.Time
	Sleep(context.Context, time.Duration) error
}

// HTTPDoer abstracts outbound HTTP for deterministic module tests.
type HTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

// HostConfig is what the coordinator hands a module on every Reconfigure.
type HostConfig struct {
	// CPAConfigPath is the resolved absolute path to the host CLIProxyAPI
	// config file the coordinator loaded providerConfig from.
	CPAConfigPath string
	// AuthDir is already namespaced to the coordinator's own plugin id
	// (i.e. "<auth-dir>/subscription-pool"); a module appends only its own
	// "providers/<id>" segment before opening secure storage.
	AuthDir string
	// ClaudeKeys and OpenAICompatibility are the relevant projections of the
	// host CPA config a module needs to discover its own accounts.
	ClaudeKeys          []ClaudeKey
	OpenAICompatibility []OpenAICompatibility
	Clock               Clock
	HTTPClient          HTTPDoer
}

// ClaudeKey mirrors the host's claude-api-key config entry shape.
type ClaudeKey struct {
	APIKey   string
	BaseURL  string
	ProxyURL string
	Prefix   string
	Headers  map[string]string
}

// OpenAICompatibility mirrors the host's openai-compatibility config entry shape.
type OpenAICompatibility struct {
	Name          string
	BaseURL       string
	Disabled      bool
	Headers       map[string]string
	APIKeyEntries []OpenAICompatibilityAPIKey
	Models        []OpenAICompatibilityModel
}

// OpenAICompatibilityModel binds the host alias to the actual upstream model.
// Cost guards must price Name, never trust the spelling of Alias.
type OpenAICompatibilityModel struct {
	Name  string
	Alias string
}

// OpenAICompatibilityAPIKey mirrors one openai-compatibility api-key-entries item.
type OpenAICompatibilityAPIKey struct {
	APIKey   string
	ProxyURL string
}

// ManagementRoute is one HTTP route a module answers. Path is the exact,
// fully-qualified path the module wants the host to dispatch to it (today
// this is each module's own pre-coordinator absolute path, e.g.
// "/v0/management/plugins/zai-coding-plan/status" — the coordinator does no
// path rewriting in this slice; renaming module route prefixes to the
// coordinator's own PluginID is deferred to the plugin-ID rename task).
type ManagementRoute struct {
	Method      string
	Path        string
	Description string
}

// ManagementResource is one GET-only, unauthenticated resource page a module
// answers, addressed the same way as ManagementRoute.Path.
type ManagementResource struct {
	Path        string
	Menu        string
	Description string
}

// ManagementRoutes is a module's full set of routes and resources.
type ManagementRoutes struct {
	Routes    []ManagementRoute
	Resources []ManagementResource
}

// ManagementCapable is optionally implemented by a Module that wants to
// expose its own management/resource routes. The coordinator discovers this
// via a type assertion, so a module that manages nothing here (e.g. a future
// provider with no operator-facing controls) simply doesn't implement it.
type ManagementCapable interface {
	// ManagementRoutes reports every route/resource this module answers.
	ManagementRoutes(ctx context.Context) ManagementRoutes
	// HandleManagement answers one request the coordinator has determined
	// (by matching req against a route this module previously declared)
	// belongs to this module.
	HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse
}

// UsageRecognizer is optionally implemented by a Module that can claim a
// usage record whose auth ID is absent from the coordinator's dispatch
// registry, for example a credential added or re-keyed after the last
// Reconfigure, or a host whose auth-ID derivation differs from the module's
// own. The coordinator only consults it when no module owns record.AuthID,
// and dispatches only when exactly one module claims the record.
type UsageRecognizer interface {
	RecognizeUsage(record pluginapi.UsageRecord) bool
}

// ConfigValidator is optionally implemented by a Module that can check a
// configuration without applying it. The coordinator stages every module's
// new configuration through ValidateConfig before it calls Reconfigure on
// any of them, so a configuration one module rejects never leaves the
// modules before it running on the new configuration. ValidateConfig must
// not open storage or contact the network, and must not change module state
// beyond recording a rejection for the module's status the way Reconfigure
// would.
type ConfigValidator interface {
	ValidateConfig(host HostConfig, providerConfig json.RawMessage) error
}

// HostCredentialOwner is optionally implemented by a Module whose auth IDs
// follow from the host config alone. While a configuration is rejected the
// coordinator fails picks closed for these IDs too, so a provider the module
// would guard stays guarded even before any configuration was accepted.
// HostAuthIDs must not depend on the provider config, change module state,
// or fail: it returns every auth ID the host config could give the provider.
type HostCredentialOwner interface {
	HostAuthIDs(host HostConfig) []string
}

// UsageAttributor is optionally implemented by a Module that can name the
// account a usage record it accepted belongs to. The coordinator calls it
// after HandleUsage, for its usage ledger only. The returned name must be a
// display name (never a key, key fragment or auth ID); "" means unknown.
type UsageAttributor interface {
	UsageAccount(record pluginapi.UsageRecord) string
}

// WireError is the coded error shape carried across the CGo ABI boundary.
// Both provider modules and the coordinator produce errors satisfying Coded
// so the ABI glue in src/ can build byte-identical error envelopes
// regardless of which package raised the error.
type WireError struct {
	Code       string
	Message    string
	Retryable  bool
	HTTPStatus int
}

// Coded is implemented by errors that carry a stable wire code.
type Coded interface {
	error
	Coded() WireError
}

// Module is implemented by each provider hosted behind the subscription-pool
// coordinator. All methods must be safe for concurrent use.
type Module interface {
	// ID returns the module's stable provider identifier, e.g. "zai".
	ID() string
	// Recognize reports whether candidate belongs to this module's family.
	// The coordinator calls Recognize on every module before calling Pick on
	// any of them, so recognition must never depend on module-internal state
	// that Pick itself would mutate.
	Recognize(candidate pluginapi.SchedulerAuthCandidate) bool
	// Reconfigure applies host and provider-specific configuration. It must
	// be atomic: on error, the module's previously active configuration (if
	// any) remains in effect.
	Reconfigure(ctx context.Context, host HostConfig, providerConfig json.RawMessage) error
	// OwnedAuthIDs returns every auth ID this module currently manages, for
	// the coordinator's dispatch registry. It must reflect exactly the
	// module's live configuration after the most recent successful
	// Reconfigure.
	OwnedAuthIDs(ctx context.Context) []string
	// Pick returns a scheduling decision for req. The coordinator only calls
	// Pick after establishing, via Recognize, that this module (and no other)
	// owns every candidate in req.
	Pick(ctx context.Context, req pluginapi.SchedulerPickRequest) (pluginapi.SchedulerPickResponse, error)
	// HandleUsage records usage against the account owning record.AuthID.
	// The coordinator only calls this for auth IDs the dispatch registry
	// maps to this module.
	HandleUsage(ctx context.Context, record pluginapi.UsageRecord) error
	// Status returns this module's own management-status JSON payload.
	Status(ctx context.Context) (json.RawMessage, error)
	// Close releases resources and flushes durable state.
	Close(ctx context.Context) error
}
