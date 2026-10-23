# Architecture

## Purpose

`subscription-pool` (repository `cpa-plugin-subscription-pool`, formerly `cpa-plugin-zai-coding-plan`) is a native CLIProxyAPI (CPA) plugin. It is a neutral coordinator that hosts one or more provider modules behind a single CPA scheduler/usage-plugin slot; today it hosts three: `zai`, `opencode-go`, and `synthetic`. The `zai` module makes a pool of Z.ai GLM Coding Plan subscriptions behave as quota-aware logical accounts across CPA's Anthropic and OpenAI-compatible lanes; `opencode-go` pools OpenCode Go accounts; `synthetic` schedules synthetic.new paid credits with fallback to the free infinite mode.

Version 0.1 (zai module):

1. discover and pair the two CPA credentials backed by each plan key;
2. poll Z.AI's plan-quota endpoint for authoritative five-hour and weekly utilization, with local credit estimation as a degraded fallback;
3. remain CPA's sole active scheduler plugin, delegate to CPA's native scheduler while every account is healthy, and exclude an entire paired account when it is exhausted, suspended, disabled, or invalid; and
4. expose authenticated management status and recovery operations.

The plugin does not proxy inference, rewrite requests, or mint credentials. The zai module's primary quota signal is `GET https://api.z.ai/api/monitor/usage/quota/limit`, queried per account with that account's plan key. Local accounting is a conservative fallback when the endpoint fails, and an observed upstream `429` remains authoritative.

## Exact CLIProxyAPI plugin contract

The baseline is CLIProxyAPI v7.2.150, using the SDK's native c-shared/dlopen plugin pattern:

```go
require github.com/router-for-me/CLIProxyAPI/v7 v7.2.154
```

```go
import (
    "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
    "github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)
```

The deployed `eceasy/cli-proxy-api` v7.2.x image is a fork, so a version label alone is not proof of compatibility. Release requires the compatibility smoke against the exact operator-reported linux/amd64 manifest in `deploy/deployed-host-image.json` and the latest Docker Hub tag resolved to an immutable manifest at CI time; `.github/host-images.json` retains the schema-5-capable v7.2.150 baseline (moved from v7.2.67 under TOG-7425; a v7.2.67 host accepts only plugin schema 1 and cannot load this plugin).

### C ABI

On Linux, the v7.2.150 host uses `dlopen(path, RTLD_NOW | RTLD_LOCAL)`, resolves exactly `cliproxy_plugin_init` with `dlsym`, and calls it using this ABI:

```c
typedef struct {
    void* ptr;
    size_t len;
} cliproxy_buffer;

typedef int (*cliproxy_host_call_fn)(
    void* host_ctx,
    const char* method,
    const uint8_t* request,
    size_t request_len,
    cliproxy_buffer* response
);
typedef void (*cliproxy_host_free_fn)(void* ptr, size_t len);

typedef struct {
    uint32_t abi_version;
    void* host_ctx;
    cliproxy_host_call_fn call;
    cliproxy_host_free_fn free_buffer;
} cliproxy_host_api;

typedef int (*cliproxy_plugin_call_fn)(
    const char* method,
    const uint8_t* request,
    size_t request_len,
    cliproxy_buffer* response
);
typedef void (*cliproxy_plugin_free_fn)(void* ptr, size_t len);
typedef void (*cliproxy_plugin_shutdown_fn)(void);

typedef struct {
    uint32_t abi_version;
    cliproxy_plugin_call_fn call;
    cliproxy_plugin_free_fn free_buffer;
    cliproxy_plugin_shutdown_fn shutdown;
} cliproxy_plugin_api;

typedef int (*cliproxy_plugin_init_fn)(
    const cliproxy_host_api* host,
    cliproxy_plugin_api* plugin
);
```

The `.so` exports:

```c
int cliproxy_plugin_init(
    const cliproxy_host_api* host,
    cliproxy_plugin_api* plugin
);
```

Initialization returns `0`, stores the host callback table, and fills all plugin callback fields. Both sides require `abi_version == pluginabi.ABIVersion`, which is `1` from v7.2.67 through v7.2.159. Registration uses `pluginabi.SchemaVersion`, which is `5` in the v7.2.154 SDK against the v7.2.150 baseline host. A buffer is freed by the side that allocated it, through that side's `free_buffer` callback.

Build with Go 1.26, `CGO_ENABLED=1`, Linux/amd64, and `-buildmode=c-shared`.

### RPC methods

Calls use a method string, JSON request bytes, and a v7.2 envelope:

```json
{"ok":true,"result":{}}
```

or:

```json
{"ok":false,"error":{"code":"plugin_error","message":"..."}}
```

Version 0.1 implements:

| Method | SDK constant | Purpose |
|---|---|---|
| `plugin.register` | `pluginabi.MethodPluginRegister` | Parse configuration, discover accounts, load state, return metadata/capabilities. |
| `plugin.reconfigure` | `pluginabi.MethodPluginReconfigure` | Atomically rebuild configuration and account indexes after CPA reload. |
| `scheduler.pick` | `pluginabi.MethodSchedulerPick` | Explicitly delegate healthy traffic to built-in round-robin, select healthy candidates during degradation, and hard-fail when managed capacity is exhausted. |
| `usage.handle` | `pluginabi.MethodUsageHandle` | Consume usage/failure records, update credit windows, classify 401/403/429. |
| `management.register` | `pluginabi.MethodManagementRegister` | Register management routes and optional non-sensitive resource shell. |
| `management.handle` | `pluginabi.MethodManagementHandle` | Serve status, refresh, unblock, and account configuration. |

