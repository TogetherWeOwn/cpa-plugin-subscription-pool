#!/usr/bin/env bash
set -euo pipefail

: "${CLIPROXY_MANAGEMENT_URL:=http://127.0.0.1:8317}"
# Test harness only (deploy/hermetic.py): when set, this ephemeral loopback stub
# is the sole approved management origin and the live port is refused, so a
# test whose curl stub is bypassed can never reach live CLIProxy. Never set it
# on a host run.
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
: "${CLIPROXY_CONFIG:=/home/ubuntu/stacks/cliproxy/config.yaml}"
: "${CLIPROXY_BACKUP:?set CLIPROXY_BACKUP to the recorded pre-install backup}"
: "${CLIPROXY_MANAGEMENT_KEY_FILE:=}"
: "${CLIPROXY_MANAGEMENT_KEY:=}"
# File-first, environment-second, both fail-closed (see verify-live.sh). The
# root-ownership assertion below applies to key files only; an in-process
# environment value has no file to assert on.
if test -z "$CLIPROXY_MANAGEMENT_KEY_FILE" && test -z "$CLIPROXY_MANAGEMENT_KEY"; then
  printf '%s\n' 'set CLIPROXY_MANAGEMENT_KEY_FILE to a root-owned 0600 file or CLIPROXY_MANAGEMENT_KEY in-process' >&2
  exit 1
fi
if test -n "$CLIPROXY_MANAGEMENT_KEY_FILE"; then
  management_ref="file:$CLIPROXY_MANAGEMENT_KEY_FILE"
else
  management_ref="env:CLIPROXY_MANAGEMENT_KEY"
fi
: "${CLIPROXY_USAGE_DIR:=/srv/cliproxy-usage}"
# Expected config ownership without changing it: the rollback never chowns.
# Keep the root default; the operator sets both to the host's actual owner
# (for example the stack owner's uid/gid) when the layout is not root-owned.
: "${CLIPROXY_CONFIG_UID:=0}"
: "${CLIPROXY_CONFIG_GID:=0}"
# Docker-native service reload: an explicit command wins, then
# `docker restart` of CLIPROXY_CONTAINER, then the legacy systemd unit.
: "${CLIPROXY_CONTAINER:=}"
: "${CLIPROXY_RESTART_COMMAND:=}"

umask 077
management_url="${CLIPROXY_MANAGEMENT_URL%/}/v0/management/plugins/subscription-pool"
python3 - "$CLIPROXY_MANAGEMENT_URL" "$management_url" <<'PY'
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
base, removal=sys.argv[1:]
if origin(base) not in {origin(value) for value in allowed}:
    raise SystemExit("management URL origin is not approved")
parsed=urllib.parse.urlsplit(removal)
if parsed.path != "/v0/management/plugins/subscription-pool" or origin(removal) != origin(base):
    raise SystemExit("management plugin removal URL is not approved")
PY

case "$CLIPROXY_CONFIG_UID" in
  "" | *[!0-9]* )
    printf '%s\n' 'CLIPROXY_CONFIG_UID and CLIPROXY_CONFIG_GID must be numeric uids' >&2
    exit 1
    ;;
esac
case "$CLIPROXY_CONFIG_GID" in
  "" | *[!0-9]* )
    printf '%s\n' 'CLIPROXY_CONFIG_UID and CLIPROXY_CONFIG_GID must be numeric uids' >&2
    exit 1
    ;;
esac
case "$CLIPROXY_CONTAINER" in
  "" ) ;;
  *[!A-Za-z0-9_.-]* | .* | -*)
    printf '%s\n' 'CLIPROXY_CONTAINER is not a valid Docker container name' >&2
    exit 1
    ;;
esac

