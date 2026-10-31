#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
# Test harness only (deploy/hermetic.py): when set, this ephemeral loopback stub
# is the sole approved management origin and the live port is refused, so a
# test whose curl stub is bypassed can never reach live CLIProxy. It accepts
# only fixture- management keys, so a real key is never sent to it. Never set
# it on a host run.
: "${CLIPROXY_TEST_MANAGEMENT_ORIGIN:=}"
if test -n "$CLIPROXY_TEST_MANAGEMENT_ORIGIN"; then
  python3 - "$CLIPROXY_TEST_MANAGEMENT_ORIGIN" <<'PY'
import sys, urllib.parse
value=sys.argv[1]
parsed=urllib.parse.urlsplit(value)
try:
    port=parsed.port
except ValueError:
    port=None
if parsed.scheme != "http" or parsed.hostname != "127.0.0.1" or port is None or not 32768 <= port <= 65535 or value != f"http://127.0.0.1:{port}":
    raise SystemExit("CLIPROXY_TEST_MANAGEMENT_ORIGIN must be http://127.0.0.1:<ephemeral port 32768-65535>; the live port is never a test origin")
PY
fi
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:=}"
: "${CLIPROXY_MANAGEMENT_KEY:=}"
: "${ZAI_CODING_PLAN_KEY_FILE:=}"
: "${ZAI_CODING_PLAN_KEY:=}"
# Secret intake is file-first, environment-second, both fail-closed. Key files
# stay the default; on hosts where no standalone key file exists, the operator
# sources the established credential file in-process and passes the value
# through the environment (never a new file on disk). Refs below are
# `file:<path>` or `env:<VAR>`; every consumer validates the value identically.
: "${CLIPROXY_SNAPSHOT_ONLY:=}"
if test -z "$CLIPROXY_MANAGEMENT_KEY_FILE" && test -z "$CLIPROXY_MANAGEMENT_KEY"; then
  printf '%s\n' 'set CLIPROXY_MANAGEMENT_KEY_FILE to a 0600 regular file or CLIPROXY_MANAGEMENT_KEY in-process' >&2
  exit 1
fi
if test -z "$ZAI_CODING_PLAN_KEY_FILE" && test -z "$ZAI_CODING_PLAN_KEY" && test -z "$CLIPROXY_SNAPSHOT_ONLY"; then
  printf '%s\n' 'set ZAI_CODING_PLAN_KEY_FILE to a 0600 regular file or ZAI_CODING_PLAN_KEY in-process (optional only in snapshot-only mode)' >&2
  exit 1
fi
# The historical model-usage dashboard endpoint is retired; leave this empty
# (the default) to skip the dashboard step. Set it only when a live dashboard
# URL is explicitly in scope for the host under test.
: "${CLIPROXY_DASHBOARD_URL:=}"
# Docker-native log capture: set CLIPROXY_CONTAINER to capture
# `docker logs` from the running CLIProxy container, or CLIPROXY_LOG_COMMAND
# to run an explicit bounded log command. The retired systemd unit path is no
# longer the default. CLIPROXY_JOURNAL_TIMEOUT bounds the capture.
: "${CLIPROXY_CONTAINER:=}"
: "${CLIPROXY_LOG_COMMAND:=}"
: "${CLIPROXY_JOURNAL_TIMEOUT:=5}"
: "${CLIPROXY_ROUTER_DRY_RUN:=}"
: "${CLIPROXY_CANARY_COMMAND:=}"
: "${CLIPROXY_CANARY_TIMEOUT:=5}"
# Snapshot-only telemetry: set to 1 when the host's usage-snapshot service is
# the sole quota poller and feed writer. The verifier then reads the existing
# snapshot files and never invokes the collectors against live feeds.
: "${CLIPROXY_SNAPSHOT_ONLY:=}"