The plugin uses `pluginabi.MethodHostLog` only for structured redacted logs. A background quota client performs bounded HTTPS GETs to Z.AI's documented monitor endpoint outside scheduler critical sections. It authenticates with the paired plan key, redacts all request/response diagnostics, applies timeouts and response-size limits, and jitter-polls each account every one to three minutes.

Registration advertises schema version 1 and the `scheduler`, `usage_plugin`, and `management_api` capabilities.

## Module layout

The coordinator/provider split gives each provider module a compiler-enforced privacy boundary: Go's `c-shared` build mode requires every file in `src/` to stay `package main` (only files there may `import "C"` / `//export`), but any other directory is a normal importable package. `src/` therefore shrinks to pure ABI glue, `internal/coordinator` owns lifecycle/dispatch, and each provider lives in its own `internal/providers/<id>` package that no other package can reach into.

```text
.
├── .github/
│   ├── ISSUE_TEMPLATE/
│   ├── workflows/ci.yml
│   ├── workflows/release.yml
│   ├── workflows/host-compatibility.yml
│   └── pull_request_template.md
├── docs/ARCHITECTURE.md
├── src/
│   ├── plugin.go             # envelope/wire-error types, plugin registration metadata
│   ├── plugin_linux.go       # C ABI init/call/free/shutdown
│   └── runtime_state.go      # var runtimeState = coordinator.New(zai, opencodego, synthetic)
├── internal/
│   ├── providermodule/
│   │   └── interface.go      # Module interface + HostConfig; the coordinator/provider boundary
│   ├── coordinator/
│   │   ├── coordinator.go    # Coordinator: lifecycle, scheduler dispatch, PluginID
│   │   ├── registry.go       # authID -> providerID dispatch registry + invariant 1 enforcement
│   │   ├── config.go         # plugins.configs.subscription-pool.* parsing, providers.<id> projection
│   │   ├── management.go     # aggregated status route + per-module route delegation
│   │   └── manifest.go       # manifest.json + empty provider stub-dir creation
│   ├── providers/zai/
│   │   ├── module.go         # zaiModule: providermodule.Module wrapping the zai runtime
│   │   ├── accounts.go       # exact-key pairing and stable auth IDs
│   │   ├── settings.go       # atomic 0600 persistence (via internal/securestore)
│   │   ├── quota.go          # bounded authoritative quota polling/parsing
│   │   ├── credits.go        # fixed-point fallback credit calculation
│   │   ├── windows.go        # fallback five-hour and weekly ledgers
│   │   ├── failures.go       # 401/403/429 classification/reset hints
│   │   ├── scheduler.go      # degraded-mode healthy selection
│   │   ├── management.go     # zai's own management routes/handlers
│   │   └── runtime.go        # zai runtime state machine
│   ├── providers/opencodego/ # OpenCode Go quota-aware pooling
│   ├── providers/synthetic/
│   │   ├── module.go         # paid/infinite lanes, threshold + concurrency scheduling, usage learning, status
│   │   ├── quota.go          # GET /v2/quotas and x-synthetic-quotas parsing
│   │   └── config.go         # providers.synthetic schema
│   ├── securestore/          # generic 0700/0600, symlink-rejecting, atomic-replace storage
│   └── testfixture/          # shared CPA config fixture builder for tests
├── config.example.yaml
├── registry.json
├── Makefile
├── go.mod
└── go.sum
```

Only `src/` must stay `package main`; every `internal/` package is independently testable without CGO or wall-clock sleeps, and `internal/providers/zai` in particular carries no CGO dependency at all.

## Configuration

### Paired CPA credentials

One Z.ai account appears twice with the exact same key:

```yaml
claude-api-key:
  - api-key: "<same-plan-key>"
    base-url: "https://api.z.ai/api/anthropic"
    prefix: "zai"

openai-compatibility:
  - name: "zai-coding-plan"
    base-url: "https://api.z.ai/api/coding/paas/v4"
    api-key-entries:
      - api-key: "<same-plan-key>"
```

Discovery accepts only this provider name and these normalized base URLs. It pairs by full-key equality in memory and computes CPA's synthesized auth IDs using the v7.2 stable-ID algorithm. The full key remains only in the in-memory account object for quota authentication; it is never persisted, returned, or logged. Never pair by suffix.

A valid logical account has exactly one matching Claude entry and exactly one matching OpenAI-compatible key entry. A recognized Z.ai entry that is unpaired, duplicated on either side, or ambiguously matched makes the whole candidate snapshot invalid. Registration/reconfiguration rejects that snapshot and keeps the last valid snapshot; first registration fails rather than leaving recognized Z.ai credentials unmanaged and eligible for built-in selection.

### Plugin schema

The coordinator owns only `enabled`, `priority`, and `cpa-config-path`; everything else nests under `providers.<id>`, and each provider owns its own sub-schema exclusively. `providers.zai`'s field set is an unchanged lift of the pre-coordinator flat schema:

```yaml
plugins:
  enabled: true
  configs:
    subscription-pool:
      enabled: true
      priority: 1000
      cpa-config-path: /app/config.yaml
      providers:
        zai:
          quota-refresh-interval: 2m
          authoritative-max-age: 5m
          threshold-percent: 97
          suspend-duration: 30m
          fallback-cooldown: 10m
          state-retention: 8d
          default-plan: pro
          accounts:
            - key-suffix: "display-or-override-match"
              name: "zai-pro-1"
              plan: pro
              disabled: false
              five-hour-credits: 12000
              weekly-credits: 60000
        opencode-go:
          threshold-percent: 97
          accounts:
            - name: go-a
              auth-ids: ["managed-auth-id"]
              dashboard-api-key: ${OPENCODE_GO_A_DASHBOARD_KEY}
```

