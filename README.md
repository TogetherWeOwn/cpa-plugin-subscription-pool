# Subscription Pool for CLIProxyAPI

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

Quota data comes from Z.AI's plan endpoint when available. The token-based credit formula is a conservative fallback when that endpoint cannot be read. Status reports whether quota is authoritative or estimated and includes freshness and off-peak fields.

## Requirements

- Linux amd64
- CLIProxyAPI v7.2.x with native plugin ABI version 1 and plugin schema version 5 (SDK v7.2.154; v7.2.67 hosts accept only schema 1 and cannot load this plugin)
- exactly one enabled scheduler plugin, with `zai-coding-plan` configured at priority `1000`
- a Z.AI Coding Plan used through supported coding tools
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
zai-coding-plan-v0.2.0.so
zai-coding-plan_0.2.0_linux_amd64.zip
checksums.txt
```

### Install the shared library

Download the versioned shared library and `checksums.txt` from the same GitHub release, then verify before installation:

```sh
version=0.2.0
sha256sum --check checksums.txt
install -m 0755 "zai-coding-plan-v${version}.so" /path/to/cliproxy/plugins/linux/amd64/
```

### Install the plugin-store archive

Verify `checksums.txt`, then supply `zai-coding-plan_0.2.0_linux_amd64.zip` to the CLIProxyAPI plugin-store flow. The archive contains exactly one root-level entry named `zai-coding-plan.so`.

Restart CLIProxyAPI after changing the native plugin. Confirm registration and all three capabilities before sending traffic. The release is not valid until CI passes the immutable host matrix: the deployed digest from [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json), the latest `eceasy/cli-proxy-api` tag resolved to its linux/amd64 manifest at run time, and the historical baseline from [`.github/host-images.json`](.github/host-images.json).

### Host compatibility matrix

The matrix is maintained in three places:

- the operator updates [`deploy/deployed-host-image.json`](deploy/deployed-host-image.json) immediately after a host deployment, recording the exact linux/amd64 manifest digest;
- [`.github/host-images.json`](.github/host-images.json) keeps the schema-5-capable v7.2.150 baseline (moved from v7.2.67 under TOG-7425);
- [`.github/scripts/resolve-host-images.sh`](.github/scripts/resolve-host-images.sh) queries Docker Hub on every run and resolves the highest semantic v7.2.x tag to an immutable linux/amd64 manifest digest.

For every image, CI loads the plugin, checks registration and all three advertised capabilities, sends real requests through the host scheduler, and requires HTTP `200` round trips for `zai/smoke-model` and `zai-openai/smoke-model` against an isolated TLS stub. Any `zai_unmanaged_candidate`, scheduler rejection, missing upstream request, or non-200 response fails the job. [`.github/workflows/host-compatibility.yml`](.github/workflows/host-compatibility.yml) holds the check. [`ci.yml`](.github/workflows/ci.yml) runs it as the `host-compat` job on pull requests (skipped for docs-only changes), on `main`, and daily at 07:17 UTC; it can also be dispatched by hand. A failed scheduled run creates a GitHub Actions notification; branch protection should require `ci-ok`, which fails if the host compatibility job fails, plus `pr-lint` and `secret-scan`.

## Configure CLIProxyAPI credentials

A single plan key must appear in both provider sections. Pairing uses full-key equality in memory; a suffix is only a redacted display and override selector.

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

## Configure the plugin

Start from [`config.example.yaml`](config.example.yaml). Plugin settings belong directly under `plugins.configs.zai-coding-plan`; `enabled` and `priority` are host fields at the same level.

```yaml
plugins:
  enabled: true
  dir: plugins
  configs:
    zai-coding-plan:
      enabled: true
      priority: 1000
      cpa-config-path: /app/config.yaml
      quota-refresh-interval: 2m
      authoritative-max-age: 5m
      threshold-percent: 97
      suspend-duration: 30m
      fallback-cooldown: 10m
      state-retention: 8d
      default-plan: pro
      accounts:
        - key-suffix: replace-with-unique-suffix
          name: zai-pro-1
          plan: pro
          disabled: false
