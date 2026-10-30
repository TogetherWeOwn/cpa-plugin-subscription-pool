#!/usr/bin/env bash
# Offline acceptance tests for extract-host-image.sh fetch reliability.
#
# A fake `curl` is put first on PATH. It serves fixed token, manifest and
# layer fixtures and injects configurable transport/HTTP errors at one phase,
# either before the response or after writing an incomplete body prefix.
# No network, registry or host binary is used. Backoff sleeps are elided.
#
# The fake counts requests, including curl-internal retries. Plain `--retry`
# retries timeout error 28, but not error 35; `--retry-all-errors` retries
# either. An external shell retry loop counts one request per invocation.
# Failed stdout bytes cannot be retracted, just as with real curl; a retry
# to --output replaces that file. Deadline/stall options and the recovered
# bearer token are checked on executed requests, never by grepping source.
#
# stdout contract: run-host-matrix.sh captures the extractor's whole stdout
# as the host binary path, so on success stdout must be exactly that path
# and diagnostics must go elsewhere.
#
# Run from anywhere: .github/scripts/extract-host-image_test.sh
set -uo pipefail

root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." >/dev/null && pwd)
# The override permits red-before/mutation runs against an isolated snapshot.
extractor=${HOST_IMAGE_EXTRACTOR:-"$root/.github/scripts/extract-host-image.sh"}
repository='example/fixture-host'
transient_failures=2
# Three requests total, not three retries after the initial request.
max_attempts_allowed=3
timeout_seconds=10

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
fail_code=${FAKE_CURL_FAIL_EXIT_CODE:-35}
partial_body=${FAKE_CURL_PARTIAL_BODY:-0}
expected_token=${FAKE_CURL_EXPECTED_TOKEN:?FAKE_CURL_EXPECTED_TOKEN is required}
mkdir -p "$state_dir"

url=''
out_file=''
write_out=''
authorization=''
connect_timeout=''
max_time=''
speed_limit=''
speed_time=''
retry=0
retry_all=0
args=("$@")
for ((i = 0; i < ${#args[@]}; i++)); do
  arg=${args[i]}
  header=''
  case "$arg" in
    --retry) retry=${args[i + 1]:-0} ;;
    --retry=*) retry=${arg#--retry=} ;;
    --retry-all-errors) retry_all=1 ;;
    -o | --output) out_file=${args[i + 1]:-} ;;
    --output=*) out_file=${arg#--output=} ;;
    -o?*) out_file=${arg#-o} ;;
    -w | --write-out) write_out=${args[i + 1]:-} ;;
    --write-out=*) write_out=${arg#--write-out=} ;;
    -H | --header) header=${args[i + 1]:-} ;;
    --header=*) header=${arg#--header=} ;;
    -H?*) header=${arg#-H} ;;
    --connect-timeout) connect_timeout=${args[i + 1]:-} ;;
    --connect-timeout=*) connect_timeout=${arg#--connect-timeout=} ;;
    --max-time) max_time=${args[i + 1]:-} ;;
    --max-time=*) max_time=${arg#--max-time=} ;;
    --speed-limit) speed_limit=${args[i + 1]:-} ;;
    --speed-limit=*) speed_limit=${arg#--speed-limit=} ;;
    --speed-time) speed_time=${args[i + 1]:-} ;;
    --speed-time=*) speed_time=${arg#--speed-time=} ;;
    http://* | https://*) url=$arg ;;
  esac
  if [[ "${header,,}" == authorization:* ]]; then
    authorization=${header#*:}
    authorization=${authorization#"${authorization%%[![:space:]]*}"}
  fi
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
    printf 'unsupported curl URL\n' >>"$state_dir/contract-errors"
    printf 'curl: (6) fake curl: unsupported URL\n' >&2
    exit 6
    ;;
esac

max_internal=1
if ((retry > 0)) && ((retry_all == 1 || fail_code == 28)); then
  max_internal=$((retry + 1))
fi

positive_limit() {
  [[ "$1" =~ ^[0-9]+([.][0-9]+)?$ && -n "${1//[0.]/}" ]]
}

request_contract() {
  local invalid=''
  positive_limit "$connect_timeout" || invalid+=' --connect-timeout'
  positive_limit "$max_time" || invalid+=' --max-time'
  positive_limit "$speed_limit" || invalid+=' --speed-limit'
  positive_limit "$speed_time" || invalid+=' --speed-time'
  if [[ -n "$invalid" ]]; then
    printf '%s: missing/nonpositive limits:%s\n' "$phase" "$invalid" >>"$state_dir/contract-errors"
    return 64
  fi
  if [[ "$phase" != token ]] &&
    [[ "${authorization:0:7}" != 'Bearer ' || "${authorization:7}" != "$expected_token" ]]; then
    printf '%s: recovered bearer token differs from fixture\n' "$phase" >>"$state_dir/contract-errors"
    return 64
  fi
}

emit_body() {
  local prefix=$1 size
  if [[ "$prefix" == 1 ]]; then
    size=$(stat -c%s "$fixture")
    # An incomplete, nonempty prefix, including binary gzip bytes for blobs.
    dd if="$fixture" bs=1 count="$((size / 2))" status=none
  else
    cat "$fixture"
  fi
}

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

  request_contract || return $?
  if [[ "$phase" == "$fail_phase" ]] && ((n <= fail_limit)); then
    if [[ "$partial_body" == 1 ]]; then
      if [[ -n "$out_file" ]]; then
        emit_body 1 >"$out_file" || return 23
      else
        emit_body 1 || return 23
      fi
      printf '%s\n' "$n" >>"$state_dir/$phase.partial-attempts"
    fi
    case "$fail_code" in
      22) message='The requested URL returned error: 401' ;;
      28) message='Operation timed out' ;;
      35) message='Recv failure: Connection reset by peer' ;;
      *) message='Injected transport failure' ;;
    esac
    printf 'curl: (%s) %s\n' "$fail_code" "$message" >&2
    return "$fail_code"
  fi

  if [[ -n "$out_file" ]]; then
    emit_body 0 >"$out_file" || return 23
  else
    emit_body 0 || return 23
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
cat >"$fake_bin/sleep" <<'FAKE_SLEEP'
#!/usr/bin/env bash
# The contract concerns requests and deadlines, not wall-clock backoff.
exit 0
FAKE_SLEEP
chmod 0755 "$fake_bin/sleep" || fatal 'cannot make fake sleep executable'