`providers.opencode-go` derives quota state from 429 response bodies (`HandleUsage`) and, when a dashboard key is configured, from an on-demand poll of that account's real usage/reset endpoint (see `internal/providers/opencodego/PROVENANCE.md`, "Usage poller (TOG-2458)"). The dashboard key is `accounts[].dashboard-api-key`, checked per account, falling back to the module-level `dashboard-api-key` only when an account does not set its own -- each account's own key returns only that account's own usage, so every account needing real polling normally needs its own key. Each configured account is polled independently and throttled independently; one account's poll failure never blocks or corrupts another's result. An empty/omitted key (module-level and per-account) is valid configuration: every window then reports `known:false` in `Status()` rather than a guessed reset time. Its schema and module logic were developed from scratch; the poller's endpoint, auth flow, and response shape were studied (never copied) from a third-party opencode-go plugin implementation under an explicit, narrow owner carve-out documented in `PROVENANCE.md` -- the module's code, structure, and identifiers must still never be developed from, or informed by, any read of that plugin's source.

| Field | Default | Rule |
|---|---:|---|
| `priority` | `1000` | Host-level `PluginInstanceConfig` field, not plugin payload. V0.1 requires this plugin to be the only enabled scheduler; the high value makes accidental lower-priority schedulers non-winning but is not a substitute for exclusivity validation. |
| `cpa-config-path` | `config.yaml` | Host config path readable by the plugin. |
| `providers.zai.quota-refresh-interval` | `2m` | Base jittered interval; accept only one to three minutes in v0.1. |
| `providers.zai.authoritative-max-age` | `5m` | Maximum age for scheduling from the last successful quota response before switching to the estimator. Must exceed the maximum jittered poll interval. |
| `providers.zai.threshold-percent` | `97` | Integer 1–100; either window reaching it exhausts the account. |
| `providers.zai.suspend-duration` | `30m` | Positive duration for 401/403. |
| `providers.zai.fallback-cooldown` | `10m` | Conservative 429 block when no trustworthy reset is present. |
| `providers.zai.state-retention` | `8d` | Must exceed one week. |
| `providers.zai.default-plan` | none | Optional `lite`, `pro`, or `max`; otherwise each account declares a plan. |
| `providers.zai.accounts[].key-suffix` | required | Override matcher only; ambiguous matches fail closed. |
| `providers.zai.accounts[].name` | generated | Unique display name. |
| `providers.zai.accounts[].plan` | inherited | `lite`, `pro`, `max`, or `custom`. |
| `providers.zai.accounts[].disabled` | `false` | Excludes both credentials. |
| `providers.zai.accounts[].five-hour-credits` | plan value | Positive custom override. |
| `providers.zai.accounts[].weekly-credits` | plan value | Positive custom override. |

Published plan buckets:

| Plan | Five-hour | Weekly |
|---|---:|---:|
| Lite | 2,000 | 10,000 |
| Pro | 12,000 | 60,000 |
| Max | 28,000 | 140,000 |

Reject the complete initial registration on invalid account cardinality, ambiguous suffixes, duplicate names, invalid durations, or non-positive buckets. An invalid reconfiguration retains the last valid internal snapshot and records a bounded redacted validation error, but returns an `invalid_config` RPC envelope; CPA v7.2.150 consequently removes the plugin from the rebuilt capability snapshot rather than serving a falsely valid registration. The exact-image integration test verifies that valid configuration registers and that invalid live configuration withdraws scheduler capability. An invalid initial registration does not advertise scheduler capability.

## Identity and pairing

Each logical account contains:

- an internal HMAC-SHA256-derived identity, never the key itself;
- a redacted display suffix;
- the `openai-compatibility:zai-coding-plan` auth ID;
- the `claude:apikey` auth ID;
- plan buckets; and
- shared health/window state.

`byAuthID` maps both CPA credentials to the same account. A usage record or failure on either protocol updates one ledger and one health state. Key rotation creates a new identity, so the replacement key does not inherit stale blocks or usage.

The stable auth-ID implementation must reproduce host iteration order and duplicate counters. Contract tests compare it with v7.2.150 fixtures and the deployed eceasy image.

## Quota and fallback credit accounting

### Authoritative quota

For each paired account, poll:

```text
GET https://api.z.ai/api/monitor/usage/quota/limit
Authorization: Bearer <plan key>
```

A successful response contains `CREDIT_LIMIT` records. Map `unit: 3, number: 5` to the five-hour bucket and `unit: 6, number: 1` to the weekly bucket. Treat `currentValue / usage` as utilization, `remaining` as advisory cross-check data, and `nextResetTime` as an epoch-millisecond reset instant. Require exactly one valid record for each bucket; missing or duplicate records make the response unusable. The server-reported plan `level` selects the normal Lite/Pro/Max defaults unless an explicit, validated account override is required.

Quota state records the source (`quota_api` or `estimate`), last successful refresh, next reset times, and any redacted warning. Poll asynchronously every one to three minutes with deterministic jitter per account; coalesce concurrent refreshes, bound connect/total timeouts and response bytes, validate all numeric fields, and retain the last result only through `authoritative-max-age`. Scheduler calls perform no network I/O.

When a poll recovers after estimated mode, replace routing utilization and resets atomically with the authoritative response. Retain the estimator ledger only for future outages; do not add estimated consumption to `currentValue`, and do not let a lower estimate clear an upstream 429 block. Record the source transition and divergence as redacted diagnostics.

