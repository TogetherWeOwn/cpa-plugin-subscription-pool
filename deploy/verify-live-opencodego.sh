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
: "${OPENCODE_GO_DASHBOARD_API_KEY_FILE:=}"
: "${OPENCODE_GO_DASHBOARD_API_KEY:=}"
# Secret intake is file-first, environment-second, both fail-closed (see
# verify-live.sh). The dashboard input is required except in snapshot-only
# unbound mode, where no dashboard credential exists to scan for.
: "${CLIPROXY_SNAPSHOT_ONLY:=}"
: "${OPENCODE_GO_ALLOW_UNBOUND:=}"
if test -z "$CLIPROXY_MANAGEMENT_KEY_FILE" && test -z "$CLIPROXY_MANAGEMENT_KEY"; then
  printf '%s\n' 'set CLIPROXY_MANAGEMENT_KEY_FILE to a 0600 regular file or CLIPROXY_MANAGEMENT_KEY in-process' >&2
  exit 1
fi
if test -z "$OPENCODE_GO_DASHBOARD_API_KEY_FILE" && test -z "$OPENCODE_GO_DASHBOARD_API_KEY"; then
  if test -z "$CLIPROXY_SNAPSHOT_ONLY" || test -z "$OPENCODE_GO_ALLOW_UNBOUND"; then
    printf '%s\n' 'set OPENCODE_GO_DASHBOARD_API_KEY_FILE to a 0600 regular file or OPENCODE_GO_DASHBOARD_API_KEY in-process (optional only in snapshot-only unbound mode)' >&2
    exit 1
  fi
fi
# Docker-native log capture: set CLIPROXY_CONTAINER to capture
# `docker logs` from the running CLIProxy container, or CLIPROXY_LOG_COMMAND
# to run an explicit bounded log command. The retired systemd unit path is no
# longer the default. CLIPROXY_JOURNAL_TIMEOUT bounds the capture.
: "${CLIPROXY_CONTAINER:=}"
: "${CLIPROXY_LOG_COMMAND:=}"
: "${CLIPROXY_JOURNAL_TIMEOUT:=5}"
# Snapshot-only telemetry: set to 1 when the host's usage-snapshot service is
# the sole quota poller and feed writer. The verifier then reads the existing
# snapshot files and never invokes the collectors against live feeds.
: "${CLIPROXY_SNAPSHOT_ONLY:=}"
# Go-lane disposition: the strict default still requires a bound dashboard
# credential with usable capacity. Set to 1 only when the lane is knowingly
# unprovisioned on the host; the verifier then accepts a strictly all-unknown
# provider shape as UNAVAILABLE (unchanged, not a regression) after the same
# confidential-value scans, and never fabricates capacity.
: "${OPENCODE_GO_ALLOW_UNBOUND:=}"

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
if test -n "$OPENCODE_GO_DASHBOARD_API_KEY_FILE"; then
  require_secure_key_file "$OPENCODE_GO_DASHBOARD_API_KEY_FILE"
  dashboard_ref="file:$OPENCODE_GO_DASHBOARD_API_KEY_FILE"
elif test -n "$OPENCODE_GO_DASHBOARD_API_KEY"; then
  dashboard_ref="env:OPENCODE_GO_DASHBOARD_API_KEY"
else
  dashboard_ref=""
  printf '%s\n' 'dashboard-key marker scan: no dashboard-key input in snapshot-only unbound mode; scanning with the management marker only' >&2
fi

python3 - "$CLIPROXY_JOURNAL_TIMEOUT" <<'PY'
import re, sys
for name, value in zip(("CLIPROXY_JOURNAL_TIMEOUT",), sys.argv[1:]):
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

status_file="$work_dir/status.json"
log_file="$work_dir/service.log"
curl_config="$work_dir/management.curl"
status_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/subscription-pool/status"

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
if parsed.path != "/v0/management/plugins/subscription-pool/status" or origin(status) != origin(base):
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

lane_file="$work_dir/go-lane"
python3 - "$status_file" "$management_ref" "$dashboard_ref" "$OPENCODE_GO_ALLOW_UNBOUND" "$lane_file" <<'PY'
import json, math, os, pathlib, re, sys
allow_unbound=sys.argv[4] != ""
lane_file=pathlib.Path(sys.argv[5])
lane_disposition="bound"
coordinator_expected_top={"plugin","status","version","generated_at","providers","validation_error"}
coordinator_required_top={"plugin","status","version","generated_at","providers"}
provider_expected={"provider","status","validation_error","credential_bound","accounts","observation_gaps"}
provider_required={"provider","status","credential_bound","observation_gaps"}
account_expected={"name","disabled","windows"}
account_required={"name","windows"}
window_kinds={"five_hour","weekly","monthly"}
window_expected={"known","exhausted","utilization","resets_at","source","authoritative"}
window_required={"known","exhausted"}
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
dashboard_key=read_secret_opt(sys.argv[3])
markers=[management] + ([dashboard_key] if dashboard_key else [])
if dashboard_key and len(dashboard_key) > 6:
    markers.append(dashboard_key[-6:])
