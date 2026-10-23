# Contributing

Thank you for improving `cpa-plugin-subscription-pool`.

## Before you start

- Search existing issues before opening a new one.
- Use a branch and pull request; never push directly to `main`.
- Keep changes focused and use Conventional Commit messages.
- Read [`docs/ARCHITECTURE.md`](docs/ARCHITECTURE.md) before changing ABI, pairing, quota, scheduler, persistence, or management behavior.
- Use placeholders in tests and examples. Never commit or paste a live credential.

## Development environment

You need Linux/amd64, Go 1.26, a GCC-compatible C toolchain, and `golangci-lint` v2.13.2.

```sh
go mod download
make fmt-check
make vet
make test
make lint
make build VERSION=0.1.0-dev
```

Use an injected clock and deterministic fixtures. Unit tests must not depend on wall-clock sleeps, live Z.AI accounts, or mutable container tags.

## Pull requests

A pull request should:

- explain the user-visible behavior and operational impact;
- include tests for success and failure paths;
- update `README.md`, `config.example.yaml`, and `CHANGELOG.md` when behavior changes;
- preserve the collector-facing management field names;
- keep all credential material out of logs, status, state, and fixtures; and
- identify the exact immutable CLIProxyAPI image digest used by any compatibility claim.

Before requesting review, run:

```sh
make fmt-check
make vet
make test
make lint
make package VERSION=0.1.0
(cd dist && sha256sum --check checksums.txt)
```

CI also runs race tests, validates the exported `cliproxy_plugin_init` symbol, checks archive contents and release metadata, scans for likely secrets, and checks required license/notices.

Docs-only pull requests (only `docs/**` or Markdown files, excluding `CHANGELOG.md`, `deploy/README.md` and anything under `.github/`) skip the Quality, build and host-compatibility jobs. The secret scan and `pr-lint` always run, and the `ci-ok` check aggregates everything. Any other change, including `go.mod`, `go.sum`, `Makefile` and workflow edits, runs the full suite, as does every push to `main` and the nightly run.

## Review requirements

Every change needs a review bound to the exact head SHA. Changes touching credentials, quota authorization, local persistence, management routes, or release trust require a separate exact-SHA security review.

Review approval is invalidated by a new commit. Address findings on the branch and request review again at the new SHA.

## Releases

Do not create or move tags manually as part of a feature pull request. Release authority creates `vMAJOR.MINOR.PATCH` only after the exact commit passes all CI, image-load, authenticated-status, normal-review, and security-review gates. GitHub Actions builds and publishes the artifacts; locally built files are never release artifacts.

## Community

Be respectful and follow [`CODE_OF_CONDUCT.md`](CODE_OF_CONDUCT.md). Report vulnerabilities privately under [`SECURITY.md`](SECURITY.md), not in public issues.