The quota client keeps each plan key only in process memory for the active configuration lifetime and drops replaced account snapshots through normal Go object replacement. V0.1 fixes the request origin to `https://api.z.ai`: custom quota endpoints are disabled in production configuration. The client rejects redirects instead of forwarding `Authorization`, does not honor ambient HTTP proxy variables by default, sends no body, caps the response body at 1 MiB, applies an overall request timeout, validates the JSON schema and status code, and never logs raw headers or bodies.

Premium credits are discounted to 0.5× off peak; peak is Monday through Friday, 06:00–10:00 UTC. Expose an `offpeak` status flag computed from an injected UTC clock. Apply the 0.5× multiplier only to fallback credit events timestamped off peak; do not alter authoritative utilization to re-derive Z.AI's accounting.

### Fallback formula

Use exact fixed point, not binary floating point. Store integer microcredits or an equivalent scale.

GLM-5.3:

```text
(input_tokens × 6.9 + cached_input_tokens × 1.7 + output_tokens × 24) / 10,000
```

GLM-5.3-Flash:

```text
(input_tokens × 2.3 + cached_input_tokens × 0.56 + output_tokens × 8) / 10,000
```

Model names use an explicit alias allowlist derived from observed CPA records. An unknown model is `unpriced`; do not invent a rate. Expose an accounting warning and apply a documented conservative policy.

Token rules:

- bind published cached-input pricing to `UsageDetail.CacheReadTokens` only;
- bind cache writes to `UsageDetail.CacheCreationTokens` and charge them at the normal input rate unless Z.ai documents a separate rate;
- never use generic `UsageDetail.CachedTokens` for pricing because the v7.2.150 Claude usage helper (`parseClaudeUsageNode`) can substitute cache-creation tokens into that field when it is zero;
- subtract `CacheReadTokens` from `InputTokens` only when a redacted real fixture proves that CPA's input count includes cache reads; otherwise treat the fields as disjoint and fail the contract test rather than double-subtracting;
- a failed request with no usage adds no estimated credits; and
- a failure that includes usage is accounted on best effort.

`usage.handle` is a lossy observation channel, not an authoritative ledger: its interface has no acknowledgment, CPA discards RPC errors, delivery may be skipped after request-context cancellation, and `UsageRecord` contains no request ID. The estimator therefore reports an integrity state (`complete_since`, `delivery_warning`, and `dedup_mode`) and becomes conservative when persistence or delivery uncertainty is observed; it never claims exact consumption and never clears an upstream 429. A bounded hash of stable non-secret fields may suppress obvious replays across restart/reconfigure, but collisions can suppress legitimate identical records, so hash dedup is explicitly heuristic rather than replay-safe.

### Fallback rolling windows

Maintain timestamped credit events per account only while authoritative quota is unavailable:

- five-hour: `(now - 5h, now]`;
- weekly: `(now - 7d, now]`.

These local windows are a conservative fallback. They do not override Z.AI's returned `nextResetTime` or bucket size.

For each fallback window:

```text
utilization = consumed_credits / configured_bucket
resets_at   = earliest expiry after which utilization is below threshold
```

When fallback usage is over threshold, `resets_at` is not necessarily `oldest + duration`: walk expiry events until the remaining sum becomes strictly lower than the configured threshold. Inject a clock so boundaries are deterministic.

Persist fallback state with a same-directory temporary write, file `fsync`, rename, and directory `fsync` where supported.

## Health state

Shared account health values:

- `healthy` — valid, enabled, under both thresholds, no active upstream block;
- `throttled` — short per-account request-rate backoff after a documented transient Z.ai 429 (business code `1302` or `1305`); impaired for scheduling, but not a capacity loss;
- `exhausted` — local threshold reached or 429 block active;
- `suspended` — 401/403 block active;
- `disabled` — administratively disabled;
- `config_error` — invalid pair/settings.

| Signal | Result |
|---|---|
| Fresh authoritative quota below thresholds | Healthy after any transient failure timer expires. |
| Either authoritative bucket reaches threshold | Exhaust until its returned reset or a successful refresh below threshold. |
| Quota endpoint unavailable | Retain bounded fresh data, then use the clearly labelled local estimator. |
| Either fallback estimate reaches threshold | Exhaust until the fallback rolling sum falls below threshold. |
| HTTP 429 with Z.ai code `1302`/`1305`, allowance not spent, no hint over 60s | Throttle only that account: 5s, 10s, 20s, 40s, 60s (never above `fallback-cooldown`), or a bounded hint of at most 60s. 429s that land while the throttle is active belong to the same burst and never advance the step. A success after expiry, or 5 minutes without a 429, resets the ladder. A sixth consecutive step takes the conservative fallback cooldown instead. |
| Any other HTTP 429 | Exhaust; parse bounded reset hints or use fallback cooldown. |
| HTTP 401/403 | Suspend for configured duration. |
| Window/timer expiry | Recompute and clear automatically. |
| Management unblock | Clear transient flags (including a request-rate throttle) and recompute retained quota/usage; never erase consumption. |
| Key rotation | Preserve state only for the same hashed identity. |

A real 429 overrides a lower estimate. Repeated failures extend, never shorten, an active block. Treat reset hints as untrusted: bound body/header lengths, reject malformed/past/unreasonably distant values, and expose the chosen reason/reset.

## Scheduler