if any(marker.encode() in raw for marker in markers):
    fail("authenticated status response failed confidential-value scan")
def reject_constant(_):
    raise ValueError("non-RFC JSON value")
try:
    status=json.loads(raw, parse_constant=reject_constant)
except (UnicodeDecodeError, json.JSONDecodeError, ValueError):
    fail("authenticated status response is not strict JSON")
scan_decoded(status, markers)
require(isinstance(status, dict) and coordinator_required_top <= set(status) <= coordinator_expected_top, "authenticated status response has unexpected top-level fields")
require(status["plugin"]=="subscription-pool" and status["status"]=="registered", "authenticated status response has unexpected plugin state")
require(isinstance(status["version"], str) and bool(status["version"]), "authenticated status response has invalid version")
require(isinstance(status["generated_at"], str) and bool(rfc3339.fullmatch(status["generated_at"])), "authenticated status response has invalid generated_at")
providers=status["providers"]
require(isinstance(providers, dict) and "opencode-go" in providers, "authenticated status response is missing the opencode-go provider entry")
provider=providers["opencode-go"]
require(isinstance(provider, dict), "authenticated opencode-go provider status must be an object")
require(provider_required <= set(provider) <= provider_expected, "authenticated opencode-go provider status has unexpected fields")
require(provider["provider"]=="opencode-go" and provider["status"]=="registered", "authenticated opencode-go provider has unexpected status")
require(isinstance(provider["credential_bound"], bool), "authenticated opencode-go provider has invalid credential_bound")
bound=provider["credential_bound"] is True
if not bound:
    require(allow_unbound, "authenticated opencode-go provider has no bound dashboard credential")
    lane_disposition="unavailable"
gaps=provider["observation_gaps"]
require(isinstance(gaps, list) and all(isinstance(gap, str) for gap in gaps), "authenticated opencode-go provider has invalid observation_gaps")
accounts=provider.get("accounts", [])
require(isinstance(accounts, list), "authenticated opencode-go provider has invalid accounts")
if bound:
    require(bool(accounts), "authenticated opencode-go provider has no accounts")
    require(any(isinstance(account, dict) and not account.get("disabled", False) for account in accounts), "authenticated opencode-go provider has no usable managed capacity")
for account in accounts:
    require(isinstance(account, dict), "authenticated opencode-go account must be an object")
    require(account_required <= set(account) <= account_expected, "authenticated opencode-go account has unexpected fields")
    require(isinstance(account["name"], str) and bool(account["name"]), "authenticated opencode-go account has invalid name")
    windows=account["windows"]
    require(isinstance(windows, dict) and set(windows) == window_kinds, "authenticated opencode-go account windows must contain exactly five_hour, weekly, and monthly")
    for kind, window in windows.items():
        require(isinstance(window, dict), f"authenticated opencode-go {kind} window must be an object")
        require(window_required <= set(window) <= window_expected, f"authenticated opencode-go {kind} window has unexpected fields")
        require(isinstance(window["known"], bool) and isinstance(window["exhausted"], bool), f"authenticated opencode-go {kind} window has invalid known/exhausted")
        if not window["known"]:
            require("utilization" not in window and "resets_at" not in window, f"authenticated opencode-go {kind} window reports data while unknown")
        else:
            if "utilization" in window:
                value=window["utilization"]
                require(isinstance(value, (int, float)) and not isinstance(value, bool) and math.isfinite(value) and 0 <= value <= 1, f"authenticated opencode-go {kind} window has invalid utilization")
            if "resets_at" in window:
                require(isinstance(window["resets_at"], str) and bool(rfc3339.fullmatch(window["resets_at"])), f"authenticated opencode-go {kind} window has invalid resets_at")
    require(not any(re.search(r"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)", key, re.I) and key != "credential_bound" for key in account), "authenticated opencode-go account contains a secret-like field")
lane_file.write_text(lane_disposition)
PY