# Fixture payload: one layer holding an executable host with known bytes.
payload_src="$work/payload"
expected_host="$payload_src/CLIProxyAPI/CLIProxyAPI"
mkdir -p "$payload_src/CLIProxyAPI" || fatal 'cannot create payload directory'
cat >"$expected_host" <<'HOST'
#!/bin/sh
if [ -n "${HOST_FIXTURE_EXECUTED_MARKER:-}" ]; then
  printf 'unexpected execution\n' >"$HOST_FIXTURE_EXECUTED_MARKER"
fi
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
  # $1 fail phase, $2 fail times, $3 pin file, $4 fixture directory,
  # $5 curl exit code (default 35), $6 emit partial body (default 0).
  # `env -u BASH_ENV`: a caller-supplied BASH_ENV runs before the extractor
  # (also a bash script) and may rewrite PATH, which would silently drop
  # the fake curl and send the test to the real registry.
  exit_code=0
  env -u BASH_ENV \
    FAKE_CURL_STATE_DIR="$case_dir/state" \
    FAKE_CURL_FIXTURE_DIR="$4" \
    FAKE_CURL_FAIL_PHASE="$1" \
    FAKE_CURL_FAIL_TIMES="$2" \
    FAKE_CURL_FAIL_EXIT_CODE="${5:-35}" \
    FAKE_CURL_PARTIAL_BODY="${6:-0}" \
    FAKE_CURL_EXPECTED_TOKEN="$sentinel" \
    HOST_FIXTURE_EXECUTED_MARKER="$case_dir/host-executed" \
    PATH="$fake_bin:$PATH" \
    timeout --foreground --kill-after=2 "$timeout_seconds" \
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

safe_output_reasons() {
  grep -Fq "$sentinel" "$case_dir/combined" &&
    printf 'bearer token value was printed; '
  grep -iqE 'authorization:[[:space:]]*bearer' "$case_dir/combined" &&
    printf 'an authorization header was printed; '
  [[ ! -e "$case_dir/host-executed" ]] ||
    printf 'the fixture host was executed; '
  [[ ! -s "$case_dir/state/contract-errors" ]] ||
    printf 'executed curl argument/token contract failed; '
}

