# Management Center proposal: Subscription Pool quota cards

## Current integration and ABI findings

The installed plugin is **Subscription Pool: Z.ai · OpenCode Go · synthetic.new** (stable ID `subscription-pool`, source `TogetherWeOwn/cpa-plugin-subscription-pool`). One coordinator hosts all three provider modules. The existing **Usage and Quota** resource menu already displays their usage, quota windows, health and Synthetic paid/infinite lanes together.

The plugin builds against CLIProxyAPI SDK **v7.2.154**, native ABI 1, registration schema 5. Verified against `sdk/pluginapi/types.go` and `internal/pluginhost/rpc_schema.go` of that tag:

- `Metadata` supports `Name`, `Version`, `Author`, `GitHubRepository`, `Logo` and `ConfigFields`, but no description field. The description belongs in `registry.json` and the resource-menu description; adding a guessed metadata field would be ignored.
- `ManagementRegistrationResponse.Resources` supports `Path`, `Menu` and `Description`. The host derives `menus` from these resources. The plugin registers `/status` with menu **Usage and Quota** and `/usage.json` without a menu. Neither route receives a management key.
- The pinned SDK has **no `QuotaProvider` capability or quota RPCs**. Consequently v0.6.1 does not advertise `quota_provider` and `supports_quota` remains false. A logo/menu is not evidence of a native quota provider.

The deployed patched v8.0.38 host has a newer contract in `sdk/pluginapi/types.go`, `internal/pluginhost/rpc_schema.go` and `internal/api/handlers/management/plugin_quota.go`: a `quota_provider` capability, `quota.identifier`, `quota.describe`, `quota.fetch` and `quota.reset` RPCs. `QuotaDescribeResponse.SupportedProviders` can name several provider keys, and `QuotaFetchResponse` supplies normalized subscription, summary and grouped quota buckets. This is a real future bridge, not present in the plugin's pinned SDK. It needs a separately tested SDK/schema compatibility change and credential-to-logical-account binding before it can be advertised safely. Do not just flip `supports_quota`: the host derives that flag from registered handlers.

The older v7.2.151/v7.2.157 Management Center findings remain relevant to the fallback design: the panel cannot add a compiled `QuotaProviderType`/adapter at runtime. Fabricating an auth file through `host.auth.save` solely to get a Quota card is inappropriate. It creates credential-shaped state, does not teach the panel about three-provider windows, and couples presentation to real credentials.

## Proposed panel integration (all three providers)

### 1. Discover the coordinator, not the retired plugins

Use authenticated `pluginsApi.list()`. When an enabled, registered plugin has ID `subscription-pool`, read one snapshot from:

```text
GET /plugins/subscription-pool/status
```

`apiClient` resolves this under `/v0/management` and supplies its existing Bearer management key. Status is an aggregate with `providers.zai`, `providers["opencode-go"]` and `providers.synthetic`. Missing, disabled or inactive modules produce no virtual entries; failure in one section must not hide the others. Do not discover `zai-coding-plan` or `opencode-go-pool` as installed plugins: their disabled legacy config entries are superseded.

The authenticated usage endpoint is an alternative shared presentation DTO:

```text
GET /plugins/subscription-pool/usage?range=24h
```

Its `quota[]` cards already normalize known/unknown utilization, reset versus regeneration versus free windows, source and freshness across the three modules. Reuse that conversion rather than create inconsistent percentage math.

### 2. Add a Subscription Pool adapter

Extend the panel's compiled provider types, configuration map, quota tab order, virtual-entry classification, store and DTO types together. A single `subscription-pool` adapter can render provider-specific sections labelled **Z.ai**, **OpenCode Go** and **synthetic.new**. Keep provider/module IDs separate from the human-readable plugin name.

Fetch one snapshot per batch, not once per account. Assign virtual-entry keys from plugin ID, module ID and the plugin's stable account/lane identity, never a raw key, suffix or auth-files `account` value. For Z.ai, use the authenticated identity pseudonym only inside the panel's management state; it is not a public resource identifier. Synthetic lanes have persisted ordinal labels such as `synthetic#1` and `synthetic-infinite#1`.

Do not manufacture files in the auth-files API. Z.ai can pair two credential protocols into one logical account, OpenCode Go manages explicit auth-ID sets, and Synthetic paid/infinite lanes may share a subscription. These are not one-file-per-account relationships.

### 3. Render provider-specific semantics faithfully