require_secure_key_file() {
  local path=$1
  test -f "$path" || { printf '%s\n' "key file must be a regular file" >&2; exit 1; }
  test ! -L "$path" || { printf '%s\n' "key file must not be a symlink" >&2; exit 1; }
  test "$(stat -c '%a' -- "$path")" = "600" || { printf '%s\n' "key file must have mode 0600" >&2; exit 1; }
}
if test -n "$CLIPROXY_MANAGEMENT_KEY_FILE"; then
  require_secure_key_file "$CLIPROXY_MANAGEMENT_KEY_FILE"
  management_ref="file:$CLIPROXY_MANAGEMENT_KEY_FILE"
else
  management_ref="env:CLIPROXY_MANAGEMENT_KEY"
fi
if test -n "$ZAI_CODING_PLAN_KEY_FILE"; then
  require_secure_key_file "$ZAI_CODING_PLAN_KEY_FILE"
  plan_ref="file:$ZAI_CODING_PLAN_KEY_FILE"
elif test -n "$ZAI_CODING_PLAN_KEY"; then
  plan_ref="env:ZAI_CODING_PLAN_KEY"
else
  plan_ref=""
  printf '%s\n' 'plan-key marker scan: no plan-key input in snapshot-only mode; scanning with the management marker only' >&2
fi

python3 - "$CLIPROXY_JOURNAL_TIMEOUT" "$CLIPROXY_CANARY_TIMEOUT" <<'PY'
import re, sys
for name, value in zip(("CLIPROXY_JOURNAL_TIMEOUT", "CLIPROXY_CANARY_TIMEOUT"), sys.argv[1:]):
    if len(value) > 16 or not re.fullmatch(r"(?:[1-9]\d*(?:\.\d+)?|0\.\d*[1-9]\d*)[smh]?", value):
        raise SystemExit(f"{name} must be a positive duration using s, m, or h")
PY

if test -z "$CLIPROXY_LOG_COMMAND" && test -z "$CLIPROXY_CONTAINER"; then
  printf '%s\n' 'set CLIPROXY_CONTAINER to the running CLIProxy container (or CLIPROXY_LOG_COMMAND for an explicit log command)' >&2
  exit 1
fi
case "$CLIPROXY_CONTAINER" in
  "" ) ;;
  *[!A-Za-z0-9_.-]* | .* | -*)
    printf '%s\n' 'CLIPROXY_CONTAINER is not a valid Docker container name' >&2
    exit 1
    ;;
esac

umask 077
work_dir=$(mktemp -d)
trap 'rm -rf "$work_dir"' EXIT

run_bounded_check() {
  local label=$1
  shift
  local stdout_pipe="$work_dir/$label.stdout.pipe"
  local stderr_pipe="$work_dir/$label.stderr.pipe"
  mkfifo "$stdout_pipe" "$stderr_pipe"
  python3 -c 'import pathlib, sys; raw=sys.stdin.buffer.read(1_048_577); pathlib.Path(sys.argv[1]).write_bytes(raw[:1_048_576]); raise SystemExit(len(raw) > 1_048_576)' "$work_dir/$label.out" <"$stdout_pipe" &
  local stdout_pid=$!
  python3 -c 'import pathlib, sys; raw=sys.stdin.buffer.read(1_048_577); pathlib.Path(sys.argv[1]).write_bytes(raw[:1_048_576]); raise SystemExit(len(raw) > 1_048_576)' "$work_dir/$label.err" <"$stderr_pipe" &
  local stderr_pid=$!
  set +e
  timeout --foreground --signal=TERM --kill-after=1 -- "$CLIPROXY_CANARY_TIMEOUT" "$@" >"$stdout_pipe" 2>"$stderr_pipe"
  local command_status=$?
  wait "$stdout_pid"
  local stdout_status=$?
  wait "$stderr_pid"
  local stderr_status=$?
  set -e
  rm -f "$stdout_pipe" "$stderr_pipe"
  if test "$stdout_status" -ne 0 || test "$stderr_status" -ne 0; then
    printf '%s output exceeded 1 MiB per stream\n' "$label" >&2
    exit 1
  fi
  if test "$command_status" -ne 0; then
    printf '%s failed\n' "$label" >&2
    exit 1
  fi
}