# $1 test name, $2 phase that must show transient attempts (or 'none').
# Authenticated requests prove the token bytes; digest checks plus rejection
# fixtures prove manifest/blob bytes; cmp proves the final host bytes.
check_success() {
  local name=$1 phase=$2
  local reasons='' request_phase want_attempts
  ((exit_code == 0)) || reasons+="exit=$exit_code (want 0); "
  printf '%s\n' "$(host_file)" >"$case_dir/expected-stdout"
  cmp -s "$case_dir/stdout" "$case_dir/expected-stdout" ||
    reasons+='stdout is not exactly the host path and one newline; '
  cmp -s "$case_dir/out/manifest.json" "$base_fixture/manifest.json" ||
    reasons+='recovered manifest bytes differ from fixture bytes; '
  cmp -s "$case_dir/out/layer-1.tar.gz" "$base_fixture/layer.tar.gz" ||
    reasons+='recovered blob bytes differ from fixture bytes; '
  if [[ -f "$(host_file)" && -x "$(host_file)" ]]; then
    cmp -s "$(host_file)" "$expected_host" ||
      reasons+='extracted host bytes differ from fixture bytes; '
  else
    reasons+='extracted host is missing, not regular or not executable; '
  fi
  for request_phase in token manifest blob; do
    want_attempts=1
    [[ "$request_phase" != "$phase" ]] || want_attempts=$((transient_failures + 1))
    [[ "$(attempts "$request_phase")" == "$want_attempts" ]] ||
      reasons+="$request_phase attempts=$(attempts "$request_phase") (want exactly $want_attempts); "
  done
  reasons+=$(safe_output_reasons)
  if [[ -z "$reasons" ]]; then
    pass "$name"
  else
    fail "$name" "$reasons"
  fi
}

# $1 test name, $2 phase, $3 grep -E pattern naming that phase,
# $4 exact attempts (default 3; a non-retryable error permits only 1).
check_persistent() {
  local name=$1 phase=$2 phase_pattern=$3 want_attempts=${4:-$max_attempts_allowed}
  local reasons='' phase_attempts
  phase_attempts=$(attempts "$phase")
  ((exit_code != 0)) || reasons+='exit=0 (want nonzero); '
  ((exit_code != 124 && exit_code != 137)) ||
    reasons+="did not terminate within ${timeout_seconds}s (timeout kill); "
  [[ "$phase_attempts" == "$want_attempts" ]] ||
    reasons+="$phase attempts=$phase_attempts (want exactly $want_attempts); "
  case "$phase" in
    token)
      [[ "$(attempts manifest)" == 0 && "$(attempts blob)" == 0 ]] ||
        reasons+='later phases were attempted without a token; '
      ;;
    manifest)
      [[ "$(attempts token)" == 1 && "$(attempts blob)" == 0 ]] ||
        reasons+='earlier phase was retried or blob was attempted without a manifest; '
      ;;
    blob)
      [[ "$(attempts token)" == 1 && "$(attempts manifest)" == 1 ]] ||
        reasons+='earlier phases were retried after a blob failure; '
      ;;
  esac
  [[ ! -s "$case_dir/stdout" ]] || reasons+='a host path was emitted on failure; '
  [[ ! -e "$(host_file)" ]] ||
    reasons+='a host was extracted despite the persistent failure; '
  grep -iqE "$phase_pattern" "$case_dir/combined" ||
    reasons+='output does not identify the failed phase; '
  if ! grep -Fq "$repository" "$case_dir/combined" &&
    ! grep -Fq "sha256:${manifest_digest_hex}" "$case_dir/combined"; then
    reasons+='output carries no safe image context (repository or pinned digest); '
  fi
  reasons+=$(safe_output_reasons)
  if [[ -z "$reasons" ]]; then
    pass "$name"
  else
    fail "$name" "$reasons"
  fi
}

