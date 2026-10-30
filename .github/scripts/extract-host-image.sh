#!/usr/bin/env bash
set -euo pipefail

pin="${1:?image pin JSON is required}"
out="${2:?output directory is required}"
repository=$(jq -er .repository "$pin")
digest=$(jq -er .manifest_digest "$pin")
platform=$(jq -er .platform "$pin")
[[ "$platform" == "linux/amd64" ]]

# A single transient transport failure used to abort the whole compatibility
# matrix: `curl: (35) Recv failure: Connection reset by peer` on the token
# request failed run-host-matrix.sh and `ci-ok` with it. Retry only transport
# failures a bounded number of times; HTTP errors, digest mismatches and every
# other failure still stop at once. `--connect-timeout` bounds a stalled
# connect without capping layer downloads, which can legitimately be large.
max_attempts=3
retry_delay=1

retryable() {
  case "$1" in
    7 | 28 | 35 | 52 | 56) return 0 ;;
    *) return 1 ;;
  esac
}

fetch() {
  # fetch <phase> <url> <destination|-> [curl args...]
  local phase=$1 url=$2 destination=$3
  shift 3
  local attempt status
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    status=0
    if [[ "$destination" == - ]]; then
      curl --fail --location --silent --show-error --connect-timeout 30 "$@" "$url" || status=$?
    else
      curl --fail --location --silent --show-error --connect-timeout 30 "$@" "$url" >"$destination" || status=$?
    fi
    if ((status == 0)); then
      return 0
    fi
    if ((attempt < max_attempts)) && retryable "$status"; then
      printf 'extract-host-image: %s request for %s failed (curl exit %d); retrying (%d/%d)\n' \
        "$phase" "$repository" "$status" "$attempt" "$max_attempts" >&2
      sleep "$((retry_delay * attempt))"
      continue
    fi
    printf 'extract-host-image: %s request for %s failed after %d attempt(s): curl exit %d\n' \
      "$phase" "$repository" "$attempt" "$status" >&2
    return "$status"
  done
}

mkdir -p "$out/rootfs"

token=$(fetch token \
  "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull" -)
auth=$(jq -er .token <<<"$token")

manifest="$out/manifest.json"
fetch manifest \
  "https://registry-1.docker.io/v2/${repository}/manifests/${digest}" "$manifest" \
  -H "Authorization: Bearer $auth" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

resolved=$(sha256sum "$manifest" | cut -d' ' -f1)
if [[ "sha256:${resolved}" != "$digest" ]]; then
  printf 'extract-host-image: manifest digest mismatch for %s: pinned %s, served sha256:%s\n' \
    "$repository" "$digest" "$resolved" >&2
  exit 1
fi

index=0
while IFS= read -r layer; do
  if [[ ! "$layer" =~ ^sha256:([0-9a-f]{64})$ ]]; then
    printf 'extract-host-image: manifest for %s lists an invalid layer digest\n' "$repository" >&2
    exit 1
  fi
  expected="${BASH_REMATCH[1]}"
  index=$((index + 1))
  blob="$out/layer-${index}.tar.gz"
  fetch blob \
    "https://registry-1.docker.io/v2/${repository}/blobs/${layer}" "$blob" \
    -H "Authorization: Bearer $auth"
  actual=$(sha256sum "$blob" | cut -d' ' -f1)
  if [[ "$actual" != "$expected" ]]; then
    printf 'extract-host-image: layer %d digest mismatch for %s: manifest expects sha256:%s, served sha256:%s\n' \
      "$index" "$repository" "$expected" "$actual" >&2
    exit 1
  fi
  tar -xzf "$blob" -C "$out/rootfs" --overwrite --exclude='dev/*'
done < <(jq -er '.layers[].digest' "$manifest")

host="$out/rootfs/CLIProxyAPI/CLIProxyAPI"
if [[ ! -x "$host" ]]; then
  printf 'extract-host-image: extracted image for %s has no executable host at %s\n' \
    "$repository" "$host" >&2
  exit 1
fi
printf '%s\n' "$host"
