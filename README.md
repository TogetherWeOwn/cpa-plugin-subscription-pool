# Subscription Pool: Z.ai · OpenCode Go · synthetic.new

<img src="src/logo.svg" alt="Subscription Pool logo" width="64" height="64">

`subscription-pool` is a native Linux/amd64 plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that schedules flat-rate subscription credentials so they are used fully without hitting provider limits. One coordinator holds CLIProxyAPI's scheduler slot and hosts one module per provider:

| Module | Provider | What it does |
|---|---|---|
| `zai` | Z.AI Coding Plan | pairs each key's Anthropic and OpenAI-compatible credentials, tracks the five-hour and weekly quota, keeps impaired accounts out of scheduling |
| `opencode-go` | OpenCode Go | quota-aware account pooling from the usage dashboard and observed 429s |
| `synthetic` | synthetic.new | weekly-credit threshold scheduling, optional concurrency caps, and automatic fallback to the free infinite mode ([details](#syntheticnew)) |
| `cloudflare-workers-ai` | Cloudflare Workers AI | account-wide daily neuron guard, conservative durable reservations, and explicit PAYG opt-in ([details](#cloudflare-workers-ai)) |
| `nvidia-nim` | NVIDIA-hosted NIM / compatible paid NIM endpoint | default-on free-only model/endpoint guard, durable local request reservations and shared RPM safety cap ([details](#nvidia-nim)) |

Modules with nothing configured are inactive and leave their providers to CLIProxyAPI's own scheduler. This repository was previously named `cpa-plugin-zai-coding-plan`.

Give each managed provider its own model aliases. When an alias also maps to a provider no module manages, a request can carry both kinds of credential. The owning module must then pick one of its own credentials explicitly. Otherwise the request fails with `coordinator_mixed_provider_pick`, or with the module's own error (for example `synthetic_foreign_candidate` or `cloudflare_no_capacity: foreign_candidate`), so the host scheduler never routes managed credentials around their quota guards. Z.ai and OpenCode Go serve such requests from their own healthy credentials. synthetic.new, Cloudflare Workers AI and NVIDIA NIM refuse them.

> [!IMPORTANT]
> Do not deploy an untagged artifact as a release. A version is releasable only after the exact release commit passes CI, immutable-image compatibility, code-review, and security-review gates and its tag is published.

## Capabilities

The plugin advertises these CLIProxyAPI capabilities:

- `scheduler`: explicitly delegate healthy traffic to native round-robin, select only healthy candidates while the pool is degraded, and fail closed when all managed capacity is impaired;
- `usage_plugin`: consume usage and failure records to maintain shared health and fallback accounting across both credential protocols;
- `management_api`: expose authenticated status, refresh, unblock, and non-secret account-configuration operations.

Quota data comes from each module's own source: Z.ai's plan endpoint (with conservative token-based estimation as fallback), OpenCode Go's per-account usage dashboard and observed 429s, and synthetic.new's quota endpoint/response headers with credit regeneration. Unknown or stale quota is never presented as a fresh full allowance.

The official plugin list shows **Subscription Pool: Z.ai · OpenCode Go · synthetic.new**, TogetherWeOwn as author, the renamed source repository, an embedded logo, and the existing **Usage and Quota** menu. The stable ID, configuration key, resource URLs and `.so` prefix remain `subscription-pool`; no credential, alias or state migration is needed. Descriptions live in the registry and resource menu because SDK v7.2.154's `Metadata` has no description field. That pinned SDK has no `QuotaProvider` capability, so `supports_quota` remains `false` rather than advertising unimplemented RPCs; the newer v8 host supports a bridge, but adding it requires a separately compatibility-tested SDK/handler change. See the [three-provider quota integration proposal](docs/management-center-quota-proposal.md).

## Requirements

- Linux amd64
- a CLIProxyAPI host accepting native plugin ABI 1 and schema 5 (SDK v7.2.154; baseline v7.2.150, with newer v8 hosts covered by the exact-image gates; v7.2.67 accepts only schema 1 and cannot load this plugin)
- exactly one enabled scheduler plugin, with `subscription-pool` configured at priority `1000`; other enabled plugins only when listed in `coexisting-plugins` (operator-confirmed to register no Scheduler) and sorted after it
- credentials for at least one configured module, used under that provider's actual account/model entitlement and terms (unconfigured modules remain inactive; NVIDIA trial rights are not production rights)
- Go 1.26 and a GCC-compatible C toolchain only when building from source

The supported baseline is CLIProxyAPI v7.2.150. Newer stock versions, including v8, must pass the immutable compatibility matrix; a patched production image additionally requires its own isolated preflight. CI always tests the v7.2.150 baseline, the operator-recorded deployed-evidence image, and the latest published stock tag. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the pinned SDK, ABI, scheduler-exclusivity, persistence, and threat-model contracts.

## synthetic.new

The `synthetic` module expects two OpenAI-compatible providers that share one synthetic.new API key: the paid provider (spends weekly credits) and the infinite-mode provider at a lower priority. Give an alias to both providers when you want automatic fallback, and a provider-specific alias when you want to choose the lane yourself:

```yaml
openai-compatibility:
  - name: synthetic-infinite          # free, slow, Big plan only, streaming required
    priority: -1
    base-url: https://api.synthetic.new/openai/v1
    api-key-entries:
      - api-key: ${SYNTHETIC_API_KEY}
    models:
      - {name: "infinite:small:vision", alias: synthetic-infinite/deepseek-v4.1-flash}
      - {name: "infinite:small:vision", alias: synthetic/deepseek-v4.1-flash}   # fallback for the paid alias
  - name: synthetic                   # weekly credits
    base-url: https://api.synthetic.new/openai/v1
    api-key-entries:
      - api-key: ${SYNTHETIC_API_KEY}
    models:
      - {name: "hf:deepseek-ai/DeepSeek-V4.1-Flash", alias: synthetic/deepseek-v4.1-flash}
      - {name: "hf:moonshotai/Kimi-K3", alias: synthetic/kimi-k3}
plugins:
  configs:
    subscription-pool:
      providers:
        synthetic:
          threshold-percent: 95        # stop new paid requests at 95% of the weekly credits
          quota-refresh-interval: 2m   # GET /v2/quotas is free; "0" uses response headers only
          max-concurrent: 0            # paid in-flight cap per key; 0 = none (Synthetic publishes no limit)
          infinite-max-concurrent: 0
```

Behavior:

- `synthetic/deepseek-v4.1-flash` uses the paid credential while the plan is under the threshold and switches to infinite mode when it is at the threshold, exhausted (429/402), or at `max-concurrent`. Without the plugin, CLIProxyAPI's own priority order gives the same fallback, but only after the paid credential has failed and cooled down.
- Paid-only models (no infinite alias) get a retryable HTTP 429 `synthetic_no_capacity` at the threshold rather than a request that could be billed as extra usage.
- The paid credential is admitted only with a known, fresh weekly reading. Before the first poll or `x-synthetic-quotas` header arrives, and once the last reading is older than `quota-max-age` (default 10m, or twice `quota-refresh-interval` when that is longer), requests go to infinite mode or get the same 429. With `quota-refresh-interval: "0"`, readings come only from response headers, so the paid credential stays closed until infinite-mode traffic brings one.
- Weekly usage comes from `GET /v2/quotas` and the `x-synthetic-quotas` response header and is projected through the documented regeneration (2% every 202 minutes). Placeholder keys (for example `<API KEY PLACEHOLDER>`) are never sent anywhere; the lanes show `health: unbound` until a real key is configured.
- `GET /v0/management/plugins/subscription-pool/status` -> `providers.synthetic.accounts[].stats` reports measured output tokens per second and time to first token per lane, so infinite-mode speed can be compared with the paid lane.

## Cloudflare Workers AI

The module defaults to `stayWithinIncluded: true`. The example host provider is **disabled** until its scoped credential and account analytics schema have been verified. Activation also requires the exact-source review/release and immutable-host gates.

Cloudflare includes **10,000 neurons per account per day**, on both Workers Free and Workers Paid. The daily allowance resets at **00:00 UTC** and does not roll over. There is no documented additional monthly Workers AI pool with Workers Paid. In October, 31 daily allowances total 310,000 neurons (a $3.41 overage-price equivalent), **not a spendable monthly credit**. Paid overage is $0.011 per 1,000 neurons; the existing Workers Paid subscription starts at $5/account/month and is separate. [Workers AI pricing](https://developers.cloudflare.com/workers-ai/platform/pricing/) · [Workers pricing](https://developers.cloudflare.com/workers/platform/pricing/).

```yaml
plugins:
  configs:
    subscription-pool:
      providers:
        cloudflare-workers-ai:
          enabled: true
          stayWithinIncluded: true       # false explicitly permits PAYG
          safety-margin-neurons: 500
          usage-refresh-interval: 30s
          usage-max-age: 2m
          providers: [cloudflare-workers-ai]
```

Use the **account-scoped** OpenAI base `https://api.cloudflare.com/client/v4/accounts/<account-id>/ai/v1`; do not append another `/v1`. The full prefixed model entries and served contexts are in [`config.example.yaml`](config.example.yaml); [`deploy/config.yaml.tmpl`](deploy/config.yaml.tmpl) starts with the two bounded text models. Render secret placeholders through the operator's supported channel before applying, keep populated config mode `0600`, and never log it. Do not assume shell-variable expansion by CLIProxyAPI.

Guard behavior:

- All keys pointing at one Cloudflare account share one daily budget. The scheduler refuses unknown, stale, malformed or permission-denied analytics, unbound credentials, unpriced/unbounded models, and requests that cannot fit the safety margin plus the full reservation. Errors are retryable `cloudflare_no_capacity` (429). A model RPM 429 is a brief cooldown, not invented daily exhaustion.
- Because this SDK's scheduler does not receive the body or requested token limits, included-only mode reserves **the full served text context for both input and output**, at uncached published neuron rates, before dispatch. The reservation is bound to each host model entry's actual upstream ID, not the spelling of its alias; missing or unbounded mappings fail closed. Local debits are durably saved and conservatively **added** to analytics, even when that double-counts completed work. Reservations are not refunded from token estimates. Qwen3 30B reserves 1,151 neurons/request; GLM 4.7 Flash reserves 5,492. Larger or multimodal catalog models are PAYG-only under this guard; no quality/routing threshold is lowered to admit them.
- State is atomic under the plugin's auth directory, `providers/cloudflare-workers-ai/budget.json`. Restart does not reset it. An unfinished batch carries its full reservations across midnight until all callbacks arrive; completion order is not assumed. Storage corruption/write failure fails closed. Do not delete state to manufacture capacity.
- `stayWithinIncluded: false` explicitly bypasses the included-neuron gate for normal PAYG, while credential validation and provider cooldowns remain. After any PAYG request, switching back stays blocked until the UTC day after the whole active batch completes, including when completion crosses midnight. Disabling the guard while a matching host provider remains enabled is rejected; disable the host provider to take it out of service.
- This is a **local routing guard, not an atomic Cloudflare billing cap**. Adaptive analytics can lag or be sampled, other applications can spend the same account allowance, and independent plugin replicas do not share reservations. Require one active scheduler per account; use Workers Free or a provider-enforced spend cap if an absolute zero-charge guarantee is required.

Analytics uses `https://api.cloudflare.com/client/v4/graphql`, account-wide `aiInferenceAdaptiveGroups.sum.totalNeurons`, filtered to the current UTC date. **The exact live schema/filter and scoped-token permission must be verified before activation**; fixtures are synthetic and schema/permission errors fail closed. Polling sends no inference traffic. The token needs only the chosen account resource, Workers AI Read (the inference API also accepts Workers AI Write) and Account Analytics Read; validate the actual permission-group names through the authorized scoped-token workflow. [Inference permissions](https://developers.cloudflare.com/api/resources/ai/methods/run/) · [Analytics authentication](https://developers.cloudflare.com/analytics/graphql-api/getting-started/authentication/api-token-auth/) · [Sampling caveat](https://developers.cloudflare.com/analytics/graphql-api/sampling/).

The Usage and Quota page shows a daily UTC window, included-only/PAYG mode, measured neurons separately from conservative local debits, the safety reserve, and the calendar projection explicitly labelled not pooled. Account labels are generated; account IDs, auth IDs, keys and upstream diagnostics never reach the public page. A guard pause can happen below 100% because the remaining headroom cannot fit another full reservation.

[OpenAI compatibility](https://developers.cloudflare.com/workers-ai/configuration/open-ai-compatibility/): chat completions and embeddings; streaming and tools depend on the selected model. `/v1/responses` is currently GPT-OSS-only and non-streaming, so coding-agent integration should use chat streaming rather than assume streaming Responses support. Native-model quality scores are not proof of Cloudflare-hosted coding performance; no paid inference or new model-comparison evaluation is required to implement this guard.

## NVIDIA NIM

**Do not enable a live NVIDIA credential before the exact-source release/compatibility gates and account entitlement checks pass.** NVIDIA advertises [free hosted APIs for development and prototyping](https://developer.nvidia.com/nim), not a universal monthly credit grant. The [official FAQ](https://forums.developer.nvidia.com/t/nvidia-nim-faq/300317) says rate limits vary by model and account; inspect the signed-in API Catalog rather than assuming 40 RPM or 1,000 credits. No authoritative remaining-credit API is used by this module.

The [NVIDIA API Trial Terms](https://assets.ngc.nvidia.com/products/api-catalog/legal/NVIDIA%20API%20Trial%20Terms%20of%20Service.pdf), sections 1.2 and 1.4, restrict trial access **and generated content** to internal testing/evaluation, not production. Section 2.6(a) prohibits confidential, controlled or sensitive input. Trial-generated code must not be promoted into a production product under these trial rights. Section 4.6 prohibits unsolicited automated bulk communications; API automation is not an exemption from the other restrictions. A separate NVIDIA or partner subscription is required for production or continued use after available credits. Model licenses and the [Technology Access Terms](https://developer.nvidia.com/legal/terms) also apply. Do not route confidential repositories, normal business production work or real end-user traffic to a free trial merely because a model is listed in the catalog.

A safe, initially blocked configuration is:

```yaml
openai-compatibility:
  - name: nvidia-nim
    base-url: https://integrate.api.nvidia.com/v1
    api-key-entries:
      - api-key: ${NVIDIA_NIM_API_KEY}  # illustrative; use the host's supported secret mechanism
    models:
      - {name: z-ai/glm-5.3-flash, alias: nvidia-nim/glm-5.3-flash}
plugins:
  configs:
    subscription-pool:
      providers:
        nvidia-nim:
          stayWithinFree: true
          free-tier-confirmed: false   # change only after verifying account/model entitlement and permitted use
          requests-per-minute: 0       # unknown: blocked in free-only mode; supply the verified conservative account/model cap
          free-models: []              # exact upstream model IDs verified free for this account
          free-request-limit: 0        # optional LOCAL lifetime admission ceiling, not a provider credit balance
          # trial-expires-at: "2026-11-01T00:00:00Z"  # only if this account actually has that expiry
```

The example `z-ai/glm-5.3-flash` was present in NVIDIA's public model catalog on 2026-10-24; catalog presence proves neither key validity, free entitlement nor production rights. For free-only admission, explicitly confirm the entitlement, configure a positive verified RPM cap, and allowlist each exact free upstream model ID. The requested alias must map only to free upstream models; ambiguous free/paid aliases are refused. The free endpoint must be exactly HTTPS `integrate.api.nvidia.com/v1` (optional trailing slash/port 443). A placeholder key, unknown credential, expired configured trial, exhausted local ceiling, observed credit exhaustion or unavailable durable state fails closed. All keys share one conservative budget; adding/replacing keys or adding aliases does not mint allowance.

`stayWithinFree: false` explicitly permits a separately authorized paid endpoint or paid models and bypasses only free-specific checks. It **does not purchase a plan, upgrade a trial, grant production rights or authorize a new charge**. Placeholder protection, durable accounting, configured RPM and upstream `Retry-After` remain enforced. `enabled: false` instead removes this module from scheduling and delegates to the host; it is **not** a billing guard.

Reservations are persisted before admission to `<auth-dir>/subscription-pool/providers/nvidia-nim/allowance.state`; failed/retried/interrupted requests count conservatively. Only the rolling 60-second RPM window refills. The optional lifetime ceiling and observed credit or trial exhaustion do not reset on a guessed day/month boundary. A provider 402, or a 429 whose body mentions credits or a trial, sets sticky free exhaustion. Ordinary RPM 429s also mention a quota, so a 429 that mentions a quota or a daily/monthly limit, but not credits or a trial, backs off for at least one hour, durably across restarts, without latching. Generic rate 429s use `Retry-After` (bounded to 24 hours; one minute if absent); a longer `Retry-After` always wins. Credit recovery is operator-controlled: re-verify the actual entitlement first; stop the relevant host, make a mode-0600 backup, and clear only `free_exhausted` in the retained state if warranted. Keep `reserved`/rate history; increase a local ceiling only by an explicitly verified amount. No automatic reset or reset API is provided. Deleting state or changing the auth directory loses the local guard history and must not be used to replenish allowance.

The usage page shows one **NVIDIA shared allowance** card, guard on/off, local reservations, the shared RPM meter and an optional lifetime ceiling. Every meter is labelled **local admissions, not provider balance**; no monthly credit bar, token-to-credit conversion or authoritative balance is invented. Other clients using the NVIDIA account are outside this proxy's observations; isolate the account or leave a conservative margin. Verified configuration is not a guarantee against account-side billing changes, so retain provider-side no-billing controls too.

Owner key instructions are in [`deploy/NVIDIA-OWNER-KEY.txt`](deploy/NVIDIA-OWNER-KEY.txt). [`deploy/nvidia-key-watch.py`](deploy/nvidia-key-watch.py) reads a private `~/secure-drop/nvidia.env` containing `KEY=...` and stages a mode-0600 JSON/YAML fragment only. It makes **no network requests**, does not activate credentials, refuses conflicting existing stages and never prints key material. `--watch` waits in that process; no timer is installed. Release operators must later GET, privately back up and merge the **full** management value, never PUT this fragment as a replacement configuration.


## Install

Release assets use one version consistently:

```text
subscription-pool-v0.7.1.so
subscription-pool_0.7.1_linux_amd64.zip
checksums.txt
```

### Install the shared library

Download the versioned shared library and `checksums.txt` from the same GitHub release, then verify before installation:

```sh
version=0.7.1
sha256sum --check checksums.txt
install -m 0755 "subscription-pool-v${version}.so" /path/to/cliproxy/plugins/linux/amd64/
```

### Install the plugin-store archive

Verify `checksums.txt`, then supply `subscription-pool_0.7.1_linux_amd64.zip` to the CLIProxyAPI plugin-store flow. The archive contains exactly one root-level entry named `subscription-pool.so`.

Use the host's supported reload procedure after changing the native plugin (or restart only when the tested host requires it). Confirm registration and all three capabilities before sending traffic. The release is not valid until CI passes the immutable host matrix: the deployed digest from [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json), the latest `eceasy/cli-proxy-api` tag resolved to its linux/amd64 manifest at run time, and the historical baseline from [`.github/host-images.json`](.github/host-images.json).

### Host compatibility matrix

The matrix is maintained in three places:

- the operator updates [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json) immediately after a host deployment, recording the exact linux/amd64 manifest digest;
- [`.github/host-images.json`](.github/host-images.json) keeps the schema-5-capable v7.2.150 baseline (moved from v7.2.67 under TOG-7425);
- [`.github/scripts/resolve-host-images.sh`](.github/scripts/resolve-host-images.sh) queries Docker Hub on every run and resolves the highest semantic v7.2.x tag to an immutable linux/amd64 manifest digest.

For every image, CI loads the plugin, checks registration and all three advertised capabilities, sends real requests through the host scheduler, and requires HTTP `200` round trips for `zai/smoke-model` and `zai-openai/smoke-model` against an isolated TLS stub. Any `zai_unmanaged_candidate`, scheduler rejection, missing upstream request, or non-200 response fails the job. [`.github/workflows/host-compatibility.yml`](.github/workflows/host-compatibility.yml) holds the check. [`ci.yml`](.github/workflows/ci.yml) runs it as the `host-compat` job on pull requests (skipped for docs-only changes), on `main`, and daily at 07:17 UTC; it can also be dispatched by hand. A failed scheduled run creates a GitHub Actions notification; branch protection should require `ci-ok`, which fails if the host compatibility job fails, plus `pr-lint` and `secret-scan`.

## Configure CLIProxyAPI credentials

### Z.ai Coding Plan

A single Z.ai plan key must appear in both provider sections. Pairing uses full-key equality in memory; a suffix is only a redacted display and override selector.

```yaml
claude-api-key:
  - api-key: ${ZAI_CODING_PLAN_KEY}
    base-url: https://api.z.ai/api/anthropic
    prefix: zai

openai-compatibility:
  - name: zai-coding-plan
    base-url: https://api.z.ai/api/coding/paas/v4
    api-key-entries:
      - api-key: ${ZAI_CODING_PLAN_KEY}
```

The environment placeholder is illustrative; use CLIProxyAPI's supported secret/config mechanism. Never commit a populated configuration file.

### OpenCode Go

Configure OpenCode Go's inference API-key credentials in CLIProxyAPI, then list their managed auth IDs under `providers.opencode-go.accounts`. Each account should carry its own `dashboard-api-key` for `/zen/go/v1/usage`; a module-wide key is appropriate only when the accounts genuinely share that dashboard credential. Missing quota stays unknown; do not reuse a sibling account's usage. See [`config.example.yaml`](config.example.yaml) for the account shape.

### synthetic.new

Use the paid and infinite OpenAI-compatible entries shown [above](#syntheticnew). The plugin discovers their API-key lanes; no OAuth files or additional credentials are required. Infinite-mode availability depends on the plan and requires streaming.

## Configure the plugin

Start from [`config.example.yaml`](config.example.yaml). Host fields (`enabled`, `priority`) and `cpa-config-path` belong under `plugins.configs.subscription-pool`. Provider settings belong under `providers.<module-id>` (`zai`, `opencode-go`, `synthetic`, `cloudflare-workers-ai` and `nvidia-nim`). Omit modules you do not use; do not copy Z.ai account overrides into a Synthetic-only installation.

```yaml
plugins:
  enabled: true
  dir: plugins
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
          state-retention: 192h
          default-plan: pro
        opencode-go:
          enabled: false  # enable only with your managed accounts configured
        synthetic:
          threshold-percent: 95
          quota-refresh-interval: 2m
          max-concurrent: 0
          infinite-max-concurrent: 0
```

The provider-specific settings below describe `providers.zai`; they are not defaults for the other modules.

| Setting | Default | Meaning |
|---|---:|---|
| `plugins.enabled` | `false` | Global native-plugin switch; set it to `true`. |
| `plugins.dir` | `plugins` | Native plugin root; Linux/amd64 libraries are loaded from `<dir>/linux/amd64/`. |
| `enabled` | host default | Per-plugin enable switch under `plugins.configs.subscription-pool`. |
| `priority` | `1000` required | Host-level scheduler priority. V0.1 also rejects any second enabled scheduler plugin. |
| `cpa-config-path` | `config.yaml` | CLIProxyAPI configuration used to discover exact credential pairs and `auth-dir`. |
| `coexisting-plugins` | `[]` | Plugin IDs you confirm register **no** Scheduler (provider-only plugins, e.g. `nvidia-nim`). Only these may be enabled alongside `subscription-pool`, and each must sort after it (priority below `1000`, or `1000` with a larger ID). Any other enabled plugin fails reconfigure closed. A provider plugin that serves the same provider ID as a pool module needs that module disabled (`providers.<id>.enabled: false`). |
| `quota-refresh-interval` | `2m` | Base poll interval; jittered polling accepts only `1m`–`3m`. |
| `authoritative-max-age` | `5m` | Maximum age before authoritative data becomes stale; must exceed maximum polling jitter. |
| `threshold-percent` | `97` | Either quota bucket reaching this percentage exhausts the account. |
| `suspend-duration` | `30m` | Conservative block after `401` or `403`. |
| `fallback-cooldown` | `10m` | Block after `429` when no trustworthy reset time is available. Z.ai request-rate 429s (codes `1302`/`1305`) first back off only the affected account for 5–60s, never longer than this value, and take this cooldown only once a sixth consecutive backoff step is needed. |
| `state-retention` | `192h` | Local estimator and deduplication retention; must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max` fallback plan. |
| `accounts` | `[]` | Required suffix selectors with optional names, plan overrides, custom buckets, and administrative disable state. |

A custom plan sets `plan: custom`, `five-hour-credits`, and `weekly-credits`. Suffixes must identify one full-key pair unambiguously; names must be unique.

A rejected configuration never leaves modules half-applied and keeps the guards on. The causes are a parse error, an unreadable `cpa-config-path`, an enabled plugin missing from `coexisting-plugins`, a bad `auth-dir`, or a provider setting a module refuses. Every module validates the new configuration before any of them applies it, and the plugin stays registered as the host scheduler. Until a valid configuration is applied, any pick with a credential a module recognizes, that the last accepted configuration owned, or that the CPA config gives Z.ai fails with `coordinator_config_rejected`; other providers' credentials are left to the host. If no configuration has been accepted since the plugin started, the plugin cannot tell which credentials it guards, so every pick fails with `coordinator_config_rejected`. The plugin status (and the rejecting module's own status) reports `reconfigure_rejected` with a `validation_error`. Fix the configuration and reload to recover.

Local state and non-secret settings are stored atomically under `<auth-dir>/subscription-pool/` (provider state under `providers/<module>/`, usage history under `usage/`). Directories are mode `0700`, files are mode `0600`, and provider keys are never persisted.

## Usage and Quota page

The plugin adds a **Usage and Quota** entry to the Management Center's plugin menu (`/v0/resource/plugins/subscription-pool/status`). It uses the same design as the official Dashboard and Quota Management pages:

- a headline verdict, requests handled with the success/failure split, and a traffic wave;
- provider tabs (All, Z.ai, OpenCode Go, synthetic.new, Cloudflare Workers AI, NVIDIA NIM), time ranges (1 h, 6 h, 24 h, 7 d), search, and a model filter;
- success rate, tokens, accounts and output-speed tiles;
- a throughput chart (requests or tokens per bucket) with a data table;
- every provider side by side, plus sortable per-account and per-model tables (requests, error rate, 429s, input/output/cached tokens, latency, time to first token, tokens per second, share);
- synthetic.new: the weekly credit with its 2%-per-202-minutes refill schedule and the time it is full again, infinite-mode requests and speed against paid, and in-flight, peak and cap per lane;
- Quota Management cards with remaining-percent bars, reset times and countdowns, and a two-week quota-window timeline.

The page follows the Management Center theme when it is served from the same origin. Its palette, type stacks and layout reproduce the [CLIProxyAPI Management Center](https://github.com/router-for-me/Cli-Proxy-API-Management-Center) design (MIT, Router-For.ME); no source code from that project is included. Usage history comes from the plugin's own ledger of the requests it schedules: 5-minute buckets for 48 hours and 1-hour buckets for 8 days, saved under the plugin's auth directory. The page and its `usage.json` data route are unauthenticated resource routes, like every CLIProxyAPI plugin page, so they carry only redacted fields: no key, key fragment, auth ID, client key, request content or upstream error text (reasons are closed codes; the free text stays on the authenticated status route).

## Management API

CLIProxyAPI mounts plugin routes under `/v0/management` and authenticates them with its management key. The plugin does not implement a second authentication scheme.

```sh
management_url=http://127.0.0.1:8317
curl --fail-with-body \
  -H "Authorization: Bearer ${CLIPROXY_MANAGEMENT_KEY}" \
  "${management_url}/v0/management/plugins/subscription-pool/status"
```

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v0/management/plugins/subscription-pool/usage` | Usage page data: requests, tokens and errors per provider, account and model for `?range=` (`1h`, `6h`, `24h` or `7d`), quota cards, and the synthetic.new panel. |
| `GET` | `/v0/management/plugins/subscription-pool/status` | Aggregated status and per-account quota for all configured provider modules, under `providers`. |
| `GET` | `/v0/management/plugins/zai-coding-plan/status` | Z.ai per-account quota, reset times, source, freshness, off-peak state, health, and integrity warnings. |
| `POST` | `/v0/management/plugins/zai-coding-plan/refresh` | Force a bounded Z.ai quota refresh. Body must be empty or `{}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/unblock` | Clear transient Z.ai blocks without erasing retained quota or usage. Optional JSON: `{"account":"name-or-suffix"}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/account-config` | Save or clear validated non-secret Z.ai account and polling settings. |

The `zai-coding-plan` route prefix above is the Z.ai module's existing compatibility API, not a second plugin registration or a config entry to enable. OpenCode Go and synthetic.new status is read from the aggregate `providers` map; they do not expose separate refresh/write routes.

The **Usage and Quota** resource entry at `/v0/resource/plugins/subscription-pool/status` displays the redacted multi-provider dashboard described above. Resource routes are intentionally unauthenticated: never put a management credential in the iframe or resource URL. Detailed diagnostics and write operations stay on authenticated management routes.

Status accounts include `five_hour_utilization`, `weekly_utilization`, `five_hour_resets_at`, `weekly_resets_at`, `quota_source`, `quota_observed_at`, `quota_age_seconds`, `quota_stale`, `offpeak`, `health`, and bounded integrity-warning fields. They also expose the existing stable account pseudonym as `identity` and an explicit transient-429 `cooldown` object. These are restricted management metadata, not public telemetry or CPA auth IDs. See the [producer/consumer status contract](docs/STATUS-CONTRACT.md) for redaction, provenance, freshness, expiry, and refusal semantics. No raw key or raw key suffix is exposed. OpenCode Go window resets are not this cooldown contract.

`account-config` requires an `account` name or suffix. It accepts non-secret fields such as `name`, `plan`, `disabled`, `five_hour_credits`, `weekly_credits`, `threshold_percent`, `polling_interval`, `authoritative_max_age`, and `timeout`; `{"account":"...","clear":true}` restores the base configuration for that account.

## Build and verify

```sh
make fmt-check
make vet
make test
make lint
make test-release
make validate-source
make package VERSION=0.7.1
make validate-release VERSION=0.7.1 TAG=v0.7.1
(cd dist && sha256sum --check checksums.txt)
```

`make package` builds a versioned `.so`, creates the plugin-store zip, writes SHA-256 checksums, and fails if an expected artifact is empty. CI additionally checks the exported `cliproxy_plugin_init` symbol, archive contents, source secret scan, and license/notice requirements.

## Release policy

A release tag is created only after all implementation slices are merged and the exact release commit has:

1. green formatting, vet, race-test, lint, build, packaging, secret-scan, and license/notice checks;
2. machine-checked tag, binary, archive, registry, changelog, and checksum consistency;
3. the immutable host compatibility matrix green for both release-required rows (the operator-reported deployed digest and the latest Docker Hub tag), with the supported v7.2.150 baseline also exercised; each row must advertise scheduler, usage, and management capabilities, return management `401`/redacted `200`, and serve both prefixed inference paths through a real scheduler pick;
4. an exact-SHA code review; and
5. a separate exact-SHA security review for credential handling and management operations.

The release tag must point to that reviewed commit after it is merged to `main`. The tag workflow validates strict semantic-version syntax and proves the event commit is reachable from `origin/main` before any repository build or packaging command receives the version. Tags trigger the release workflow. The release workflow runs the host matrix in a separate `host-matrix` job (third-party host binaries run as root there) and `publish` only uploads artifacts whose SHA-256 digest matches the one `build` recorded before that job ran. Do not publish locally built artifacts or move an existing tag.

## Security and scope

Never commit, print, or return plan keys, authorization headers, management credentials, raw request bodies, or key hashes. Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).

Z.AI Coding Plan access is for supported coding tools; Claude Code is a supported tool and this project's intended agent workloads are Claude Code sessions. This plugin must not turn the subscription into a public inference or resale service.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md), [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md), [`CHANGELOG.md`](CHANGELOG.md), [`SECURITY.md`](SECURITY.md), [`NOTICE`](NOTICE), and [`LICENSE`](LICENSE).
