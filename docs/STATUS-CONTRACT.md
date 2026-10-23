# Z.ai identity and transient cooldown status contract

This is an additive producer contract, not a deployed collector integration or
permission to publish status. It changes neither scheduling nor provider polling.
The collector must implement and review its own acceptance boundary before use.

## Supported source

Use the authenticated management JSON from:

- `GET /v0/management/plugins/zai-coding-plan/status` (the Z.ai module body), or
- `GET /v0/management/plugins/subscription-pool/status`, selecting **only**
  `providers.zai` and that object's own `generated_at`.

`zaiModule.Status` serializes `pluginRuntime.managementStatus`. The export is on
`managementAccountStatus`, not the internal account struct. The coordinator
passes this JSON through; do not infer this contract from an HTML resource page,
a health label, a quota reset, the aggregate timestamp, or another provider.
A live aggregate status request can invoke provider polling (see OpenCode Go
below). Do not describe it as side-effect-free or run it as an offline probe.

## Fields

Existing fields retain their names and meanings. Each Z.ai `accounts[]` entry
adds:

| Field | Type | Meaning |
|---|---|---|
| `identity` | string | Full 64-character lowercase hexadecimal account pseudonym. |
| `cooldown.active` | boolean | Whether the runtime's transient 429 `ExhaustedUntil` is strictly after `generated_at`. |
| `cooldown.until` | RFC3339 UTC string or null | Active transient deadline, otherwise explicit null. |
| `cooldown.reason` | string | Closed-vocabulary deadline provenance below; empty when inactive. |
| `cooldown.source` | string | Exactly `zai_runtime_health_v1`, including when inactive. |

`generated_at` already exists at the Z.ai body root. It timestamps the runtime
snapshot projection, **not** an upstream observation, a cooldown start, or an
attestation of quota freshness. Quota age/source fields describe a separate
signal. Timestamp encoding uses Go `time.Time.MarshalJSON` (RFC3339 with
fractional-second precision as needed): https://pkg.go.dev/time@go1.26.0#Time.MarshalJSON.

Example fragments (pseudonym and times are synthetic):

```json
{
  "identity": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
  "cooldown": {
    "active": true,
    "until": "2026-09-24T12:02:00Z",
    "reason": "retry_after",
    "source": "zai_runtime_health_v1"
  }
}
```

Fresh explicit no-active-state:

```json
{"active":false,"until":null,"reason":"","source":"zai_runtime_health_v1"}
```

### Reason vocabulary

| Value | Existing runtime origin |
|---|---|
| `retry_after` | Validated Retry-After header after a 429. |
| `reset_header` | Validated recognized rate-limit reset header after a 429. |
| `reset_body` | Validated reset hint from a bounded 429 response body. |
| `quota_reset_fallback` | Runtime used its capacity reset deadline after a 429 without a usable hint. This does not independently certify the quota source as authoritative. |
| `configured_fallback` | Runtime used its configured fallback duration after a 429. |
| `upstream_rate_limit` | Active runtime cooldown with an unrecognized/legacy reason; original text is withheld. |

The export never forwards free-text persisted reasons, header values, or response
bodies. These labels explain the existing deadline selection; they do not change
it or claim a new provider observation.

## Identity and redaction boundary

The identity is the **existing** `accountIdentity` result: HMAC-SHA256 over the
trimmed full plan key with fixed public namespace
`zai-coding-plan:account-identity:v1`. It is stable across account/display renames,
array reorder, paired API lanes, process restart, and unchanged-key
reconfiguration. Key rotation changes it. It is not a CPA auth ID and cannot be
used to fabricate one. It is not an account name, suffix, or positional index.

The fixed namespace is public, not a secret salt. This identifier permits
cross-install correlation and candidate-key verification; it is pseudonymous,
**not anonymous**. Treat it as restricted management metadata. Do not publish it
in public telemetry, public dashboards, logs, or metric labels. Exact-head
security/redaction review is required before landing this exposure.

Full keys, raw key suffixes (`key_suffix` stays `"redacted"`), CPA auth IDs,
request headers, and failure bodies are not newly exported. The unauthenticated
resource dashboard uses a separate typed subset and does not render these new
identity/cooldown fields; a regression test guards that boundary. Existing
operator-configured display names are not join keys or a secret-storage field.

## Lifecycle and consumer acceptance

This contract covers only the **transient 429 block**. Authentication suspension,
administrative disablement, and quota-threshold exhaustion remain independent.
An account can have `cooldown.active:true` while `health` says `suspended` or
`disabled`; conversely `cooldown.active:false` never proves scheduler eligibility.
Quota exhaustion alone must not become a cooldown. Neither does a short
request-rate throttle (`health:"throttled"`, at most 60 seconds, after a Z.ai
`1302`/`1305` 429): it is reported only through `health`, and `cooldown` stays
inactive unless the throttle escalates to the conservative fallback cooldown.

At the deadline (`until <= generated_at`), the producer emits the explicit
inactive object, even if an expired internal timestamp is retained. Existing
unblock clears the transient block; a subsequent fresh status emits inactive.
Unblock does not clear suspension or manufacture quota capacity. This is a
snapshot, not an event log: it cannot distinguish expiry from manual release,
provide the original apply time, or guarantee observation of every transition.

