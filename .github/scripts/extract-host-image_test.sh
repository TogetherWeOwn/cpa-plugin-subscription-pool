#!/usr/bin/env bash
# Offline acceptance tests for extract-host-image.sh fetch reliability.
#
# A fake `curl` is put first on PATH. It serves fixed token, manifest and
# layer fixtures and can inject `curl: (35) Recv failure: Connection reset
# by peer` at exactly one phase. No network, registry or host binary is used.
#
# How the fake models curl retries: plain `--retry N` does not retry error
# 35 in real curl (by default it retries only timeouts and a listed set of
# HTTP response codes), so the fake also does not repeat an attempt inside
# one invocation for `--retry` on its own. It repeats inside one invocation
# only for `--retry N --retry-all-errors`. An external shell retry loop is
# modelled naturally, as one counted attempt per fake invocation. Either
# shape of repair therefore has to count the same total attempts, and a
# repair that would not retry error 35 against real curl fails here too.
#
# Injected resets happen before any response body bytes, matching an SSL
# connect failure. Mid-body partial transfers are not modelled.
#
# stdout contract: run-host-matrix.sh captures the extractor's whole stdout
# as the host binary path, so on success stdout must be exactly that path
# and diagnostics must go elsewhere.
#
# Run from anywhere: .github/scripts/extract-host-image_test.sh
set -uo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." >/dev/null && pwd)
extractor="$root/.github/scripts/extract-host-image.sh"
repository='example/fixture-host'
# A repair must survive this many consecutive resets at a single phase.
transient_failures=2
# Upper bound proving a persistent failure gives up instead of looping.
max_attempts_allowed=20
timeout_seconds=90

pass_count=0
fail_count=0

pass() {
  printf 'PASS: %s\n' "$1"
  pass_count=$((pass_count + 1))
}

fail() {
  printf 'FAIL: %s: %s\n' "$1" "$2"
  fail_count=$((fail_count + 1))
}

fatal() {
  printf 'FATAL: %s\n' "$1" >&2
  exit 2
}

for tool in jq sha256sum tar timeout; do
  command -v "$tool" >/dev/null 2>&1 || fatal "required tool not found: $tool"
done
[[ -x "$extractor" ]] || fatal "extractor is not executable: $extractor"

work=$(mktemp -d) || fatal 'mktemp failed'
trap 'rm -rf "$work"' EXIT

fake_bin="$work/fake-bin"
mkdir -p "$fake_bin" || fatal 'cannot create fake bin directory'
cat >"$fake_bin/curl" <<'FAKE_CURL'
#!/usr/bin/env bash
set -uo pipefail

state_dir=${FAKE_CURL_STATE_DIR:?FAKE_CURL_STATE_DIR is required}
fixture_dir=${FAKE_CURL_FIXTURE_DIR:?FAKE_CURL_FIXTURE_DIR is required}
fail_phase=${FAKE_CURL_FAIL_PHASE:-none}
fail_times=${FAKE_CURL_FAIL_TIMES:-0}
mkdir -p "$state_dir"

url=''
out_file=''
write_out=''
retry=0
retry_all=0
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  arg=${args[i]}
  case "$arg" in
    --retry) retry=${args[i + 1]:-0} ;;
    --retry=*) retry=${arg#--retry=} ;;
    --retry-all-errors) retry_all=1 ;;
    -o | --output) out_file=${args[i + 1]:-} ;;
    --output=*) out_file=${arg#--output=} ;;
    -o?*) out_file=${arg#-o} ;;
    -w | --write-out) write_out=${args[i + 1]:-} ;;
    --write-out=*) write_out=${arg#--write-out=} ;;
    http://* | https://*) url=$arg ;;
  esac
done
case "$retry" in
  '' | *[!0-9]*) retry=0 ;;
esac

case "$fail_times" in
  always) fail_limit=999999 ;;
  '' | *[!0-9]*) fail_limit=0 ;;
  *) fail_limit=$fail_times ;;
esac