CPA v7.2.150 invokes only the first active scheduler plugin, ordered by descending `plugins.configs.<id>.priority` and then ascending plugin ID. Quota enforcement is therefore a deployment invariant, not composable middleware: v0.1 supports exactly one enabled scheduler plugin, `subscription-pool`. Startup/dogfood validation inspects the configured and registered capability set and refuses the lane if any second scheduler is enabled or if this plugin is not first. `priority: 1000` is required as defense in depth, but exclusivity is the safety property. CI fixtures cover a lower-priority competitor, a higher-priority competitor, and an equal-priority ID tie; every non-exclusive configuration must fail closed before traffic is admitted.

The coordinator, not any individual provider module, holds this single scheduler slot and dispatches every `scheduler.pick`/`usage.handle` call across its hosted modules under six invariants:

1. **No overlapping ownership.** The dispatch registry (`authID -> providerID`) is rebuilt from every module's `OwnedAuthIDs()` after each successful reconfigure, staged then swapped atomically. If any auth ID is claimed by more than one module, the whole reconfigure fails and the previous registry stays live.
2. **Zero recognizing modules -> unhandled.** The coordinator calls every module's `Recognize` on the incoming candidates before calling any module's `Pick`. If none recognize any candidate, it returns `Handled:false` so unrelated providers remain untouched.
3. **More than one recognizing module -> fail closed.** If candidates in one request are recognized by more than one module, the coordinator returns a `coordinator_mixed_provider_pick` error rather than calling any module's `Pick`.
4. **Usage dispatches by auth ID first.** `usage.handle` looks up `record.AuthID` in the registry, and a registry match always wins over the record's self-reported provider field. Only when no module owns the auth ID does the coordinator ask modules that implement the optional `providermodule.UsageRecognizer`; it dispatches when exactly one claims the record (the synthetic module claims records from its configured provider names, which keeps quota learning working after a key is re-pasted and the host derives a new auth ID). No claimant, or more than one, is a silent no-op, matching CPA's own behavior for usage records belonging to unrelated providers.
5. **Recognized-but-unmanaged or all-impaired never silently falls back.** Exactly one module recognizes the candidates, and the coordinator propagates that module's own `Pick` result verbatim, including any hard scheduler error such as `zai_unmanaged_candidate` or `zai_no_capacity`.
6. **Never blend domains.** A structural consequence of invariant 3: no code path can merge two modules' pick results, because a mixed-recognition request never reaches any module's `Pick`.

For the zai module's own `Pick`, called only once the coordinator has established zai (and no other module) owns every candidate in the request:

1. Map candidates to logical accounts and reject the call with a scheduler error if any recognized Z.ai candidate is absent from the validated snapshot.
2. Recompute expired health.
3. If no managed account is impaired, return `Handled:true, DelegateBuiltin:"round-robin"`; CPA retains native round-robin/session affinity through the explicit SDK delegate.
4. In degraded state, discard every candidate belonging to an impaired account, including its sibling credential.
5. Round-robin among healthy candidates per provider/model, preserving header-derived stickiness when possible (`X-Session-ID`, `Session-Id`, `Session_id`, `X-Client-Request-Id`). Return the selected `AuthID` with `Handled:true`.
6. If the request contains managed Z.ai candidates but no healthy managed candidate remains, return a scheduler error; `Handled:false` would fall back to built-in selection in v7.2.150 and is forbidden on this path. When at least one of those accounts is only `throttled`, the error is the retryable `zai_rate_limited` with `http_status: 429`, which CPA forwards as a 429 so the client retries in seconds. Otherwise it is the non-retryable `zai_no_capacity` (HTTP 500). The `zai_rate_limited` message must never contain "no healthy managed ... capacity remains": the model router quarantines the whole lane for 15 minutes on that phrase. CPA already retries a request on another credential after an upstream failure, so while any sibling account is healthy the throttled account is simply skipped.
7. If the request contains no recognized Z.ai candidate, return `Handled:false` (so the coordinator's invariant 2 branch is unreachable here — Recognize already filtered this module out).

### synthetic module

`internal/providers/synthetic` schedules synthetic.new subscription credentials. A synthetic.new plan has a single weekly credit limit that regenerates 2% every 202 minutes (no five-hour window and no lockout); the Big plan adds infinite mode, the model alias `infinite:small:vision`, which never counts against the weekly limit, runs much slower, and requires `stream: true`.

The host config declares two OpenAI-compatible providers with the same key: a paid provider (default name `synthetic`) and an infinite provider (default `synthetic-infinite`) at a lower `priority`. An alias served by both (for example `synthetic/deepseek-v4.1-flash` -> `hf:deepseek-ai/DeepSeek-V4.1-Flash` on the paid provider and `infinite:small:vision` on the infinite one) gets automatic fallback:

1. Lanes are discovered from the host's `openai-compatibility` entries on every reconfigure, with auth IDs derived exactly as the host derives them. Candidates are also recognized by `compat_name`/provider key, so a re-keyed credential is still recognized. A credential seen at runtime but not in the last reconfigure (a learned lane) is bound to its provider's account when that provider uses exactly one; a learned paid lane that cannot be bound (two or more accounts, or none) fails closed: it is never admitted, because its spend cannot be checked against any weekly limit, and requests go to infinite mode or get the retryable `synthetic_no_capacity` until the next reconfigure binds it.
2. A paid lane is available while its account's weekly usage, projected forward through the regeneration ticks that have passed since the last reading, is below `threshold-percent` (default 95), the account is not in a learned 429/402 exhaustion window, the lane is not resting after a rate-limit 429, and it is under `max-concurrent` (0 = no local cap).
3. `Pick` returns a paid lane (round-robin) when one is available; otherwise an available infinite lane (counted as a fallback); otherwise a retryable `synthetic_no_capacity` / `synthetic_infinite_busy` error with HTTP 429. It never lets a request reach a paid credential that is at the threshold, so a plan with extra usage enabled does not spill into usage-based billing. A candidate set that also contains another provider's credentials makes the module decline (`Handled:false`).
4. Quota comes from `GET <origin>/v2/quotas` (free; polled every `quota-refresh-interval`, default 2m, only for real keys, never for placeholder keys such as `<API KEY PLACEHOLDER>`) and from the `x-synthetic-quotas` header on every response. A paid 429 or 402 parks the account until the reported `nextRegenAt` or `failure-cooldown`; a paid 429 whose header still shows credits rests only that lane for `infinite-cooldown`; an infinite-mode 429 rests the infinite lane for `infinite-cooldown`.
5. In-flight slots are released by the matching usage record and expire after `in-flight-ttl`, so a lost record cannot wedge a capped lane.
6. Status reports per-lane health, the projected weekly usage, the next regeneration tick, in-flight counts, routing counters, and measured throughput (output tokens per second excluding time to first token, plus mean time to first token) so paid and infinite speed can be compared directly.

The plugin registers `scheduler_across_priorities` so CLIProxyAPI v8 hosts offer both priority tiers to the scheduler; without it the infinite lane would be invisible until the paid credential was already cooling down. Picks no module recognizes still return `Handled:false`, and the host's selector then applies its normal priority order.

### Inactive modules

A module with nothing to manage claims nothing: the zai module with no Z.ai credential in the host config, the OpenCode Go module with `enabled: false` or no accounts, and the synthetic module with no matching providers or `enabled: false` all return `Recognize == false` and own no auth IDs. Their providers' traffic goes to the host's own scheduler, and a missing Z.ai plan no longer prevents the plugin from registering. A `providers.zai.accounts` override with no Z.ai credential in the host config still fails reconfigure (`accounts[0]: key-suffix matches no account`), exactly as an unmatched override does when credentials exist: the override names an account the operator expects, so it is never silently dropped.

The scheduler performs no disk, network, or host callback while holding its lock. Contract tests assert the all-impaired module error propagates through the coordinator and `pluginhost.PickAuth` with `handled=true` and cannot fall back to a known-bad credential.

## Management API

`management.register` adds routes mounted by CPA under `/v0/management` and protected by CPA's management key. The coordinator itself answers one aggregated status route; each hosted module additionally declares its own routes, which the coordinator dispatches to verbatim (no path rewriting in this slice — a module's route keeps whatever path it declared before the coordinator existed):

