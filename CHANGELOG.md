# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/), and this project follows [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

## [0.5.0] - 2026-10-23

### Added

- `providers.synthetic`: a synthetic.new provider module. It manages a paid provider (default `synthetic`) and an infinite-mode provider (default `synthetic-infinite`, the Big plan's free `infinite:small:vision` alias) that share one API key. New requests stay on the paid credential while the plan's weekly credits are under `threshold-percent` (default 95) used and move to infinite mode for aliases both providers serve once the plan is at the threshold, exhausted (429/402), or at `max-concurrent`. Paid-only models are rejected with a retryable HTTP 429 `synthetic_no_capacity` instead of spilling into usage-based billing. A paid credential seen at runtime but absent from the last reconfigure is bound to its provider's account when there is exactly one; one that cannot be bound (two or more accounts, or none) is never admitted, because its spend cannot be checked against a weekly limit, and shows as `unbound`. Quota comes from the free `GET /v2/quotas` endpoint (never polled with placeholder keys) and the `x-synthetic-quotas` response header, projected forward through the documented 2%-per-202-minutes regeneration. Status reports per-lane health, projected weekly usage, in-flight counts, routing counters and measured output tokens per second and time to first token for paid versus infinite mode. The Subscription Quota page shows synthetic.new lanes.
- The coordinator dispatches usage for auth IDs no module owns to the single module that claims it through the new optional `providermodule.UsageRecognizer`.
- The plugin advertises `scheduler_across_priorities` so CLIProxyAPI v8 hosts offer lower-priority fallback credentials to the scheduler. Unrecognized picks still return `Handled:false` and keep the host's priority order.

### Changed

- The repository and Go module are renamed from `cpa-plugin-zai-coding-plan` to `cpa-plugin-subscription-pool` (the plugin ID was already `subscription-pool`). GitHub redirects the old repository, git and release URLs.

### Fixed

- A host config without any Z.ai credential no longer fails the whole plugin with `provider zai: no complete Z.ai account pairs found`. The zai module is inactive in that case, so the other provider modules register and schedule. A partial or malformed Z.ai configuration still fails closed, and so does a `providers.zai.accounts` override with no Z.ai credential in the host config (`accounts[0]: key-suffix matches no account`, the same error an unmatched override gets when credentials exist); remove the override when the Z.ai plan is gone.
- An OpenCode Go module with `enabled: false` or no accounts no longer claims OpenCode Go candidates. Before, `Recognize` still claimed them and `Pick` then failed every OpenCode Go request with `opencode_go_unmanaged_candidate`.

- Deployment verification accepts in-process secret intake and the snapshot service's own feed envelope: `deploy/verify-live.sh`, `deploy/verify-live-opencodego.sh`, and `deploy/rollback.sh` take `CLIPROXY_MANAGEMENT_KEY` / `ZAI_CODING_PLAN_KEY` / `OPENCODE_GO_DASHBOARD_API_KEY` from the environment (sourced in-process from the established credential file, never a new file on disk) as an alternative to 0600 key files, with identical one-line/UTF-8 validation and unchanged confidential-value scans. In snapshot-only mode the feed check accepts the existing `{observedAt, records, schemaVersion, staleAfterSeconds}` envelope (records only need to be non-empty objects) while collector-written feeds keep the strict shape checks; the Go `credential_bound` agreement applies to collector feeds only. The runbook no longer extracts keys into temp files, references no absent key paths, and parameterizes the config ownership it verifies (current host `1000:1000`) instead of asserting `root:root`.
- Deployment verification now accepts the v0.4.5 status shape: `collector-zai.py` and `deploy/verify-live.sh` validate the per-account `identity` pseudonym (64 lowercase hex, unique, confidential-value scanned) and the `cooldown` object (closed reason vocabulary, exact source, active/inactive consistency) from `docs/STATUS-CONTRACT.md`, plus the optional `five_hour_error` diagnostic. Previously both scripts rejected live v0.4.5 status with `secret-like field is forbidden: status.accounts[0].identity` / `unexpected fields`, so no published script could complete acceptance after the v0.4.5 install.
- `deploy/verify-live.sh` and `deploy/verify-live-opencodego.sh` are Docker-native: log evidence comes from `docker logs` of `CLIPROXY_CONTAINER` (or an explicit `CLIPROXY_LOG_COMMAND`) instead of the retired `cliproxy.service` journal; the retired model-usage dashboard fetch runs only when `CLIPROXY_DASHBOARD_URL` is explicitly set; `CLIPROXY_SNAPSHOT_ONLY=1` reads the usage-snapshot service's existing feed files with a freshness bound instead of invoking collectors against live feeds. The container name is validated before any credentialed request.
- `deploy/verify-live-opencodego.sh` distinguishes an unprovisioned Go lane: with `OPENCODE_GO_ALLOW_UNBOUND=1` it records `UNAVAILABLE (no bound dashboard credential; unchanged, not a v0.4.5 regression)` after the same shape and confidential-value scans, and requires the snapshot to agree (`credential_bound: false`). The strict default still mandates a bound credential with usable capacity.
- `deploy/rollback.sh` restores the current-host Docker layout by default (`/home/ubuntu/stacks/cliproxy/config.yaml`, `docker restart`), verifies declared `CLIPROXY_CONFIG_UID`/`CLIPROXY_CONFIG_GID` ownership without ever changing it, and bounds the management DELETE response the same way the runbook does.

### Changed

- CLIProxyAPI SDK `v7.2.67` -> `v7.2.154` and approved baseline host `v7.2.67` -> `v7.2.150` (TOG-7425): the deferred bump re-fires on plugin-relevant patches in the unevaluated v7.2.152-159 window (`00c63a56` pluginhost HTTP wire profile, session-affinity/interceptor/quota plugin capabilities, schema 5->6 at v7.2.155) with SDK `SchemaVersion` 1 -> 5. ABI stays 1; v7.2.67 hosts accept only schema 1 and can no longer load this plugin. Dependabot now suppresses schema-6 patches (7.2.155-159) until the baseline moves past schema 5.

## [0.4.5] - 2026-10-02

### Fixed

- `providers.zai`: a single Z.ai request-rate 429 (business code `1302` "Rate limit reached for requests", or `1305` "temporarily overloaded") no longer collapses the whole Z.ai lane. Before, it put the account into the 10-minute conservative cooldown. On a one-account pool the next pick then returned the `zai_no_capacity` 500, and the model router quarantined the lane for 15 minutes on that message. Now the 429 throttles only that account with a bounded backoff (5s doubling to 60s, never above `fallback-cooldown`, or a bounded hint of at most 60s), and CPA retries on a healthy sibling. When every account is only throttled, pick returns the retryable `zai_rate_limited` error with `http_status: 429` instead of a 500. A sixth consecutive step, any other 429 code, a long reset hint, or a spent allowance keeps the existing conservative cooldown, and `zai_no_capacity` remains the answer once every account is hard-impaired. Throttles show as `health: "throttled"` and are not exported as a status-contract `cooldown`. (TOG-12741)

### Added

- Z.ai authenticated management status now exports the existing account pseudonym and a redacted transient-429 cooldown projection through the actual status DTO. The consumer contract defines explicit inactive state, generation freshness, closed reason provenance, and identity-only joins. OpenCode Go quota resets remain unsupported as cooldown evidence; no collector, routing, or deployment changes are included.

## [0.4.4] - 2026-09-14

### Fixed

- `providers.zai`: `parseQuotaResponse` was still all-or-nothing on the two CREDIT_LIMIT windows — any unparseable five-hour window (invalid usage, currentValue, remaining, nextResetTime, duplicate, or a credits-out-of-range value) discarded the whole response, including a perfectly valid weekly window, and fell back to "estimate". The five-hour and weekly windows now parse independently; the weekly window remains governing (a weekly failure still falls back to estimate, unchanged), but a five-hour failure is recorded as a named diagnostic (`five_hour_error` in the status response) instead of voiding the response. A window that fails to parse is left absent, never zero-filled, so it cannot be misread as "unused, full capacity available". (TOG-2497, completing TOG-2490 fix item 2)

## [0.4.3] - 2026-09-14

### Fixed

- `providers.zai`: the quota poller rejected a CREDIT_LIMIT window with `nextResetTime: null` — which Z.ai sends on a window with zero usage and no reset scheduled (e.g. right after a rollover with no traffic since) — and discarded the entire response, including the unrelated, valid weekly window. `nextResetTime: null` on an unused (`currentValue == 0`) window now parses as utilization 0 with no pending reset; a consumed window still requires a valid reset time. (TOG-2490)

## [0.4.2] - 2026-09-14

### Changed

- The `/v0/resource/plugins/subscription-pool/status` page (Management Center's "Plugins" nav entry) is now a real per-account dashboard instead of a static placeholder: it renders both `zai` and `opencode-go` accounts server-side from the coordinator's own `Status()` aggregation, with per-window utilization bar, percent, absolute UTC reset timestamp, countdown, source (quota_api/estimate for Z.ai; authoritative/429-derived/unknown for OpenCode Go), and health. No management or dashboard key is ever embedded in the page. Resource-page ownership moved from the `zai` module to the coordinator, since only the coordinator can see both providers; the menu entry is renamed from "Z.ai Quota" to "Subscription Quota" and its description now mentions both providers. (TOG-2477)

## [0.4.1] - 2026-09-13

### Fixed

- `providers.opencode-go`: the dashboard usage poller fetched `/zen/go/v1/usage` once with a single module-level key and broadcast that one snapshot to every configured account, so all accounts reported identical utilization/reset regardless of their real usage. `dashboard-api-key` now lives on `accounts[]` (falling back to the module-level key only when an account omits its own), and each account is polled independently with its own key. (TOG-2472)
- `providers.zai`: the quota poller rejected upstream CREDIT_LIMIT/`nextResetTime` values serialized with a trailing `.0` (e.g. `1789347583607.0`), falling back to a stuck-at-0% estimate. `strictInt64` now also accepts an exact, finite, in-range whole-number float. (TOG-2473)

## [0.4.0] - 2026-09-13

### Added

- Operator bundle now packages the OpenCode Go telemetry path alongside the existing Z.ai lane: `collector-opencodego.py` and `deploy/verify-live-opencodego.sh` are installed into the release bundle, checksummed, and enforced by strict bundle-manifest validation.
- Release workflow artifact staging and the published GitHub release now include both provider collectors and both live verifiers, so the installed bundle proves both `zai` and `opencode-go` provider modules are present and their telemetry files are valid.

### Changed

- Versioned release metadata (`Makefile`, `registry.json`, CI/package expectations, deploy runbook text) bumped to `0.4.0` to reflect the dual-provider `subscription-pool` coordinator baseline merged in `0.3.0`.

## [0.3.0] - 2026-09-13

### Added

- Neutral `subscription-pool` coordinator plugin ID replacing the single-provider `zai-coding-plan` registration; Z.ai support is now a `providerModule` extraction (`internal/providers/zai`) behind a coordinator-owned dispatch registry.
- `providerModule` interface and per-`authID`/`providerID`/`logicalAccountID` dispatch registry: zero recognized families yields `Handled:false`, more than one recognized family or a recognized-but-unmanaged/all-impaired family fails closed instead of silently passing traffic through.
- Config YAML reshaped to a `providers.<name>` root shape (`providers.zai`, with `providers.opencode-go` reserved for the sibling module) with strict unknown-field rejection.

### Changed

- Z.ai account/quota/health/polling/management behavior is unchanged; existing tests were relocated to `internal/providers/zai` and continue to pass without modification.

## [0.2.0] - 2026-09-11

### Added

- v0.2.0 release packaging and strict archive/checksum validation for the Linux/amd64 shared library and plugin-store archive.
- Deployed, latest, and CLIProxyAPI v7.2.67 baseline compatibility evidence in the release gate.
- Credential-free live verification, bounded canary guidance, and recoverable rollback procedures for the Z.ai lane.

### Security

- Live verification rejects malformed or confidential management responses, unsafe key-file permissions, missing managed capacity, and unbounded service output.
- Rollback validates the backup and safe configuration directory before management-plane mutation and atomically restores the configuration.

## [0.1.0] - 2026-09-10

### Added

- Exact-key pairing of Z.AI Anthropic and OpenAI-compatible CLIProxyAPI credentials into one logical account.
- Authoritative five-hour and weekly quota polling, including reset timestamps and off-peak status, with fixed-point token accounting as a degraded fallback.
- Shared account health and quota-aware scheduling across both credential protocols.
- Authenticated management status, refresh, unblock, and non-secret account-configuration operations.
- Atomic redacted state storage under `<auth-dir>/zai-coding-plan/` with restrictive permissions.
- Native CLIProxyAPI v7.2.x C ABI registration for `scheduler`, `usage_plugin`, and `management_api` capabilities.
- Pinned-host integration tests covering registration, management dispatch, quota-aware scheduling, invalid reconfiguration, and all-impaired failure propagation.
- Versioned Linux/amd64 shared-library and plugin-store archive packaging with SHA-256 checksums.
- Continuous integration, exact-image compatibility gate, release consistency checks, security checks, and community documentation.

### Security

- Management routes rely on CLIProxyAPI management-key authentication.
- Raw plan keys, authorization headers, request bodies, and management credentials are excluded from persistent state, logs, and status responses; persistence uses only derived account identities and redacted state.
- State and settings commits are atomic, redacted, and recovered fail-closed after interrupted writes.

[Unreleased]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/compare/v0.5.0...HEAD
[0.5.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.5.0
[0.4.5]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.5
[0.4.4]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.4
[0.4.3]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.3
[0.4.2]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.2
[0.4.1]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.1
[0.4.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.4.0
[0.3.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.3.0
[0.2.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.2.0
[0.1.0]: https://github.com/TogetherWeOwn/cpa-plugin-subscription-pool/releases/tag/v0.1.0
