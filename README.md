# Subscription Pool: Z.ai · OpenCode Go · synthetic.new

<img src="src/logo.svg" alt="Subscription Pool logo" width="64" height="64">

`subscription-pool` is a native Linux/amd64 plugin for [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) that schedules flat-rate subscription credentials so they are used fully without hitting provider limits. One coordinator holds CLIProxyAPI's scheduler slot and hosts one module per provider:

| Module | Provider | What it does |
|---|---|---|
| `zai` | Z.AI Coding Plan | pairs each key's Anthropic and OpenAI-compatible credentials, tracks the five-hour and weekly quota, keeps impaired accounts out of scheduling |
| `opencode-go` | OpenCode Go | quota-aware account pooling from the usage dashboard and observed 429s |
| `synthetic` | synthetic.new | weekly-credit threshold scheduling, optional concurrency caps, and automatic fallback to the free infinite mode ([details](#syntheticnew)) |

Modules with nothing configured are inactive and leave their providers to CLIProxyAPI's own scheduler. This repository was previously named `cpa-plugin-zai-coding-plan`.

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
- CLIProxyAPI v7.2.x with native plugin ABI version 1 and plugin schema version 5 (SDK v7.2.154; v7.2.67 hosts accept only schema 1 and cannot load this plugin)
- exactly one enabled scheduler plugin, with `subscription-pool` configured at priority `1000`
- credentials for at least one of Z.ai Coding Plan, OpenCode Go or synthetic.new, used under that provider's subscription terms (unconfigured modules remain inactive)
- Go 1.26 and a GCC-compatible C toolchain only when building from source

The supported host range is CLIProxyAPI v7.2.150 through the newest `eceasy/cli-proxy-api` v7.2.x tag that passes the compatibility matrix. CI always tests the v7.2.150 baseline, the operator-recorded deployed image, and the latest published tag. See [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) for the pinned SDK, ABI, scheduler-exclusivity, persistence, and threat-model contracts.

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
- Weekly usage comes from `GET /v2/quotas` and the `x-synthetic-quotas` response header and is projected through the documented regeneration (2% every 202 minutes). Placeholder keys (for example `<API KEY PLACEHOLDER>`) are never sent anywhere; the lanes show `health: unbound` until a real key is configured.
- `GET /v0/management/plugins/subscription-pool/status` -> `providers.synthetic.accounts[].stats` reports measured output tokens per second and time to first token per lane, so infinite-mode speed can be compared with the paid lane.

## Install

Release assets use one version consistently:

```text
subscription-pool-v0.6.1.so
subscription-pool_0.6.1_linux_amd64.zip
checksums.txt
```

### Install the shared library

Download the versioned shared library and `checksums.txt` from the same GitHub release, then verify before installation:

```sh
version=0.6.1
sha256sum --check checksums.txt
install -m 0755 "subscription-pool-v${version}.so" /path/to/cliproxy/plugins/linux/amd64/
```

### Install the plugin-store archive

Verify `checksums.txt`, then supply `subscription-pool_0.6.1_linux_amd64.zip` to the CLIProxyAPI plugin-store flow. The archive contains exactly one root-level entry named `subscription-pool.so`.

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

Start from [`config.example.yaml`](config.example.yaml). Host fields (`enabled`, `priority`) and `cpa-config-path` belong under `plugins.configs.subscription-pool`. Provider settings belong under `providers.zai`, `providers.opencode-go` and `providers.synthetic`. Omit modules you do not use; do not copy Z.ai account overrides into a Synthetic-only installation.

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
| `quota-refresh-interval` | `2m` | Base poll interval; jittered polling accepts only `1m`–`3m`. |
| `authoritative-max-age` | `5m` | Maximum age before authoritative data becomes stale; must exceed maximum polling jitter. |
| `threshold-percent` | `97` | Either quota bucket reaching this percentage exhausts the account. |
| `suspend-duration` | `30m` | Conservative block after `401` or `403`. |
| `fallback-cooldown` | `10m` | Block after `429` when no trustworthy reset time is available. Z.ai request-rate 429s (codes `1302`/`1305`) first back off only the affected account for 5–60s, never longer than this value, and take this cooldown only once a sixth consecutive backoff step is needed. |
| `state-retention` | `192h` | Local estimator and deduplication retention; must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max` fallback plan. |
| `accounts` | `[]` | Required suffix selectors with optional names, plan overrides, custom buckets, and administrative disable state. |

A custom plan sets `plan: custom`, `five-hour-credits`, and `weekly-credits`. Suffixes must identify one full-key pair unambiguously; names must be unique.

Local state and non-secret settings are stored atomically under `<auth-dir>/subscription-pool/` (provider state under `providers/<module>/`, usage history under `usage/`). Directories are mode `0700`, files are mode `0600`, and provider keys are never persisted.

## Usage and Quota page

The plugin adds a **Usage and Quota** entry to the Management Center's plugin menu (`/v0/resource/plugins/subscription-pool/status`). It uses the same design as the official Dashboard and Quota Management pages:

- a headline verdict, requests handled with the success/failure split, and a traffic wave;
- provider tabs (All, Z.ai, OpenCode Go, synthetic.new), time ranges (1 h, 6 h, 24 h, 7 d), search, and a model filter;
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
| `GET` | `/v0/management/plugins/subscription-pool/status` | Aggregated status and per-account quota for all three modules, under `providers`. |
| `GET` | `/v0/management/plugins/zai-coding-plan/status` | Z.ai per-account quota, reset times, source, freshness, off-peak state, health, and integrity warnings. |
| `POST` | `/v0/management/plugins/zai-coding-plan/refresh` | Force a bounded Z.ai quota refresh. Body must be empty or `{}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/unblock` | Clear transient Z.ai blocks without erasing retained quota or usage. Optional JSON: `{"account":"name-or-suffix"}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/account-config` | Save or clear validated non-secret Z.ai account and polling settings. |

The `zai-coding-plan` route prefix above is the Z.ai module's existing compatibility API, not a second plugin registration or a config entry to enable. OpenCode Go and synthetic.new status is read from the aggregate `providers` map; they do not expose separate refresh/write routes.

The **Usage and Quota** resource entry at `/v0/resource/plugins/subscription-pool/status` displays the redacted three-provider dashboard described above. Resource routes are intentionally unauthenticated: never put a management credential in the iframe or resource URL. Detailed diagnostics and write operations stay on authenticated management routes.

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
make package VERSION=0.6.1
make validate-release VERSION=0.6.1 TAG=v0.6.1
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

The release tag must point to that reviewed commit after it is merged to `main`. The tag workflow validates strict semantic-version syntax and proves the event commit is reachable from `origin/main` before any repository build or packaging command receives the version. Tags trigger the release workflow. Do not publish locally built artifacts or move an existing tag.

## Security and scope

Never commit, print, or return plan keys, authorization headers, management credentials, raw request bodies, or key hashes. Report vulnerabilities privately as described in [`SECURITY.md`](SECURITY.md).

Z.AI Coding Plan access is for supported coding tools; Claude Code is a supported tool and this project's intended agent workloads are Claude Code sessions. This plugin must not turn the subscription into a public inference or resale service.

## Contributing

See [`CONTRIBUTING.md`](CONTRIBUTING.md), [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md), [`CHANGELOG.md`](CHANGELOG.md), [`SECURITY.md`](SECURITY.md), [`NOTICE`](NOTICE), and [`LICENSE`](LICENSE).