| Method/path | Owner | Purpose |
|---|---|---|
| `GET /v0/management/plugins/subscription-pool/usage` | coordinator | Usage page data (`?range=1h|6h|24h|7d`, default `24h`): ledger buckets and totals per provider, account and model; normalized quota cards (used/remaining percent, reset or regeneration time, window start and length); the synthetic.new panel (regeneration schedule, per-lane in-flight, peak and cap, measured speed, routing counters). Same body as the unauthenticated `usage.json` resource. |
| `GET /v0/management/plugins/subscription-pool/status` | coordinator | Aggregated status: `{"plugin":"subscription-pool","status":...,"providers":{"<id>":<module's own Status() JSON>}}`. Callers reading a specific provider's fields (e.g. `accounts`) must unwrap `providers.<id>` first. |
| `GET /v0/management/plugins/zai-coding-plan/status` | zai module | Redacted utilization/reset state, quota source and freshness, off-peak state, health, and warnings. This is also exactly the JSON aggregated under `providers.zai` in the coordinator's own status route above. |
| `POST /v0/management/plugins/zai-coding-plan/refresh` | zai module | Poll quota now, compact fallback state, and recompute health. |
| `POST /v0/management/plugins/zai-coding-plan/unblock` | zai module | Clear transient blocks, then recompute retained quota/usage. |
| `POST /v0/management/plugins/zai-coding-plan/account-config` | zai module | Save/clear non-key plan metadata. |

Collector-facing zai status is locked by `internal/providers/zai/testdata/status_authoritative.golden.json` and `status_fallback.golden.json`. Utilizations are finite ratios in `[0,1]`; `quota_age_seconds` is a non-negative integer; timestamp fields are RFC 3339; `quota_source` is `quota_api` or `estimate`; and unknown top-level or account fields fail closed. Freshness, reset, off-peak, health, and estimator-integrity fields are machine-visible. `quota_error` is omitted when empty and otherwise contains only a bounded redacted message. Status never contains full keys, derived account identities, request bodies, authorization headers, or management credentials. A retained snapshot returned with `status=reconfigure_rejected` is projected only as stale `config_error` capacity and cannot be selected as healthy capacity; live installation verification rejects that state outright.

The host collector writes its separate sanitized lane file only beneath a pre-created root-owned real directory. It rejects symlinked or substituted output parents, holds an `O_DIRECTORY|O_NOFOLLOW` descriptor across mode enforcement, same-directory temporary creation, atomic replacement, and directory fsync, and writes the final file as mode `0600`. This root-owned collector output is distinct from the unprivileged CPA state directory below.

CPA must reject unauthenticated management HTTP requests before dispatch. The coordinator itself (not any individual module) registers the **Usage and Quota** Management Center menu entry at `/v0/resource/plugins/subscription-pool/status`, since it is the only party that can see every hosted provider. The page follows the official Management Center's Dashboard and Quota Management design (same palette, type, components and layout) and is one self-contained HTML document: inline styles, one inline script allowed by a per-response CSP nonce (`default-src 'none'; script-src 'nonce-...'; style-src 'unsafe-inline'; img-src data:; connect-src 'self'; base-uri 'none'; form-action 'none'`), no external assets, and no `frame-ancestors`, so a cross-origin Management Center can still frame it. Its first payload is embedded as JSON (`json.Marshal` escapes `<`, `>` and `&`, so data cannot close the element); it then refreshes from `/v0/resource/plugins/subscription-pool/usage.json?range=1h|6h|24h|7d` every 30 seconds. CPA dispatches resource routes without management authentication, so both carry only redacted fields: provider IDs, account display names, model aliases, counters, quota percentages and reset times. Never a key, key fragment or fingerprint, auth ID, client API key, request content or management credential. The same JSON is available on the authenticated `GET /v0/management/plugins/subscription-pool/usage`.

