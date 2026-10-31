# Security Policy

## Supported versions

Before the first stable release, only the latest tagged version receives security fixes. A version is supported only when its assets and SHA-256 checksums were produced by the repository's release workflow.

| Version | Supported |
|---|---|
| Latest tagged release | Yes |
| Older or untagged builds | No |

## Reporting a vulnerability

Use GitHub's private vulnerability reporting flow from the repository's **Security** tab. Do not open a public issue, discussion, or pull request containing exploit details or credentials.

Include, when safe:

- the affected release and asset checksum;
- the CLIProxyAPI image digest and configuration shape;
- reproduction steps using placeholders or revoked test material; and
- the expected and observed security boundary.

Do not include a live Z.AI plan key, CLIProxyAPI management key, authorization header, private configuration, or raw request body. If sensitive evidence is required, first establish the private advisory channel.

## Security boundaries

- A plan key is paired in memory across the Anthropic and OpenAI-compatible CLIProxyAPI entries. It must never appear in status, logs, errors, filenames, persistent state, or tests.
- Plugin state is stored under `<auth-dir>/subscription-pool/` (Z.ai state in `providers/zai/`); directories must be mode `0700` and files mode `0600`. Atomic replacements must retain those permissions and reject symlink targets.
- Z.AI quota polling uses the corresponding plan key only for the documented HTTPS quota endpoint. Responses and errors are bounded and treated as untrusted input.
- Management endpoints are mounted beneath `/v0/management` and rely on CLIProxyAPI's management-key authentication. The **Usage and Quota** resource routes (`/v0/resource/plugins/subscription-pool/status` and `usage.json`) are unauthenticated by host design and serve only redacted usage data: no keys, fingerprints, auth IDs or free-text errors.
- `unblock` clears transient health flags but must recompute retained quota/usage and cannot create capacity.
- Native `.so` files execute inside CLIProxyAPI. Install only assets from a reviewed tagged release after checking `checksums.txt`.

## Release security gate

Credential handling and management changes require an independent security review bound to the exact release commit SHA. The release workflow also runs the repository's secret and license/notice checks. A later commit invalidates the earlier review.

Security fixes follow the same pull-request and release process. Maintainers may withhold details until a corrected release is available.