if test -n "$CLIPROXY_ROUTER_DRY_RUN"; then
  run_bounded_check router-dry-run bash -c "$CLIPROXY_ROUTER_DRY_RUN"
  grep -Eq '^zai/(openai/)?[A-Za-z0-9._-]+$' "$work_dir/router-dry-run.out" || { printf '%s\n' 'router dry-run did not select a Z.ai model' >&2; exit 1; }
fi

if test -n "$CLIPROXY_CANARY_COMMAND"; then
  run_bounded_check canary bash -c "$CLIPROXY_CANARY_COMMAND"
fi

status_file="$work_dir/status.json"

trap 'rm -rf "$work_dir"' EXIT
dashboard_file="$work_dir/dashboard.json"
log_file="$work_dir/service.log"
curl_config="$work_dir/management.curl"
status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/zai-coding-plan/status"

python3 - "$CLIPROXY_MANAGEMENT_URL" "$status_url" <<'PY'
import os, sys, urllib.parse
allowed={os.environ["CLIPROXY_TEST_MANAGEMENT_ORIGIN"]} if os.environ.get("CLIPROXY_TEST_MANAGEMENT_ORIGIN") else {"http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317"}
def origin(value):
    parsed=urllib.parse.urlsplit(value)
    if parsed.scheme not in {"http","https"} or not parsed.hostname or parsed.username or parsed.password:
        raise SystemExit("management URL must be an absolute HTTP(S) URL without userinfo")
    if parsed.query or parsed.fragment:
        raise SystemExit("management URL must not contain a query or fragment")
    try:
        port=parsed.port
    except ValueError:
        raise SystemExit("management URL has an invalid port")
    host=parsed.hostname.lower()
    rendered=f"[{host}]" if ":" in host else host
    return f"{parsed.scheme}://{rendered}:{port or (80 if parsed.scheme == 'http' else 443)}"
base, status=sys.argv[1:]
if origin(base) not in {origin(value) for value in allowed}:
    raise SystemExit("management URL origin is not approved")
parsed=urllib.parse.urlsplit(status)
if parsed.path != "/v0/management/plugins/zai-coding-plan/status" or origin(status) != origin(base):
    raise SystemExit("management status URL is not approved")
PY

python3 - "$management_ref" "$curl_config" <<'PY'
import os, pathlib, sys
ref=sys.argv[1]
if ref.startswith("env:"):
    key=os.environ.get(ref[4:], "")
    if not key or key.strip() != key or "\n" in key or "\r" in key:
        raise SystemExit("management key environment input must contain exactly one non-empty line")
else:
    raw=pathlib.Path(ref[5:] if ref.startswith("file:") else ref).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        raise SystemExit("management key file must contain exactly one non-empty line")
    try:
        key=lines[0].decode("utf-8")
    except UnicodeDecodeError:
        raise SystemExit("management key file must contain valid UTF-8")
if os.environ.get("CLIPROXY_TEST_MANAGEMENT_ORIGIN") and not key.startswith("fixture-"):
    raise SystemExit("CLIPROXY_TEST_MANAGEMENT_ORIGIN accepts only fixture- management keys; a real key never leaves for a test origin")
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

