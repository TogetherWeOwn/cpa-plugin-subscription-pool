# OpenCode Go module provenance

This module was written from scratch for the clean-room OpenCode Go slice.

Implementation inputs were limited to:

- the first-party fixture corpus's `spec/OBSERVED-BEHAVIOR.md` and its cited Case D01, I01, I02, and I03 files;
- the merged `internal/providermodule.Module` interface and coordinator contracts in this repository at `7272eec45925`;
- the pinned CLIProxyAPI SDK public types used by that interface.

No third-party OpenCode Go plugin source tree, source file, test, fixture, identifier sequence, or prose was opened, cloned, read, adapted, or transcribed while implementing this module.

Observed behavior used by the implementation:

- `spec/OBSERVED-BEHAVIOR.md` §2a / Case I01 and I03: a 429 body names the governing quota window and `Retry-After` carries the matching reset countdown;
- `spec/OBSERVED-BEHAVIOR.md` §2b / Case I02: the 403 doctype response with a 1–3 second countdown is a transient microthrottle and does not modify quota-window pacing state;
- `spec/OBSERVED-BEHAVIOR.md` §4 / Case I02: proxy-visible connection IDs can be grouped as one configured logical account when supplied by the operator;
- `spec/OBSERVED-BEHAVIOR.md` §1 / Case D01: the module does not depend on an authenticated dashboard payload.

The five-hour and weekly enforcement shapes were not observed. They are deliberately limited to the issue-mandated structural generalization of the observed monthly pattern: a 429 that explicitly names the window plus a bounded reset hint. The module status exposes this limitation. When a quota response has no reset evidence, the module keeps `resets_at` unknown and uses a separate conservative impairment cooldown; it does not publish the cooldown as a reset timestamp. Unobserved utilization is likewise omitted rather than reported as zero.

Review gate:

```sh
git grep -iE '<prohibited-author>|<prohibited-upstream-repository>' -- internal/providers/opencodego
```

The reviewer substitutes the prohibited names from the issue's binding clean-room gate. The command must return no matches. This gate covers `quota.go` and `quota_test.go`, added below, the same as every other file in this module.

## 2026-09-13 — Usage poller (TOG-2458)

The owner directed (verbatim): *"FOR THE OPENCODE GO CAN YOU NOT USE THE COOKIE AUTH TOKEN TO GET THE USAGE AND RESET TIMES? YOU SHOULD BE ABLE TO. LOOK AT HOW THE OLD OPENCODE-GO CLIPROXY PLUGIN DID IT AND IMPLEMENT OUR OWN VERSION OF THAT IN OUR PLUGIN."* This is a narrow, explicit carve-out on top of the standing 2026-09-10 clean-room rule ("do not reuse code from the other opencode-go plugin... we create this from scratch"): it authorizes *studying* the old plugin's endpoint, auth flow, and response shape for this slice only. It does not authorize opening, reading, or transcribing any of that plugin's source, structure, or identifiers, and no such source was opened, cloned, read, adapted, or transcribed while implementing this poller.

Instead, the endpoint, auth mechanism, and response envelope were established from two first-party, unrestricted sources:

- our own production OmniRoute TypeScript source `open-sse/services/opencodeQuotaFetcher.ts`, which already implements and validates a Bearer-token-authenticated `GET https://opencode.ai/zen/go/v1/usage` fetcher for this exact provider;
- the first-party fixture corpus's TOG-2288 Case D01, which independently captured a live 401 at that exact URL with a Bearer-shaped error body (`{"type":"error","error":{"type":"AuthError","message":"Missing API key."}}`), confirming the endpoint and auth mechanism empirically.

The response envelope's field names (`usage.rolling/weekly/monthly`, each `{status, percent, resetsAt}`) come from `opencodeQuotaFetcher.ts` alone -- they were never independently fixture-observed for this module. This is a narrow, owner-approved exception to the general fixture-only evidentiary bar: the *shape* is taken from our own first-party TS source, not the restricted third-party plugin.

Window mapping: `rolling` -> `five_hour`, `weekly` -> `weekly`, `monthly` -> `monthly`.

The poller is on-demand (triggered from `Status()`, throttled to once per 30s) rather than a background goroutine: the endpoint is account-agnostic (one Bearer key returns all three windows for the whole credential, not per-account), and `Pick`/`HandleUsage` never need synchronous access to live poll data -- both already operate off cached `windowState`, exactly as they did before this poller existed. Freshness is therefore bounded by how often something calls `Status()`; in production that is the (separate, follow-on) collector script that polls the coordinator's management endpoint on an interval, the same way it already does for `zai`.

No credential is bound in any current sandbox. The module fails closed: an empty `dashboard-api-key` is valid configuration, not an error, and every window reports `known:false` until either a 429 is observed or a poll succeeds.

## 2026-09-13 — Per-account dashboard key (TOG-2472)

The initial poller above bound one `dashboard-api-key` to the whole module and polled it once per `Status()` call, writing that single snapshot into every configured account. This was wrong: `/zen/go/v1/usage` reports the usage of the credential making the request, not of "the module" -- three Go accounts with three different keys have three different usages, so broadcasting one account's poll result to all of them silently misreported the other two (confirmed live: go-1/go-2/go-3 all showed identical numbers on install while each account's own key returned its own real, different numbers on direct request).

Fixed by moving `dashboard-api-key` onto `accounts[]` (falling back to the module-level key when an account omits its own, for the common case where accounts genuinely share one key) and polling each account with its own key, concurrently, each throttled independently by `minPollInterval`. One account's poll failure or missing key no longer affects any other account's result.

## 2026-10-31 — Background poller (TWO-335 M2)

Polling from `Status()` meant freshness depended on someone viewing status: with nobody viewing it, `threshold-percent` was never enforced, and an exhausted poll reading stayed in place past the window's reset. The unauthenticated usage resource also reaches `Status()`, so any caller could trigger outbound polls carrying the dashboard key.

The module now starts a background poller on `Reconfigure` when an account has a dashboard key, polling every `quota-refresh-interval` (default `2m`, at least `30s`, `"0"` turns it off), and stops it in `Close`. `Status()` only reads cached state. A window whose reset time has passed is forgotten, so an exhausted or over-threshold reading never parks an account past its reset.