```

| Setting | Default | Meaning |
|---|---:|---|
| `plugins.enabled` | `false` | Global native-plugin switch; set it to `true`. |
| `plugins.dir` | `plugins` | Native plugin root; Linux/amd64 libraries are loaded from `<dir>/linux/amd64/`. |
| `enabled` | host default | Per-plugin enable switch under `plugins.configs.zai-coding-plan`. |
| `priority` | `1000` required | Host-level scheduler priority. V0.1 also rejects any second enabled scheduler plugin. |
| `cpa-config-path` | `config.yaml` | CLIProxyAPI configuration used to discover exact credential pairs and `auth-dir`. |
| `quota-refresh-interval` | `2m` | Base poll interval; jittered polling accepts only `1m`–`3m`. |
| `authoritative-max-age` | `5m` | Maximum age before authoritative data becomes stale; must exceed maximum polling jitter. |
| `threshold-percent` | `97` | Either quota bucket reaching this percentage exhausts the account. |
| `suspend-duration` | `30m` | Conservative block after `401` or `403`. |
| `fallback-cooldown` | `10m` | Block after `429` when no trustworthy reset time is available. Z.ai request-rate 429s (codes `1302`/`1305`) first back off only the affected account for 5–60s, never longer than this value, and take this cooldown only once a sixth consecutive backoff step is needed. |
| `state-retention` | `8d` | Local estimator and deduplication retention; must exceed one week. |
| `default-plan` | none | Optional `lite`, `pro`, or `max` fallback plan. |
| `accounts` | `[]` | Required suffix selectors with optional names, plan overrides, custom buckets, and administrative disable state. |

A custom plan sets `plan: custom`, `five-hour-credits`, and `weekly-credits`. Suffixes must identify one full-key pair unambiguously; names must be unique.

Local state and non-secret settings are stored atomically under `<auth-dir>/zai-coding-plan/`. The directory is mode `0700`, files are mode `0600`, and provider keys are never persisted.

## Management API

CLIProxyAPI mounts plugin routes under `/v0/management` and authenticates them with its management key. The plugin does not implement a second authentication scheme.

```sh
management_url=http://127.0.0.1:8317
curl --fail-with-body \
  -H "Authorization: Bearer ${CLIPROXY_MANAGEMENT_KEY}" \
  "${management_url}/v0/management/plugins/zai-coding-plan/status"
```

| Method | Path | Purpose |
|---|---|---|
| `GET` | `/v0/management/plugins/zai-coding-plan/status` | Redacted per-account quota, reset times, source, freshness, off-peak state, health, and integrity warnings. |
| `POST` | `/v0/management/plugins/zai-coding-plan/refresh` | Force a bounded quota refresh for configured accounts. Body must be empty or `{}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/unblock` | Clear transient blocks without erasing retained quota or usage. Optional JSON: `{"account":"name-or-suffix"}`. |
| `POST` | `/v0/management/plugins/zai-coding-plan/account-config` | Save or clear validated non-secret account and polling settings. |

The plugin also registers a **Z.ai Quota** resource entry at `/v0/resource/plugins/zai-coding-plan/status`, so current Management Center builds place Z.ai in the left navigation. CLIProxyAPI resource routes are intentionally unauthenticated; the resource page therefore contains no quota payload and no management credential. Detailed quota remains on the authenticated status endpoint above until the Management Center accepts native plugin-backed Quota-page cards.

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
make package VERSION=0.2.0
make validate-release VERSION=0.2.0 TAG=v0.2.0
(cd dist && sha256sum --check checksums.txt)
```

`make package` builds a versioned `.so`, creates the plugin-store zip, writes SHA-256 checksums, and fails if an expected artifact is empty. CI additionally checks the exported `cliproxy_plugin_init` symbol, archive contents, source secret scan, and license/notice requirements.

## Release policy

A v0.2.0 tag is created only after all implementation slices are merged and the exact release commit has:

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