A conservative collector acceptance policy, exercised by the test-only consumer
in `internal/providers/zai/status_contract_test.go`:

1. Select the Z.ai body explicitly. Require valid JSON, `plugin:"zai-coding-plan"`,
   `status:"registered"`, a nonzero parseable `generated_at`, and an accounts
   array. An omitted/null accounts array means unavailable evidence, not a
   global release. Older producers missing these new fields are unsupported.
2. Set a positive, bounded maximum snapshot age in the consumer. The executable
   examples use **60 seconds**, allow no future clock skew, and reject older or
   future-dated snapshots. This is a test policy, not a deployed collector
   setting. Reject snapshots older than the last accepted generation in a
   stateful collector; never refresh their age just because they were fetched.
3. Validate the full snapshot before producing any changes. Require unique
   lowercase 64-hex identities, every cooldown field, exact supported source,
   boolean `active`, and the closed reason vocabulary. Malformed, missing,
   unsupported, or contradictory data refuses the snapshot without partial
   writes or release instructions. Never log the rejected raw payload.
4. Join only an explicitly known full identity in the Z.ai provider namespace.
   A well-formed unknown identity is dropped, even for a singleton. Never join
   by name, suffix, lane index, array order, or guessed auth ID. This export
   supplies no CPA-auth mapping; if the consumer lacks an independently
   authorized mapping, integration remains blocked rather than guessing.
5. Active evidence requires `until > generated_at` and `until > consumer_now`.
   A deadline elapsed in transit is dropped, not converted to a fresh release.
   Even a future deadline must be discarded when its snapshot becomes stale.
6. Inactive evidence requires explicit `active:false`, `until:null`, and empty
   reason. It may release only the collector's own matching transient-429
   observation, never native host suspension/quota/administrative state.
   Missing accounts, unknown identities, stale data, and parse errors are not
   evidence of health, eligibility, or release. A collector must separately
   expire its own stale observations without falsely asserting provider health.

The test-only consumer demonstrates validation/drop semantics and does not
implement storage, monotonic generation tracking, host joins, or any writes.
No public telemetry transformer or historical helper is retargeted by this
change. Historical PR #386 remains proposal-only evidence, not deployed
integration evidence; a helper's nonzero exit must not be assumed write-free.

## OpenCode Go is not this contract

`internal/providers/opencodego/module.go`, `Module.Status`, first calls
`pollUsageOnce(ctx)` and later refreshes account windows. It is **not a pure
read**: with configured dashboard credentials it can make network requests and
update quota state. Offline contract tests use synthetic state/clients only.

Its serialized window fields include `known`, `exhausted`, optional `resets_at`,
`source`, and `authoritative`; it does not export the internal `CooldownAt`, the
Z.ai identity, or this cooldown object. Window resets and exhaustion have a
different lifecycle. Never reinterpret `resets_at` as a transient cooldown
expiry, join by display name, copy a Z.ai identity into OpenCode Go, or invent an
auth ID. The supported collector disposition for OpenCode Go cooldown data is
**unsupported / no accepted cooldown evidence**, not `active:false`. A separate
producer contract and review would be needed to change that disposition.

## synthetic.new status

`GET /v0/management/plugins/subscription-pool/status` -> `providers.synthetic` is a separate, additive shape. It never contains an API key; accounts are identified by a one-way `account` fingerprint and the host auth ID.

| Field | Meaning |
|---|---|
| `status` | `registered`, `inactive` (no matching providers or `enabled: false`), or `reconfigure_rejected` with `validation_error` |
| `threshold_percent`, `paid_providers`, `infinite_providers` | effective configuration |
| `credential_bound` | `true` once any lane has a real key; placeholder keys report `false` |
| `accounts[]` | one entry per lane (provider entry x api-key entry): `name`, `provider`, `kind` (`paid` or `infinite`), `auth_id`, `account`, `learned` (credential seen at runtime but not in the last reconfigure), `credential_bound`, `health` (`healthy`, `throttled`, `exhausted`, `unbound`), `health_reason`, `in_flight`, `max_concurrent`, `last_poll_error` |
| `accounts[].windows.weekly` | `known`, `utilization` (0-1, projected through regeneration ticks since the last reading), `exhausted`, `resets_at` (the next 2% regeneration tick, not a full reset), `source`, `authoritative` (true for `/v2/quotas` polls). Infinite lanes report `utilization: 0` with source `infinite mode (free, not metered)` |
| `accounts[].next_regen_percent` | percent of the weekly limit returned at `resets_at` |
| `accounts[].stats` | `requests`, `failures`, `output_tokens`, `tps_samples`, `mean_output_tps`, `median_output_tps` (output tokens / (latency - time to first token), responses with at least 20 output tokens, last 200), `mean_ttft_ms`, `last_status` |
| `routing` | `paid_picks`, `infinite_picks`, `fallbacks_to_infinite`, `rejections`, `infinite_non_stream_picks`, `last_fallback_reason`, `last_fallback_at` |

Counters and throughput reset when CLIProxyAPI restarts; they are not persisted.