if test -z "$CLIPROXY_SNAPSHOT_ONLY"; then
  "${COLLECTOR_OPENCODEGO:-$(dirname "$0")/collector-opencodego.py}" \
    --management-key-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$CLIPROXY_MANAGEMENT_KEY_FILE" \
    --secret-marker-file "$OPENCODE_GO_DASHBOARD_API_KEY_FILE" \
    --url "$status_url"
else
  printf '%s\n' 'snapshot-only: existing usage-snapshot service remains the sole poller; collector invocation skipped'
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

python3 - "$CLIPROXY_USAGE_DIR/opencode-go.json" "$log_file" "$management_ref" "$dashboard_ref" "$lane_file" <<'PY'
import json, os, pathlib, re, sys
from datetime import datetime, timezone
projected, service_log = map(pathlib.Path, sys.argv[1:3])
lane_disposition=pathlib.Path(sys.argv[5]).read_text().strip()
require_disposition=lane_disposition == "unavailable"
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
management=read_secret(sys.argv[3])
dashboard_key=read_secret_opt(sys.argv[4])
markers=[management] + ([dashboard_key] if dashboard_key else [])
if dashboard_key and len(dashboard_key) > 6:
    markers.append(dashboard_key[-6:])
for path in (projected, service_log):
    raw = path.read_bytes()
    if len(raw) > 1_048_576:
        fail(f"{path.name} exceeds bounded scan size")
    if any(marker.encode() in raw for marker in markers):
        fail(f"{path.name} failed confidential-value scan")
payload=load_json(projected)
scan_decoded(payload, markers, projected.name)
snapshot_only=bool(os.environ.get("CLIPROXY_SNAPSHOT_ONLY"))
snapshot_service_feed=snapshot_only and isinstance(payload, dict) and "lane" not in payload
if snapshot_service_feed:
    require(set(payload) == {"observedAt","records","schemaVersion","staleAfterSeconds"}, "snapshot feed has an unexpected envelope: the existing snapshot service writes only observedAt/records/schemaVersion/staleAfterSeconds")
    require(isinstance(payload["records"], list) and bool(payload["records"]) and all(isinstance(record, dict) for record in payload["records"]), "snapshot feed has no records")
elif require_disposition:
    require(isinstance(payload, dict) and payload.get("schemaVersion")==1 and payload.get("lane")=="opencode-go" and isinstance(payload.get("records"), list), "projected collector payload has an invalid schema")
else:
    require(isinstance(payload, dict) and payload.get("schemaVersion")==1 and payload.get("lane")=="opencode-go" and isinstance(payload.get("records"), list) and bool(payload["records"]), "projected collector payload has an invalid schema")
require(isinstance(payload.get("observedAt"), str) and bool(re.fullmatch(r"\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:Z|[+-]\d{2}:\d{2})", payload["observedAt"])), "projected collector payload has an invalid observedAt")
require(isinstance(payload.get("staleAfterSeconds"), int) and not isinstance(payload.get("staleAfterSeconds"), bool) and payload["staleAfterSeconds"] > 0, "projected collector payload has an invalid staleAfterSeconds")
try:
    observed_at=datetime.fromisoformat(payload["observedAt"].replace("Z", "+00:00"))
except ValueError:
    fail("projected collector payload has an invalid observedAt")
now=datetime.now(timezone.utc)
require(observed_at <= now, "projected collector payload is dated in the future")
require((now - observed_at).total_seconds() <= payload["staleAfterSeconds"], "projected collector payload is stale: snapshot service is not refreshing the feed")
serialized=json.dumps(payload, allow_nan=False)
require(not re.search(r'"(?:api[-_]?key|authorization|credential|secret|token|key_hash|identity)"\s*:', serialized, re.I), "projected collector payload contains a secret-like field")
# Snapshot-service feeds carry no credential_bound field; the lane disposition
# was already decided from live coordinator status above, so there is nothing
# to agree against here. Collector-written feeds still prove agreement below.
if snapshot_service_feed:
    pass
elif require_disposition:
    require(payload.get("credential_bound") is False, "projected collector payload disagrees with the unavailable lane disposition")
else:
    require(payload.get("credential_bound") is True, "projected collector payload reports no bound dashboard credential")
PY

if test "$(cat "$lane_file")" = "unavailable"; then
  printf 'dogfood-live: opencode-go lane UNAVAILABLE (no bound dashboard credential; unchanged, not a v0.4.5 regression) after coordinator status, snapshot freshness, and service-log confidential-value scans PASS\n'
else
  printf 'dogfood-live: authenticated coordinator status, opencode-go provider lane, collector output, snapshot freshness, and service-log confidential-value scans PASS\n'
fi