# $1 test name, $2 last reached gate: pin, manifest, layer or host,
# $3 optional diagnostic pattern, $4 optional rejected host kind. Request
# counts prove the intended gate was reached, not an unrelated earlier failure.
check_rejected() {
  local name=$1 gate=$2 diagnostic=${3:-} host_kind=${4:-} reasons=''
  local want_token=1 want_manifest=1 want_blob=0
  if [[ "$gate" == pin ]]; then
    want_token=0
    want_manifest=0
  elif [[ "$gate" == layer || "$gate" == host ]]; then
    want_blob=1
  fi
  ((exit_code != 0)) || reasons+='exit=0 (want rejection); '
  ((exit_code != 124 && exit_code != 137)) || reasons+='rejection timed out; '
  [[ ! -s "$case_dir/stdout" ]] || reasons+='a host path was emitted on rejection; '
  [[ "$(attempts token)" == "$want_token" &&
    "$(attempts manifest)" == "$want_manifest" &&
    "$(attempts blob)" == "$want_blob" ]] ||
    reasons+="requests token/manifest/blob=$(attempts token)/$(attempts manifest)/$(attempts blob) (want $want_token/$want_manifest/$want_blob at $gate gate); "
  if [[ "$gate" != host && -e "$(host_file)" ]]; then
    reasons+='a host was extracted before rejection; '
  fi
  if [[ "$host_kind" == directory ]]; then
    [[ -d "$(host_file)" && -x "$(host_file)" ]] ||
      reasons+='executable-directory fixture did not reach the host gate; '
  elif [[ "$host_kind" == non-executable-file ]]; then
    [[ -f "$(host_file)" && ! -x "$(host_file)" ]] ||
      reasons+='non-executable regular-file fixture did not reach the host gate; '
  fi
  if [[ -n "$diagnostic" ]] && ! grep -iqE "$diagnostic" "$case_dir/stderr"; then
    reasons+="missing intended diagnostic: $diagnostic; "
  fi
  reasons+=$(safe_output_reasons)
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

# All allowed transport failures recover on request 3 and stop on request 3
# when persistent. Error 28 exercises timeout handling; 55 covers send errors.
# Error 35 is already covered by the original named reset cases above.
for code in 6 7 18 28 52 55 56; do
  for phase in token manifest blob; do
    name="transient-${phase}-curl-${code}-recovers-with-exact-bytes"
    setup_case "$name"
    run_case "$phase" "$transient_failures" "$base_pin" "$base_fixture" "$code"
    check_success "$name" "$phase"

    name="persistent-${phase}-curl-${code}-stops-at-three-requests"
    setup_case "$name"
    run_case "$phase" always "$base_pin" "$base_fixture" "$code"
    check_persistent "$name" "$phase" "$phase"
  done
done

# An HTTP/auth failure is not a transient transport failure, at any boundary.
for phase in token manifest blob; do
  name="non-retryable-${phase}-curl-22-stops-after-one-request"
  setup_case "$name"
  run_case "$phase" always "$base_pin" "$base_fixture" 22
  check_persistent "$name" "$phase" "$phase" 1
done

# Each failed attempt emits half a real response. Retaining these bytes and
# appending the next response breaks token parsing or manifest/blob integrity.
for phase in token manifest blob; do
  name="partial-${phase}-body-is-discarded-before-retry"
  setup_case "$name"
  run_case "$phase" "$transient_failures" "$base_pin" "$base_fixture" 18 1
  printf '1\n2\n' >"$case_dir/expected-partial-attempts"
  if ! cmp -s "$case_dir/state/$phase.partial-attempts" "$case_dir/expected-partial-attempts"; then
    fail "$name" 'both failed requests must emit a nonempty body prefix'
  else
    check_success "$name" "$phase"
  fi
done

# The fake rejects every actual request missing a positive connect timeout,
# total deadline, low-speed limit or low-speed interval. This is an executed
# argument contract at all three stages, not a source-text assertion.
name='every-fetch-stage-receives-connect-total-and-stall-limits'
setup_case "$name"
run_case none 0 "$base_pin" "$base_fixture"
check_success "$name" none

# Fail closed: the served manifest does not hash to the pinned digest, so
# extraction must stop before any blob fetch or host extraction.
wrong_manifest_hex=$(printf 'not-the-served-manifest' | sha256sum | cut -d' ' -f1)
mismatch_pin="$work/pin-wrong-manifest.json"
write_pin "$mismatch_pin" "$wrong_manifest_hex" 'linux/amd64'
name='manifest-digest-mismatch-fails-before-extraction'
setup_case "$name"
run_case none 0 "$mismatch_pin" "$base_fixture"
check_rejected "$name" manifest

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
check_rejected "$name" layer

# Fail closed: a non-linux/amd64 pin must be rejected before any fetch.
platform_pin="$work/pin-arm64.json"
write_pin "$platform_pin" "$manifest_digest_hex" 'linux/arm64'
name='non-linux-amd64-platform-fails-before-fetch'
setup_case "$name"
run_case none 0 "$platform_pin" "$base_fixture"
check_rejected "$name" pin

# Correctly digest-pinned layers reach the regular/executable host gate.
for kind in directory non-executable-file; do
  fixture="$work/fixtures-host-$kind"
  payload="$work/payload-host-$kind"
  mkdir -p "$fixture" "$payload/CLIProxyAPI" || fatal 'cannot create rejected-host fixture'
  if [[ "$kind" == directory ]]; then
    mkdir "$payload/CLIProxyAPI/CLIProxyAPI" || fatal 'cannot create executable directory'
    chmod 0755 "$payload/CLIProxyAPI/CLIProxyAPI" || fatal 'cannot set directory mode'
  else
    cp "$expected_host" "$payload/CLIProxyAPI/CLIProxyAPI" || fatal 'cannot copy non-executable host'
    chmod 0644 "$payload/CLIProxyAPI/CLIProxyAPI" || fatal 'cannot set non-executable mode'
  fi
  tar -czf "$fixture/layer.tar.gz" -C "$payload" CLIProxyAPI || fatal 'cannot archive rejected host'
  digest=$(sha256sum "$fixture/layer.tar.gz" | cut -d' ' -f1)
  size=$(stat -c%s "$fixture/layer.tar.gz")
  jq --arg digest "sha256:$digest" --argjson size "$size" \
    '.layers[0].digest = $digest | .layers[0].size = $size' \
    "$base_fixture/manifest.json" >"$fixture/manifest.json" || fatal 'cannot write rejected-host manifest'
  cp "$base_fixture/token.json" "$fixture/" || fatal 'cannot copy token fixture'
  digest=$(sha256sum "$fixture/manifest.json" | cut -d' ' -f1)
  pin="$work/pin-host-$kind.json"
  write_pin "$pin" "$digest" 'linux/amd64'
  name="host-$kind-is-rejected-without-emitting-a-path"
  setup_case "$name"
  run_case none 0 "$pin" "$fixture"
  check_rejected "$name" host '' "$kind"
done

# Hash each malformed manifest and pin those exact bytes, so a digest mismatch
# cannot mask the intended layer-list/digest rejection. No blob may be fetched.
# Newlines stay inside ONE JSON descriptor. Both segments use the real fixture
# digest, so splitting it into records cannot fail just on an unknown blob URL
# or a mismatched blob checksum; the whole value must be rejected before fetch.
while IFS='|' read -r kind update diagnostic; do
  fixture="$work/fixtures-layers-$kind"
  mkdir -p "$fixture" || fatal 'cannot create rejected-manifest fixture'
  cp "$base_fixture/token.json" "$base_fixture/layer.tar.gz" "$fixture/" ||
    fatal 'cannot copy rejected-manifest fixtures'
  jq --arg digest "sha256:$layer_digest_hex" "$update" \
    "$base_fixture/manifest.json" >"$fixture/manifest.json" ||
    fatal 'cannot write rejected manifest'
  digest=$(sha256sum "$fixture/manifest.json" | cut -d' ' -f1)
  pin="$work/pin-layers-$kind.json"
  write_pin "$pin" "$digest" 'linux/amd64'
  name="manifest-$kind-is-rejected-before-blob-fetch"
  setup_case "$name"
  run_case none 0 "$pin" "$fixture"
  check_rejected "$name" manifest "$diagnostic"
done <<'LAYER_CASES'
empty-layers|.layers = []|declares no layers
malformed-layer-list|.layers = {}|
invalid-layer-digest|.layers[0].digest = "not-a-sha256-digest"|
null-layer-digest|.layers[0].digest = null|
embedded-newline-layer-digest|.layers[0].digest = ($digest + "\n" + $digest)|
trailing-newline-layer-digest|.layers[0].digest = ($digest + "\n")|
LAYER_CASES

# Invalid pin types and repository grammar must fail before networking, not
# after coercion to TSV strings. All three counters must remain exactly zero.
while IFS='|' read -r kind update; do
  pin="$work/pin-$kind.json"
  jq "$update" "$base_pin" >"$pin" || fatal 'cannot write invalid pin fixture'
  name="pin-$kind-is-rejected-with-zero-requests"
  setup_case "$name"
  run_case none 0 "$pin" "$base_fixture"
  check_rejected "$name" pin
done <<'PIN_CASES'
boolean-repository|.repository = true
null-repository|.repository = null
boolean-manifest-digest|.manifest_digest = true
null-manifest-digest|.manifest_digest = null
boolean-platform|.platform = true
null-platform|.platform = null
trailing-separator-repository|.repository = "example/repo-"
PIN_CASES

printf '%d passed, %d failed\n' "$pass_count" "$fail_count"
((fail_count == 0))