### Usage ledger

`internal/usage` counts every usage record a hosted module accepts (owned auth ID or a single `UsageRecognizer` claim), keyed by provider, account display name (from the optional `providermodule.UsageAttributor`, called after `HandleUsage`) and model alias: requests, failures (429, other 4xx, 5xx), input/output/reasoning/cached tokens, latency, time to first token, and output speed (output tokens over generation time, for successful responses of at least 20 tokens and 100 ms). Two tiers bound memory: 5-minute buckets for 48 hours and 1-hour buckets for 8 days, at most 200 rows per bucket (the rest fold into one `(other)` row per provider). The coordinator saves the ledger at most every 5 minutes, in the background, and synchronously on shutdown, to `<auth-dir>/subscription-pool/usage/ledger.state` through `securestore` (0700 directory, 0600 file, atomic rename). The name has no `.json` suffix because the host parses every `*.json` file under the auth directory as a possible credential. Every persistence failure is soft and shows on the page; scheduling never waits on it.

## Persistence and concurrency

The coordinator namespaces all on-disk state under its own plugin ID, then gives each hosted module an isolated subdirectory it alone writes to:

```text
<auth-dir>/subscription-pool/
├── manifest.json              # coordinator-owned, diagnostic only — never read back for dispatch
└── providers/
    ├── zai/
    │   ├── settings.json
    │   ├── state.json
    │   └── settings.recovery.json  # transient during settings commit recovery
    └── opencode-go/                # reserved, empty stub — no module implemented yet
```

`manifest.json` records which providers are currently registered and when; the in-memory dispatch registry, rebuilt from every live module's `OwnedAuthIDs()` on each reconfigure, remains the sole source of truth for dispatch — the manifest is never consulted to make a routing decision. Manifest write failures are soft (logged, never fail `Reconfigure`).

`internal/securestore` implements the durable-storage primitives once, generically, and both the coordinator (for `manifest.json` and the `providers/opencode-go` stub) and the zai module (for its own `providers/zai/*`) use it as a library rather than reimplementing it:

- directory mode `0700`;
- files mode `0600`, including replacements;
- reject symlink targets and use atomic same-directory replacement;
- `os.SameFile`-based detection of a directory replaced out from under an open handle;
- two-phase commit with a recovery marker for interrupted writes.

`internal/securestore` does no path composition itself — callers build the full absolute directory (e.g. `filepath.Join(pluginAuthDir, "providers", "zai")`) before opening a store, keeping each module decoupled from the coordinator's own identity.

zai-module-specific requirements, unchanged from pre-coordinator behavior:

- never persist the API key;
- key settings by hashed account identity so renames survive but rotations reset;
- one in-process mutex protects the active snapshot, state, dedup set, and cursors;
- copy a persistence snapshot under lock, then write after releasing it; and
- shutdown is idempotent and ordered: stop accepting new work, cancel the quota-poller context, synchronously join every poller/refresh worker, then flush persistence and return. No goroutine may retain or call the host callback table after shutdown returns, because CPA deletes host callback state and immediately unloads the `.so`.

Quarantine corrupt state with a redacted diagnostic. Start conservatively with an accounting warning rather than silently replacing it.

## Data flow

```text
CPA config YAML
    │ plugin.register / plugin.reconfigure
    ▼
validate ── discover exact key pairs ── load 0600 state/settings
    │
    ├── both auth IDs ─────────────────────────────┐
    │                                              │
quota poller ── authoritative buckets/resets        │ CPA candidates
    │                                              │ scheduler.pick
    ├── unavailable → local estimator               ▼
CPA completed request ── usage.handle ── health ── recompute health
    │                                  │            │
    ├── usage → fallback rolling ledger│            ├── all healthy → delegate builtin RR
    └── 401/403/429 ───────────────────┘            ├── degraded → healthy candidate
                                                    └── none healthy → scheduler error
    │
    ├── atomic redacted persistence
    └── authenticated status → collector lane `zai`
```

## Threat model

### Assets

Z.ai keys, CPA management authentication, usage/account state, local ledger integrity, and the native `.so` executing in CPA.