case "$url" in
  *auth.docker.io/token*)
    phase=token
    fixture="$fixture_dir/token.json"
    ;;
  */manifests/*)
    phase=manifest
    fixture="$fixture_dir/manifest.json"
    ;;
  */blobs/*)
    phase=blob
    fixture="$fixture_dir/layer.tar.gz"
    ;;
  *)
    printf 'curl: (6) fake curl: unsupported URL\n' >&2
    exit 6
    ;;
esac

max_internal=1
if ((retry_all == 1 && retry > 0)); then
  max_internal=$((retry + 1))
fi

attempt_once() {
  local counter_file="$state_dir/$phase.attempts"
  local n=0
  if [[ -f "$counter_file" ]]; then
    n=$(cat "$counter_file")
    case "$n" in
      '' | *[!0-9]*) n=0 ;;
    esac
  fi
  n=$((n + 1))
  printf '%s\n' "$n" >"$counter_file"

  if [[ "$phase" == "$fail_phase" ]] && ((n <= fail_limit)); then
    printf 'curl: (35) Recv failure: Connection reset by peer\n' >&2
    return 35
  fi

  if [[ -n "$out_file" ]]; then
    cat "$fixture" >"$out_file" || return 23
  else
    cat "$fixture" || return 23
  fi
  case "$write_out" in
    *'%{http_code}'*) printf '200' ;;
  esac
  return 0
}

status=0
for ((internal = 1; internal <= max_internal; internal++)); do
  if attempt_once; then
    exit 0
  else
    status=$?
  fi
done
exit "$status"
FAKE_CURL
chmod 0755 "$fake_bin/curl" || fatal 'cannot make fake curl executable'

# Fixture payload: one layer holding an executable host with known bytes.
payload_src="$work/payload"
expected_host="$payload_src/CLIProxyAPI/CLIProxyAPI"
mkdir -p "$payload_src/CLIProxyAPI" || fatal 'cannot create payload directory'
cat >"$expected_host" <<'HOST'
#!/bin/sh
printf 'fixture-host\n'
HOST
chmod 0755 "$expected_host" || fatal 'cannot make payload host executable'

base_fixture="$work/fixtures-base"
mkdir -p "$base_fixture" || fatal 'cannot create fixture directory'
tar -czf "$base_fixture/layer.tar.gz" -C "$payload_src" CLIProxyAPI ||
  fatal 'cannot create layer fixture'
layer_digest_hex=$(sha256sum "$base_fixture/layer.tar.gz" | cut -d' ' -f1)
layer_size=$(stat -c%s "$base_fixture/layer.tar.gz")
jq -cn \
  --arg digest "sha256:${layer_digest_hex}" \
  --argjson size "$layer_size" \
  '{schemaVersion: 2,
    mediaType: "application/vnd.oci.image.manifest.v1+json",
    layers: [{mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
              digest: $digest, size: $size}]}' \
  >"$base_fixture/manifest.json" || fatal 'cannot create manifest fixture'
manifest_digest_hex=$(sha256sum "$base_fixture/manifest.json" | cut -d' ' -f1)

# Runtime-only sentinel used as the bearer token value. It must never be
# printed by the extractor in any scenario.
sentinel="fixture-sentinel-$$"
jq -cn --arg token "$sentinel" '{token: $token}' >"$base_fixture/token.json" ||
  fatal 'cannot create token fixture'

write_pin() {
  # $1 destination, $2 manifest digest hex, $3 platform
  jq -cn \
    --arg repository "$repository" \
    --arg digest "sha256:$2" \
    --arg platform "$3" \
    '{repository: $repository, manifest_digest: $digest, platform: $platform}' \
    >"$1" || fatal "cannot write pin $1"
}

case_dir=''
exit_code=0

setup_case() {
  case_dir="$work/cases/$1"
  mkdir -p "$case_dir/state" "$case_dir/out" || fatal "cannot create case $1"
}

run_case() {
  # $1 fail phase, $2 fail times, $3 pin file, $4 fixture directory
  # `env -u BASH_ENV`: a caller-supplied BASH_ENV runs before the extractor
  # (also a bash script) and may rewrite PATH, which would silently drop
  # the fake curl and send the test to the real registry.
  exit_code=0
  env -u BASH_ENV \
    FAKE_CURL_STATE_DIR="$case_dir/state" \
    FAKE_CURL_FIXTURE_DIR="$4" \
    FAKE_CURL_FAIL_PHASE="$1" \
    FAKE_CURL_FAIL_TIMES="$2" \
    PATH="$fake_bin:$PATH" \
    timeout --foreground "$timeout_seconds" \
    "$extractor" "$3" "$case_dir/out" \
    >"$case_dir/stdout" 2>"$case_dir/stderr" || exit_code=$?
  cat "$case_dir/stdout" "$case_dir/stderr" >"$case_dir/combined"
}

attempts() {
  local file="$case_dir/state/$1.attempts"
  if [[ -f "$file" ]]; then
    cat "$file"
  else
    printf '0\n'
  fi
}

host_file() {
  printf '%s\n' "$case_dir/out/rootfs/CLIProxyAPI/CLIProxyAPI"
}

# $1 test name, $2 phase that must show transient attempts (or 'none')
check_success() {
  local name=$1 phase=$2
  local reasons=''
  local want_attempts=$((transient_failures + 1))
  ((exit_code == 0)) || reasons+="exit=$exit_code (want 0); "
  [[ "$(cat "$case_dir/stdout")" == "$(host_file)" ]] ||
    reasons+='stdout is not exactly the host path; '
  if [[ -x "$(host_file)" ]]; then
    cmp -s "$(host_file)" "$expected_host" ||
      reasons+='extracted host bytes differ from fixture bytes; '
  else
    reasons+='extracted host is missing or not executable; '
  fi
  if [[ "$phase" != none ]]; then
    [[ "$(attempts "$phase")" == "$want_attempts" ]] ||
      reasons+="$phase attempts=$(attempts "$phase") (want $want_attempts: $transient_failures resets then success); "
  fi
  grep -Fq "$sentinel" "$case_dir/combined" &&
    reasons+='bearer token value was printed; '
  if [[ -z "$reasons" ]]; then
    pass "$name"
  else
    fail "$name" "$reasons"
  fi
}

# $1 test name, $2 phase, $3 grep -E pattern naming that phase
check_persistent() {
  local name=$1 phase=$2 phase_pattern=$3
  local reasons=''
  local phase_attempts
  phase_attempts=$(attempts "$phase")
  ((exit_code != 0)) || reasons+='exit=0 (want nonzero); '
  ((exit_code != 124)) ||
    reasons+="did not terminate within ${timeout_seconds}s (timeout kill); "
  ((phase_attempts >= transient_failures + 1)) ||
    reasons+="$phase attempts=$phase_attempts (want at least $((transient_failures + 1)): it must retry); "
  ((phase_attempts <= max_attempts_allowed)) ||
    reasons+="$phase attempts=$phase_attempts (want at most $max_attempts_allowed: retries must be bounded); "
  case "$phase" in
    token)
      [[ "$(attempts manifest)" == 0 && "$(attempts blob)" == 0 ]] ||
        reasons+='later phases were attempted without a token; '
      ;;
    manifest)
      [[ "$(attempts blob)" == 0 ]] ||
        reasons+='blob phase was attempted without a manifest; '
      ;;
  esac
  [[ ! -e "$(host_file)" ]] ||
    reasons+='a host was extracted despite the persistent failure; '
  grep -iqE "$phase_pattern" "$case_dir/combined" ||
    reasons+='output does not identify the failed phase; '
  if ! grep -Fq "$repository" "$case_dir/combined" &&
    ! grep -Fq "sha256:${manifest_digest_hex}" "$case_dir/combined"; then
    reasons+='output carries no safe image context (repository or pinned digest); '
  fi
  grep -Fq "$sentinel" "$case_dir/combined" &&
    reasons+='bearer token value was printed; '
  grep -iqE 'authorization:[[:space:]]*bearer' "$case_dir/combined" &&
    reasons+='an authorization header was printed; '
  if [[ -z "$reasons" ]]; then
    pass "$name"
  else
    fail "$name" "$reasons"
  fi
}

base_pin="$work/pin-base.json"
write_pin "$base_pin" "$manifest_digest_hex" 'linux/amd64'

# Baseline: proves the fixtures themselves are valid, so failures below are
# caused by missing reliability behaviour rather than a broken fixture.
setup_case clean-fetch-extracts-exact-host-bytes
run_case none 0 "$base_pin" "$base_fixture"
check_success clean-fetch-extracts-exact-host-bytes none

for phase in token manifest blob; do
  name="transient-${phase}-reset-recovers-with-exact-bytes"
  setup_case "$name"
  run_case "$phase" "$transient_failures" "$base_pin" "$base_fixture"
  check_success "$name" "$phase"
done

setup_case persistent-token-reset-bounded-nonzero-diagnostic
run_case token always "$base_pin" "$base_fixture"
check_persistent persistent-token-reset-bounded-nonzero-diagnostic token 'token|auth'

setup_case persistent-manifest-reset-bounded-nonzero-diagnostic
run_case manifest always "$base_pin" "$base_fixture"
check_persistent persistent-manifest-reset-bounded-nonzero-diagnostic manifest 'manifest'

setup_case persistent-blob-reset-bounded-nonzero-diagnostic
run_case blob always "$base_pin" "$base_fixture"
check_persistent persistent-blob-reset-bounded-nonzero-diagnostic blob 'blob|layer'

# Fail closed: the served manifest does not hash to the pinned digest, so
# extraction must stop before any blob fetch or host extraction.
wrong_manifest_hex=$(printf 'not-the-served-manifest' | sha256sum | cut -d' ' -f1)
mismatch_pin="$work/pin-wrong-manifest.json"
write_pin "$mismatch_pin" "$wrong_manifest_hex" 'linux/amd64'
name='manifest-digest-mismatch-fails-before-extraction'
setup_case "$name"
run_case none 0 "$mismatch_pin" "$base_fixture"
reasons=''
((exit_code != 0)) || reasons+='exit=0 (want nonzero); '
[[ "$(attempts blob)" == 0 ]] ||
  reasons+='blob phase was attempted after a manifest digest mismatch; '
[[ ! -e "$(host_file)" ]] ||
  reasons+='a host was extracted despite the manifest digest mismatch; '
if [[ -z "$reasons" ]]; then
  pass "$name"
else
  fail "$name" "$reasons"
fi

# Fail closed: the manifest is correctly pinned but names a layer digest
# that the served blob bytes do not hash to.
wrong_layer_hex=$(printf 'not-the-served-layer' | sha256sum | cut -d' ' -f1)
layer_fixture="$work/fixtures-wrong-layer"
mkdir -p "$layer_fixture" || fatal 'cannot create wrong-layer fixture directory'
cp "$base_fixture/token.json" "$base_fixture/layer.tar.gz" "$layer_fixture/" ||
  fatal 'cannot copy wrong-layer fixtures'
jq -cn \
  --arg digest "sha256:${wrong_layer_hex}" \
  --argjson size "$layer_size" \
  '{schemaVersion: 2,
    mediaType: "application/vnd.oci.image.manifest.v1+json",
    layers: [{mediaType: "application/vnd.oci.image.layer.v1.tar+gzip",
              digest: $digest, size: $size}]}' \
  >"$layer_fixture/manifest.json" || fatal 'cannot create wrong-layer manifest'
wrong_layer_manifest_hex=$(sha256sum "$layer_fixture/manifest.json" | cut -d' ' -f1)
layer_pin="$work/pin-wrong-layer.json"
write_pin "$layer_pin" "$wrong_layer_manifest_hex" 'linux/amd64'
name='layer-digest-mismatch-fails-before-extraction'
setup_case "$name"
run_case none 0 "$layer_pin" "$layer_fixture"
reasons=''
((exit_code != 0)) || reasons+='exit=0 (want nonzero); '
[[ "$(attempts blob)" -ge 1 ]] ||
  reasons+='blob phase was never attempted, so the layer check was not reached; '
[[ ! -e "$(host_file)" ]] ||
  reasons+='a host was extracted despite the layer digest mismatch; '
if [[ -z "$reasons" ]]; then
  pass "$name"
else
  fail "$name" "$reasons"
fi

# Fail closed: a non-linux/amd64 pin must be rejected before any fetch.
platform_pin="$work/pin-arm64.json"
write_pin "$platform_pin" "$manifest_digest_hex" 'linux/arm64'
name='non-linux-amd64-platform-fails-before-fetch'
setup_case "$name"
run_case none 0 "$platform_pin" "$base_fixture"
reasons=''
((exit_code != 0)) || reasons+='exit=0 (want nonzero); '
[[ "$(attempts token)" == 0 && "$(attempts manifest)" == 0 && "$(attempts blob)" == 0 ]] ||
  reasons+='a fetch was attempted for a rejected platform; '
[[ ! -e "$(host_file)" ]] ||
  reasons+='a host was extracted for a rejected platform; '
if [[ -z "$reasons" ]]; then
  pass "$name"
else
  fail "$name" "$reasons"
fi

printf '%d passed, %d failed\n' "$pass_count" "$fail_count"
((fail_count == 0))
