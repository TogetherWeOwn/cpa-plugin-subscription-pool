package main

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/coordinator"
	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const pluginID = coordinator.PluginID
const pluginDisplayName = "Subscription Pool: Z.ai · OpenCode Go · synthetic.new"

// The registration carries its own logo: no remote fetch or management key is
// needed when the official panel renders it.
//
//go:embed logo.svg
var pluginLogo []byte

// pluginVersion is stamped at build time with -ldflags -X main.pluginVersion.
// It is mirrored into coordinator.PluginVersion so the registered plugin
// metadata (pluginRegistration) and the coordinator's own aggregated
// management status body never disagree on version.
var pluginVersion = "0.0.0-dev"

func init() {
	coordinator.PluginVersion = pluginVersion
}

type envelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *envelopeError  `json:"error,omitempty"`
}

type envelopeError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	Retryable  bool   `json:"retryable,omitempty"`
	HTTPStatus int    `json:"http_status,omitempty"`
}

func (e *envelopeError) Error() string { return e.Code + ": " + e.Message }

// Coded implements providermodule.Coded, the shared wire-error contract also
// implemented by internal/providers/zai's own envelopeError, so an error
// raised inside a provider module and propagated verbatim by the coordinator
// still carries its code/message/HTTP status here.
func (e *envelopeError) Coded() providermodule.WireError {
	return providermodule.WireError{Code: e.Code, Message: e.Message, Retryable: e.Retryable, HTTPStatus: e.HTTPStatus}
}

type registration struct {
	SchemaVersion uint32             `json:"schema_version"`
	Metadata      pluginapi.Metadata `json:"metadata"`
	Capabilities  capabilities       `json:"capabilities"`
}

// capabilities mirrors the rpcCapabilities wire schema of CLIProxyAPI's
// internal/pluginhost. Every true field must be backed by a handler in
// pluginCall: the host rejects registrations that advertise no capability
// (internal/pluginhost/host.go validPlugin) and warns on every advertised
// method that fails to answer.
type capabilities struct {
	Scheduler bool `json:"scheduler"`
	// SchedulerAcrossPriorities asks hosts that support it (CLIProxyAPI v8)
	// to offer candidates from every priority tier instead of only the
	// highest available one. The synthetic module needs this to move a
	// request from the paid credential (priority 0) to the lower-priority
	// infinite-mode credential before the paid plan is exhausted. Picks no
	// module recognizes still return Handled:false, and the host's own
	// selector then keeps its normal priority order. Older hosts ignore it.
	SchedulerAcrossPriorities bool `json:"scheduler_across_priorities,omitempty"`
	UsagePlugin               bool `json:"usage_plugin"`
	ManagementAPI             bool `json:"management_api"`
}

func pluginRegistration() registration {
	return registration{
		SchemaVersion: pluginabi.SchemaVersion,
		Metadata: pluginapi.Metadata{
			Name:             pluginDisplayName,
			Version:          pluginVersion,
			Author:           "TogetherWeOwn",
			GitHubRepository: "https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool",
			Logo:             "data:image/svg+xml;base64," + base64.StdEncoding.EncodeToString(pluginLogo),
		},
		// Advertised per docs/ARCHITECTURE.md: quota-aware scheduler,
		// usage accounting feed, and management status endpoints. The
		// scheduler declines every pick until quota state exists, which
		// keeps the host's native scheduler in control.
		Capabilities: capabilities{
			Scheduler:                 true,
			SchedulerAcrossPriorities: true,
			UsagePlugin:               true,
			ManagementAPI:             true,
		},
	}
}

// schedulerPick explicitly delegates healthy traffic to CPA's built-in
// round-robin scheduler and takes over only while managed accounts are
// impaired. A hard scheduler error prevents fallback to known-bad capacity.
func schedulerPick(request []byte) ([]byte, error) {
	var pick pluginapi.SchedulerPickRequest
	if err := json.Unmarshal(request, &pick); err != nil {
		return nil, fmt.Errorf("decode scheduler request")
	}
	response, err := runtimeState.Pick(pick)
	if err != nil {
		return nil, err
	}
	return okEnvelope(response)
}

// usageHandle consumes a lossy best-effort usage observation. Persistence
// failures are surfaced in estimator integrity state, but the response remains
// successful because CPA discards usage-plugin RPC errors.
func usageHandle(request []byte) ([]byte, error) {
	var record pluginapi.UsageRecord
	if err := json.Unmarshal(request, &record); err != nil {
		return nil, fmt.Errorf("decode usage record")
	}
	_ = runtimeState.HandleUsage(record)
	return okEnvelope(struct{}{})
}

// managementHandle answers a management.handle RPC.
func managementHandle(request []byte) ([]byte, error) {
	var req pluginapi.ManagementRequest
	if err := json.Unmarshal(request, &req); err != nil {
		return nil, fmt.Errorf("decode management request")
	}
	response := runtimeState.HandleManagement(context.Background(), req)
	return okEnvelope(response)
}

func okEnvelope(value any) ([]byte, error) {
	raw, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{OK: true, Result: raw})
}

func errorEnvelopeFor(err error) []byte {
	if typed, ok := err.(providermodule.Coded); ok {
		wire := typed.Coded()
		raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: wire.Code, Message: wire.Message, Retryable: wire.Retryable, HTTPStatus: wire.HTTPStatus}})
		return raw
	}
	return errorEnvelope("plugin_error", err.Error())
}

func errorEnvelope(code, message string) []byte {
	raw, _ := json.Marshal(envelope{OK: false, Error: &envelopeError{Code: code, Message: message}})
	return raw
}