| Threat | Control |
|---|---|
| Scheduler precedence bypasses quota enforcement | Require `subscription-pool` to be the only enabled scheduler, validate the configured/registered capability set before admitting the lane, and retain `priority: 1000` only as defense in depth. |
| A provider module blends into or reads another module's private state | Compiler-enforced package boundary (`internal/providermodule.Module`, one package per provider) plus the coordinator's dispatch registry rejecting any auth ID claimed by more than one module. |
| Keys leak through logs/status/errors | Keys live only in the active in-memory account snapshot for exact pairing and authenticated quota requests; they are never persisted or returned. Central redaction and tests scan every serialized output/log/error path for fixture keys. |
| Quota request leaks or is redirected | Fix production requests to HTTPS `api.z.ai`, reject redirects, disable ambient proxy use by default, never forward credentials cross-origin, cap body/time, validate schema, and never log raw headers or bodies. |
| Wrong pairing through suffix collision | Pair only by full-key equality. Suffix matching is override-only and ambiguity fails closed. |
| Unauthenticated quota/account disclosure | Data only on CPA management-key routes. Integration-test unauthorized/authorized HTTP behavior. Resource shell has no data. |
| Local disclosure | `0700` directory, `0600` files, unprivileged CPA user, no secrets in filenames. |
| Ledger tampering creates false capacity | Validate schema, non-negative bounded values, model/rate IDs, timestamps, sizes. Corruption causes conservative degraded state. Unblock never deletes usage. |
| ABI mismatch crashes CPA | Pin v7.2.154 SDK, inspect exported symbol, then load-test exact eceasy image digest. |
| Malicious reset hints deny service | Allowlist fields, cap lengths/future duration, reject malformed/past values. |
| Usage replay double charges | Persist a bounded heuristic hash of stable non-secret fields; surface collision/replay limitations through `dedup_mode` and integrity warnings. |
| Concurrency race/deadlock | No host callback or I/O under state mutex; run `go test -race`. |
| Release substitution | Protected main, PRs, pinned modules, CI, tagged checksums, store checksum verification. |

Key discovery/persistence and management routing require independent security review at the exact commit SHA. CI builds release artifacts from reviewed semver tags; do not publish locally built binaries.

## CI and release gates

Pull requests must pass:

```text
gofmt check
go vet ./...
golangci-lint run
go test -race ./...
CGO_ENABLED=1 GOOS=linux GOARCH=amd64 go build -buildmode=c-shared ./src
nm -D <artifact> | grep ' cliproxy_plugin_init$'
```

A release job loads the `.so` into the exact `eceasy/cli-proxy-api` v7.2.x image digest, confirms registration and all advertised capabilities, and calls authenticated status. This is mandatory before dogfood.

A `vMAJOR.MINOR.PATCH` tag produces a versioned `.so`, CPA store zip, SHA-256 checksums, changelog release notes, and matching `registry.json`. Every PR receives exact-SHA review; keys/management changes also receive security review.

## Test strategy

Unit/property coverage:

- quota response parsing for Pro/Lite/Max buckets, reset epochs, malformed/oversized input, endpoint failures, and off-peak boundaries;
- quota client redirect rejection, fixed-origin enforcement, disabled ambient proxies, body/time caps, secret redaction, and cancellation;
- poll jitter, timeout, stale-data, authoritative-to-fallback-to-authoritative reconciliation, and freshness fields;
- exact fallback formula vectors, large counts, zero usage, and rounding boundaries;
- cache semantics and unknown models;
- fallback rolling boundary and earliest threshold reset with multiple events;
- Lite/Pro/Max/custom bucket validation;
- exact pairing, duplicates, missing siblings, ambiguous suffixes, rotation;
- reset headers/body variants and adversarial values;
- cross-protocol 401/403/429 health;
- scheduler precedence/exclusivity with lower-, higher-, and equal-priority competitors; healthy explicit built-in round-robin delegation, degraded exclusion/round-robin/stickiness, and all-impaired hard-error propagation;
- invalid reconfiguration retains the previous internal snapshot, returns `invalid_config`, surfaces the validation error, and is withdrawn from the host capability snapshot;
- lossy usage delivery, persistence-failure integrity state, and heuristic dedup collision/replay cases;
- shutdown cancels and joins every background worker before callback teardown/unload;
- permissions, atomic replacement, corrupt files, symlinks; and
- collector status golden JSON plus adversarial strict-schema, RFC JSON, timestamp, integer-age, rejected-reconfiguration, one-line-key, and output-parent substitution probes.

Coordinator/dispatch coverage (`internal/coordinator`):

- registry rejects overlapping/duplicate auth-ID ownership across modules, and a rejected rebuild leaves the previous registry live;
- zero recognizing modules returns `Handled:false`; more than one recognizing module fails closed with `coordinator_mixed_provider_pick`;
- usage dispatch resolves strictly through the registry by auth ID, never a self-reported provider field;
- a recognized-but-unmanaged or all-impaired pick propagates the owning module's own hard error verbatim, never a silent `Handled:false`;
- manifest write and empty provider stub-dir (`providers/opencode-go`) creation, and idempotence across repeated reconfigures.

ABI/integration coverage:

- Go 1.26 CGO `.so` build and exported symbol;
- upstream v7.2.150 load baseline;
- exact deployed eceasy image load;
- paired-account synthetic usage/failure flow;
- management-key enforcement; and
- persistence across shutdown/reload without double count.

## Operational and ToS note

The Z.ai GLM Coding Plan is licensed for supported coding tools, and Claude Code is one of those tools. Our intended agent workloads are Claude Code sessions. Deployment must enforce that boundary outside the plugin: expose the `zai` lane only to the authenticated Claude Code agent environment, do not publish it as a general OpenAI-compatible endpoint, and do not route arbitrary clients through the plan credentials. The plugin classifies credentials and capacity; it cannot itself prove which end-user tool originated generic CPA traffic. Re-check current Z.ai terms before any release that materially changes clients or traffic patterns.

The repository and reference plugin are MIT-licensed. Preserve required notices when adapting reference code.

## Open verification items before v0.1.0

1. Capture a redacted real quota response fixture and verify the five-hour/weekly unit mapping and utilization scale against the live endpoint.
2. Capture redacted real `pluginapi.UsageRecord` fixtures to validate input/cache field overlap and model aliases for fallback accounting. Pricing is already bound to `CacheReadTokens` and `CacheCreationTokens`, never generic `CachedTokens`.
3. Capture real redacted Z.ai 429/reset variants.