unauthenticated=$(curl -q --fail-early --max-redirs 0 --silent --show-error --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' --output /dev/null --write-out '%{http_code}' "$status_url")
test "$unauthenticated" = 401

curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' \
  --config "$curl_config" \
  "$status_url" >"$status_file"

python3 - "$status_file" "$management_ref" "$plan_ref" <<'PY'
import json, math, os, pathlib, re, sys
expected_top={"plugin","status","version","generated_at","accounts"}
required={"identity","cooldown","name","key_suffix","plan","five_hour_utilization","weekly_utilization","five_hour_resets_at","weekly_resets_at","quota_source","quota_observed_at","quota_age_seconds","quota_stale","offpeak","health","estimator_complete_since","delivery_warning","persistence_warning","unknown_model_warning","heuristic_dedup_warning","dedup_mode"}
optional={"quota_error","five_hour_error"}
cooldown_reasons={"retry_after","reset_header","reset_body","quota_reset_fallback","configured_fallback","upstream_rate_limit"}
identity_pattern=re.compile(r"^[0-9a-f]{64}$")
rfc3339=re.compile(r"^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})$")
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(ref):
    if ref.startswith("env:"):
        value=os.environ.get(ref[4:], "")
        if not value or value.strip() != value or "\n" in value or "\r" in value:
            fail("secret environment input must contain exactly one non-empty line")
        return value
    raw=pathlib.Path(ref[5:] if ref.startswith("file:") else ref).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail("secret input file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail("secret input file must contain valid UTF-8")
def read_secret_opt(ref):
    return read_secret(ref) if ref else None
def scan_decoded(value, markers):
    if isinstance(value, str):
        if any(marker and marker in value for marker in markers):
            fail("authenticated status response failed decoded confidential-value scan")
    elif isinstance(value, dict):
        for key, child in value.items():
            scan_decoded(key, markers)
            scan_decoded(child, markers)
    elif isinstance(value, list):
        for child in value:
            scan_decoded(child, markers)
raw=pathlib.Path(sys.argv[1]).read_bytes()
if len(raw) > 1_048_576:
    fail("authenticated status response exceeds bounded scan size")
management=read_secret(sys.argv[2])
plan=read_secret_opt(sys.argv[3])
markers=[management] + ([plan] if plan else [])
if plan and len(plan) > 6:
    markers.append(plan[-6:])
if any(marker.encode() in raw for marker in markers):
    fail("authenticated status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
try:
    status=json.loads(raw, parse_constant=reject_constant)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
    fail("authenticated status response is not strict JSON")
scan_decoded(status, markers)
require(isinstance(status, dict) and set(status) == expected_top, "authenticated status response has unexpected top-level fields")
require(status["plugin"]=="zai-coding-plan" and status["status"]=="registered", "authenticated status response has unexpected plugin state")
require(isinstance(status["version"], str) and bool(status["version"]), "authenticated status response has invalid version")
require(isinstance(status["generated_at"], str) and bool(rfc3339.fullmatch(status["generated_at"])), "authenticated status response has invalid generated_at")
require(isinstance(status["accounts"], list) and bool(status["accounts"]), "authenticated status response has no accounts")
require(any(isinstance(account, dict) and account.get("health") == "healthy" and account.get("quota_stale") is False for account in status["accounts"]), "authenticated status response has no usable managed capacity")
seen_identities=set()
for account in status["accounts"]:
    require(isinstance(account, dict), "authenticated status account must be an object")
    require(required <= set(account) and set(account) <= required | optional, "authenticated status account has unexpected fields")
    require(isinstance(account["name"], str) and bool(account["name"]), "authenticated status account has invalid name")
    require(account["key_suffix"] == "redacted", "authenticated status account key suffix is not redacted")
    identity=account["identity"]
    require(isinstance(identity, str) and bool(identity_pattern.fullmatch(identity)), "authenticated status account has invalid identity")
    require(identity not in seen_identities, "authenticated status account identities are not unique")
    seen_identities.add(identity)
    cooldown=account["cooldown"]
    require(isinstance(cooldown, dict) and set(cooldown) == {"active","until","reason","source"}, "authenticated status account has invalid cooldown shape")
    require(isinstance(cooldown["active"], bool), "authenticated status account has invalid cooldown active flag")
    require(cooldown["source"] == "zai_runtime_health_v1", "authenticated status account has unsupported cooldown source")
    if cooldown["active"]:
        require(cooldown["reason"] in cooldown_reasons, "authenticated status account has unsupported cooldown reason")
        require(isinstance(cooldown["until"], str) and bool(rfc3339.fullmatch(cooldown["until"])), "authenticated status account has invalid cooldown deadline")
    else:
        require(cooldown["until"] is None and cooldown["reason"] == "", "authenticated status account has contradictory inactive cooldown")
    require(account["quota_source"] in {"quota_api","estimate"}, "authenticated status account has invalid quota source")
    require(isinstance(account["quota_age_seconds"], int) and not isinstance(account["quota_age_seconds"], bool) and account["quota_age_seconds"] >= 0, "authenticated status account has invalid quota age")
    require(all(account[field] is None or isinstance(account[field], str) and rfc3339.fullmatch(account[field]) for field in ("five_hour_resets_at","weekly_resets_at","estimator_complete_since")), "authenticated status account has invalid optional timestamp")
    require(isinstance(account["quota_observed_at"], str) and bool(rfc3339.fullmatch(account["quota_observed_at"])), "authenticated status account has invalid observed timestamp")
    require(all(isinstance(account[field], (int,float)) and not isinstance(account[field], bool) and math.isfinite(account[field]) and 0 <= account[field] <= 1 for field in ("five_hour_utilization","weekly_utilization")), "authenticated status account has invalid utilization")
    require(not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) for key in account if key != "identity"), "authenticated status account contains a secret-like field")
PY

if test -z "$CLIPROXY_SNAPSHOT_ONLY"; then
  "${COLLECTOR_ZAI:-$(dirname "$0")/collector-zai.py}" \
    --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$ZAI_CODING_PLAN_KEY_FILE" \
    --url "$status_url"
else
  printf '%s\n' 'snapshot-only: existing usage-snapshot service remains the sole poller; collector invocation skipped'
fi

# Prove both provider modules are hosted by the coordinator, not just that the
# zai module's own sub-route answers. This queries the coordinator's own
# aggregate status route (distinct from status_url above) and asserts every
# expected provider ID is present, independent of and in addition to the
# opencode-go lane's own deep field validation in verify-live-opencodego.sh.
coordinator_status_file="$work_dir/coordinator-status.json"
coordinator_status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/subscription-pool/status"

python3 - "$CLIPROXY_MANAGEMENT_URL" "$coordinator_status_url" <<'PY'
import os, sys, urllib.parse
allowed={os.environ["CLIPROXY_TEST_MANAGEMENT_ORIGIN"]} if os.environ.get("CLIPROXY_TEST_MANAGEMENT_ORIGIN") else {"http://127.0.0.1:8317", "http://[::1]:8317", "http://localhost:8317"}
def origin(value):
    parsed=urllib.parse.urlsplit(value)
    if parsed.scheme not in {"http","https"} or not parsed.hostname or parsed.username or parsed.password:
        raise SystemExit("management URL must be an absolute HTTP(S) URL without userinfo")
    if parsed.query or parsed.fragment:
        raise SystemExit("management URL must not contain a query or fragment")
    try:
        port=parsed.port
    except ValueError:
        raise SystemExit("management URL has an invalid port")
    host=parsed.hostname.lower()
    rendered=f"[{host}]" if ":" in host else host
    return f"{parsed.scheme}://{rendered}:{port or (80 if parsed.scheme == 'http' else 443)}"
base, status=sys.argv[1:]
if origin(base) not in {origin(value) for value in allowed}:
    raise SystemExit("management URL origin is not approved")
parsed=urllib.parse.urlsplit(status)
if parsed.path != "/v0/management/plugins/subscription-pool/status" or origin(status) != origin(base):
    raise SystemExit("coordinator status URL is not approved")
PY

curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
  --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' \
  --config "$curl_config" \
  "$coordinator_status_url" >"$coordinator_status_file"

python3 - "$coordinator_status_file" "$management_ref" "$plan_ref" <<'PY'
import json, os, pathlib, sys
expected_providers={"zai", "opencode-go"}
def fail(message):
    raise SystemExit(message)
def read_secret(ref):
    if ref.startswith("env:"):
        value=os.environ.get(ref[4:], "")
        if not value or value.strip() != value or "\n" in value or "\r" in value:
            fail("secret environment input must contain exactly one non-empty line")
        return value
    raw=pathlib.Path(ref[5:] if ref.startswith("file:") else ref).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail("secret input file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail("secret input file must contain valid UTF-8")
def read_secret_opt(ref):
    return read_secret(ref) if ref else None
def scan_decoded(value, markers):
    if isinstance(value, str):
        if any(marker and marker in value for marker in markers):
            fail("coordinator status response failed decoded confidential-value scan")
    elif isinstance(value, dict):
        for key, child in value.items():
            scan_decoded(key, markers)
            scan_decoded(child, markers)
    elif isinstance(value, list):
        for child in value:
            scan_decoded(child, markers)
raw=pathlib.Path(sys.argv[1]).read_bytes()
if len(raw) > 1_048_576:
    fail("coordinator status response exceeds bounded scan size")
management=read_secret(sys.argv[2])
plan=read_secret_opt(sys.argv[3])
markers=[management] + ([plan] if plan else [])
if plan and len(plan) > 6:
    markers.append(plan[-6:])
if any(marker.encode() in raw for marker in markers):
    fail("coordinator status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
try:
    status=json.loads(raw, parse_constant=reject_constant)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
    fail("coordinator status response is not strict JSON")
scan_decoded(status, markers)
if not isinstance(status, dict) or status.get("plugin") != "subscription-pool":
    fail("coordinator status response is not the subscription-pool aggregate")
providers=status.get("providers")
if not isinstance(providers, dict) or not expected_providers.issubset(providers):
    fail("coordinator status response does not list both zai and opencode-go provider modules")
PY

if test -n "$CLIPROXY_DASHBOARD_URL"; then
  curl -q --fail-with-body --fail-early --max-redirs 0 --silent --show-error \
    --max-time 5 --max-filesize 1048576 \
    "$CLIPROXY_DASHBOARD_URL" >"$dashboard_file"
else
  printf '%s\n' 'dashboard step skipped: historical model-usage dashboard endpoint is retired'
fi

if test -n "$CLIPROXY_LOG_COMMAND"; then
  bash -c "$CLIPROXY_LOG_COMMAND" \
    | python3 -c 'import sys; raw=sys.stdin.buffer.read(1_048_577); sys.stdout.buffer.write(raw); raise SystemExit(len(raw) > 1_048_576)' \
    >"$log_file"
else
  timeout --foreground --signal=TERM --kill-after=1 -- "$CLIPROXY_JOURNAL_TIMEOUT" \
    docker logs --since 15m --tail 2000 "$CLIPROXY_CONTAINER" 2>&1 \
    | python3 -c 'import sys; raw=sys.stdin.buffer.read(1_048_577); sys.stdout.buffer.write(raw); raise SystemExit(len(raw) > 1_048_576)' \
    >"$log_file"
fi

python3 - "$CLIPROXY_USAGE_DIR/zai.json" "$dashboard_file" "$log_file" "$management_ref" "$plan_ref" <<'PY'
import json, os, pathlib, re, sys
from datetime import datetime, timezone
projected, dashboard, service_log = map(pathlib.Path, sys.argv[1:4])
def fail(message):
    raise SystemExit(message)
def require(condition, message):
    if not condition:
        fail(message)
def read_secret(ref):
    if ref.startswith("env:"):
        value=os.environ.get(ref[4:], "")
        if not value or value.strip() != value or "\n" in value or "\r" in value:
            fail("secret environment input must contain exactly one non-empty line")
        return value
    raw=pathlib.Path(ref[5:] if ref.startswith("file:") else ref).read_bytes()
    lines=raw.splitlines()
    if len(lines) != 1 or not lines[0] or lines[0].strip() != lines[0]:
        fail("secret input file must contain exactly one non-empty line")
    try:
        return lines[0].decode("utf-8")
    except UnicodeDecodeError:
        fail("secret input file must contain valid UTF-8")
def read_secret_opt(ref):
    return read_secret(ref) if ref else None
def load_json(path):
    try:
        return json.loads(path.read_bytes(), parse_constant=lambda _: fail(f"{path.name} contains a non-RFC JSON value"))
    except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
        fail(f"{path.name} is not strict JSON")
def scan_decoded(value, markers, label):
    if isinstance(value, str):
        if any(marker and marker in value for marker in markers):
            fail(f"{label} failed decoded confidential-value scan")
    elif isinstance(value, dict):
        for key, child in value.items():
            scan_decoded(key, markers, label)
            scan_decoded(child, markers, label)
    elif isinstance(value, list):
        for child in value:
            scan_decoded(child, markers, label)
management=read_secret(sys.argv[4])
plan=read_secret_opt(sys.argv[5])
markers=[management] + ([plan] if plan else [])
if plan and len(plan) > 6:
    markers.append(plan[-6:])
scan_targets=[projected, service_log]
if dashboard.exists():
    scan_targets.append(dashboard)
for path in scan_targets:
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        fail(f"{path.name} exceeds bounded scan size")
    if any(marker.encode() in raw for marker in markers):
        fail(f"{path.name} failed confidential-value scan")
payload=load_json(projected)
if dashboard.exists():
    dashboard_payload=load_json(dashboard)
    scan_decoded(dashboard_payload, markers, dashboard.name)
scan_decoded(payload, markers, projected.name)
snapshot_service_feed = bool(os.environ.get("CLIPROXY_SNAPSHOT_ONLY")) and isinstance(payload, dict) and "lane" not in payload
if snapshot_service_feed:
    require(set(payload) == {"observedAt","records","schemaVersion","staleAfterSeconds"}, "snapshot feed has an unexpected envelope: the existing snapshot service writes only observedAt/records/schemaVersion/staleAfterSeconds")
    require(isinstance(payload["records"], list) and bool(payload["records"]) and all(isinstance(record, dict) for record in payload["records"]), "snapshot feed has no records")
else:
    require(isinstance(payload, dict) and payload.get("schemaVersion")==1 and payload.get("lane")=="zai" and isinstance(payload.get("records"), list) and bool(payload["records"]), "projected collector payload has an invalid schema")
require(isinstance(payload.get("observedAt"), str) and bool(re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", payload["observedAt"])), "projected collector payload has an invalid observedAt")
require(isinstance(payload.get("staleAfterSeconds"), int) and not isinstance(payload.get("staleAfterSeconds"), bool) and payload["staleAfterSeconds"] > 0, "projected collector payload has an invalid staleAfterSeconds")
try:
    observed_at=datetime.fromisoformat(payload["observedAt"].replace("Z", "+00:00"))
except ValueError:
    fail("projected collector payload has an invalid observedAt")
now=datetime.now(timezone.utc)
require(observed_at <= now, "projected collector payload is dated in the future")
require((now - observed_at).total_seconds() <= payload["staleAfterSeconds"], "projected collector payload is stale: snapshot service is not refreshing the feed")
def scan_keys(value, label):
    if isinstance(value, dict):
        for key, child in value.items():
            if key != "identity" and re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I):
                fail(f"{label} contains a secret-like field")
            scan_keys(child, label)
    elif isinstance(value, list):
        for child in value:
            scan_keys(child, label)
scan_keys(payload, projected.name)
if not snapshot_service_feed:
    require(all(isinstance(record, dict) and record.get("key_suffix")=="redacted" for record in payload["records"]), "projected collector payload contains an unredacted key suffix")
    require(all(isinstance(record, dict) and isinstance(record.get("identity"), str) and bool(re.fullmatch(r"[0-9a-f]{64}", record["identity"])) for record in payload["records"]), "projected collector payload has an invalid account identity")
    require(all(isinstance(record, dict) and isinstance(record.get("cooldown"), dict) and isinstance(record["cooldown"].get("active"), bool) for record in payload["records"]), "projected collector payload has an invalid account cooldown")
PY

printf 'dogfood-live: authenticated status, collector lane, service-log, snapshot freshness, and bounded secret-marker scans PASS\n'