| Module | Show | Never infer |
|---|---|---|
| `zai` | plan, five-hour and weekly utilization/reset, health, quota source, freshness and off-peak fields | an unknown weekly window is not 0% used; transient request throttles are not quota exhaustion |
| `opencode-go` | each account's dashboard/observed windows, utilization, reset times, source, freshness and health | do not copy another account's dashboard snapshot or invent a reset time from a cooldown |
| `synthetic` | paid weekly credits, threshold, next regeneration tick, projected full time, free infinite-mode status, per-lane concurrency and paid/infinite usage/speed | a 2%-per-202-minutes regeneration tick is not a complete weekly reset; infinite mode is not a metered full quota bucket |

Z.ai/OpenCode Go status utilization is a ratio; the usage DTO uses percentages. Convert exactly once, reject non-finite numbers and clamp display fractions to `[0,1]`. Preserve `known`, `stale`, `source` and authoritative/estimated distinctions. The quota timeline must treat resetting, regenerating and unmetered windows differently, rather than represent every provider as a 5-hour/168-hour allowance.

### 4. Refresh without amplification

Ordinary card loading reads the shared status/usage snapshot and relies on each module's bounded background polling. Z.ai retains its compatibility management operation:

```text
POST /plugins/zai-coding-plan/refresh
```

This is a module route served by the coordinator, not a legacy plugin config to re-enable. Its body must be empty or `{}`. Batch Z.ai refresh invokes it once then reads one aggregate snapshot. OpenCode Go and Synthetic currently have no equivalent management refresh route: refetch the snapshot; do not invent endpoints or expose dashboard/API keys to the browser.

## Future native v8 quota bridge

After an SDK upgrade has passed the immutable host matrix:

1. Implement all advertised quota RPCs and a stable `subscription-pool` identifier. Describe only provider keys that the bridge really resolves; never claim broad `claude`/`openai` ownership merely because Z.ai uses those protocols.
2. Resolve each `quota.fetch` request's auth ID through the coordinator's managed-ownership registry, then return only that logical account's normalized buckets. Reject unmanaged IDs. Do not echo request `StorageJSON`, metadata, attributes or credentials into results.
3. Preserve unknown/stale windows and Synthetic regeneration semantics. The normalized v8 bucket type has only window, remaining fraction, reset time and description; do not fake full capacity or a full reset if it cannot encode the source semantics. Keep the richer Usage and Quota menu available.
4. Describe `supports_reset: false`; quota reset is not a subscription operation. Return an explicit unsupported response rather than clearing accounting or buying capacity.
5. Verify native plugin-list `supports_quota`, provider discovery, per-auth fetch isolation and actual official-panel rendering on a throwaway instance of the production image. Only then publish the bridge as supported.

## Security constraints

- Never put the management key in an iframe URL, query, fragment, HTML body, `postMessage`, resource response or logo. Keep management fetches in `apiClient`.
- Resource routes are unauthenticated and informational only; their DTO contains redacted labels and closed codes, not upstream error text or credential-derived fingerprints.
- Do not display API-key-backed auth-files `account` fields; these can contain raw API keys.
- Render unknown text safely and validate optional fields before using them.
- Never create synthetic credential files or write `quota.signals` on unrelated credentials for presentation.

## Acceptance tests for the proposed bridge/panel work

1. Discovery uses `subscription-pool`; absent/disabled/unregistered plugins create no cards or error banner.
2. One status/usage request serves all accounts across all three modules in a batch.
3. Each module can be absent, disabled, malformed or stale without corrupting sibling cards.
4. Z.ai ratios convert once; five-hour and weekly windows, source and stale flags remain distinct.
5. OpenCode Go accounts retain their own snapshots and unknown reset times.
6. Synthetic paid/infinite lanes retain ordinal labels and correct regeneration/free semantics.
7. Batch Z.ai refresh performs one POST and one aggregate GET; other modules receive no invented refresh requests.
8. No management credential, auth ID, fingerprint, raw key or request content leaks into public resources, logos or iframe properties.
9. Native fetch (when implemented) rejects unmanaged credentials and never confuses protocol siblings, dashboard accounts or Synthetic subscriptions.
10. Existing compiled auth-file providers keep their current loading, sorting and refresh behavior; rollback restores them unchanged.

## Rollback and migration

The panel work is gated by discovery of the enabled `subscription-pool` plugin. Reverting its adapter/store/virtual-entry merge restores the existing Quota page without altering credentials or scheduling. v0.6.1 branding itself keeps the plugin ID, `.so` prefix, config key, model aliases, state paths and API routes unchanged. Remove only the disabled legacy **plugin config entries**, after GET/backup/full-section PUT and model-catalog comparison; retain their `.so` files for rollback. Never remove Z.ai's OpenAI-compatible provider named `zai-coding-plan` or the module's compatibility management routes as part of plugin cleanup.