config_dir=$(dirname -- "$CLIPROXY_CONFIG")
test -d "$config_dir" && test ! -L "$config_dir"
test "$(stat -c '%u:%g:%a' -- "$config_dir")" = "$CLIPROXY_CONFIG_UID:$CLIPROXY_CONFIG_GID:700" || {
  printf '%s\n' 'config directory must be owned by CLIPROXY_CONFIG_UID:CLIPROXY_CONFIG_GID with mode 0700' >&2
  exit 1
}
test -f "$CLIPROXY_BACKUP" && test ! -L "$CLIPROXY_BACKUP"
test "$(stat -c '%u:%a' -- "$CLIPROXY_BACKUP")" = "$CLIPROXY_CONFIG_UID:600" || {
  printf '%s\n' 'rollback backup must be owned by CLIPROXY_CONFIG_UID with mode 0600' >&2
  exit 1
}

restored=
curl_config=
delete_response=
delete_error=
cleanup() { rm -f -- "$restored" "$curl_config" "$delete_response" "$delete_error"; }
trap cleanup EXIT

# Validate the management key and build the curl config BEFORE replacing the live
# config file below, so an invalid key can never abort this script after the restore
# has already landed but before the service has been told to reload it.
curl_config=$(mktemp)
python3 - "$management_ref" "$curl_config" <<'PY'
import os, pathlib, sys
ref=sys.argv[1]
if ref.startswith("env:"):
    key=os.environ.get(ref[4:], "")
    if not key or key.strip() != key or '\n' in key or '\r' in key:
        raise SystemExit('management key environment input must contain one non-empty line')
else:
    key_path=pathlib.Path(ref[5:] if ref.startswith("file:") else ref)
    if key_path.is_symlink() or key_path.stat().st_uid != 0 or key_path.stat().st_mode & 0o777 != 0o600:
        raise SystemExit('management key file must be root-owned mode 0600 and not a symlink')
    key=key_path.read_text().strip()
    if not key or '\n' in key or '\r' in key:
        raise SystemExit('management key file must contain one non-empty line')
path=pathlib.Path(sys.argv[2])
path.write_text('header = "Authorization: Bearer ' + key.replace('\\', '\\\\').replace('"', '\\"') + '"\n')
path.chmod(0o600)
PY

restored=$(mktemp --tmpdir="$config_dir" .config.yaml.rollback.XXXXXX)
install -m 0600 -- "$CLIPROXY_BACKUP" "$restored"
python3 - "$restored" "$config_dir" <<'PY'
import os, pathlib, sys
with pathlib.Path(sys.argv[1]).open('rb') as handle:
    os.fsync(handle.fileno())
directory_fd=os.open(sys.argv[2], os.O_RDONLY | os.O_DIRECTORY)
try:
    os.fsync(directory_fd)
finally:
    os.close(directory_fd)
PY
mv -T -- "$restored" "$CLIPROXY_CONFIG"
restored=

delete_response=$(mktemp)
delete_error=$(mktemp)
if curl -q --fail --fail-early --max-redirs 0 --silent --show-error --connect-timeout 2 --max-time 5 --max-filesize 1048576 --noproxy '*' --proxy '' --config "$curl_config" --output "$delete_response" --stderr "$delete_error" -X DELETE "$management_url" >/dev/null; then
  printf '%s\n' 'management-plane plugin removal PASS'
else
  printf '%s\n' 'management unavailable; configuration restore completed, plugin cleanup remains pending (response body suppressed)' >&2
  if grep -Eq '^curl: \([0-9]+\) [[:print:]]{0,240}$' "$delete_error"; then
    tr -d '\r\n' <"$delete_error" >&2
    printf '\n' >&2
  fi
fi
if test -n "$CLIPROXY_RESTART_COMMAND"; then
  bash -c "$CLIPROXY_RESTART_COMMAND"
elif test -n "$CLIPROXY_CONTAINER"; then
  docker restart "$CLIPROXY_CONTAINER" >/dev/null
else
  systemctl reload cliproxy.service || systemctl restart cliproxy.service
fi
if test -d "$CLIPROXY_USAGE_DIR" && test -f "$(dirname "$0")/remove-usage-output.py"; then
  python3 "$(dirname "$0")/remove-usage-output.py" "$CLIPROXY_USAGE_DIR"
fi
printf '%s\n' 'rollback: restored configuration and reloaded service'
