#!/usr/bin/env bash
set -euo pipefail

pin="${1:?image pin JSON is required}"
out="${2:?output directory is required}"

die() {
  printf 'extract-host-image: %s\n' "$*" >&2
  exit 1
}

fields=$(jq -er '[.repository, .manifest_digest, .platform] | @tsv' "$pin") ||
  die 'cannot read the image pin JSON'
IFS=$'\t' read -r repository digest platform <<<"$fields"
[[ -n "$repository" && -n "$digest" && -n "$platform" ]] ||
  die 'image pin is missing repository, manifest_digest or platform'
[[ "$platform" == 'linux/amd64' ]] ||
  die "unsupported platform $platform for $repository: only linux/amd64 is supported"

# A single transient transport failure used to abort the whole compatibility
# matrix: `curl: (35) Recv failure: Connection reset by peer` on the token
# request failed run-host-matrix.sh and `ci-ok` with it. Retry only transport
# failures, a bounded number of times; HTTP errors, digest mismatches and every
# other failure still stop at once.
max_attempts=3
# Half-open sockets and rate-limited resolvers are common on shared runners; a
# connect that takes longer than this is already anomalous for Docker Hub.
connect_timeout=10

retryable() {
  case "$1" in
    6 | 7 | 18 | 28 | 35 | 52 | 56) return 0 ;;
    *) return 1 ;;
  esac
}

body_file=''
cleanup() {
  [[ -z "$body_file" ]] || rm -f "$body_file"
}
trap cleanup EXIT

fetch() {
  # fetch <phase> <destination|-> <url> [curl args...]
  #
  # Each attempt writes to its own scratch file and is moved into place only
  # when curl exits zero. Writing straight to the destination (or to the
  # caller's stdout for the token phase) would let a partial body from a failed
  # attempt be concatenated with the retry's body, so a recoverable reset would
  # still produce a corrupt token or manifest.
  local phase=$1 destination=$2 url=$3
  shift 3
  local -a extra=("$@")
  local attempt status
  body_file=$(mktemp "${TMPDIR:-/tmp}/extract-host-image.XXXXXX")
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    status=0
    curl --fail --location --silent --show-error \
      --connect-timeout "$connect_timeout" \
      ${extra[@]+"${extra[@]}"} "$url" >"$body_file" || status=$?
    if ((status == 0)); then
      if [[ "$destination" == - ]]; then
        cat "$body_file"
        rm -f "$body_file"
      else
        mv "$body_file" "$destination"
      fi
      body_file=''
      return 0
    fi
    if ((attempt < max_attempts)) && retryable "$status"; then
      printf 'extract-host-image: %s request for %s failed (curl exit %d); retrying (%d/%d)\n' \
        "$phase" "$repository" "$status" "$attempt" "$max_attempts" >&2
      sleep "$attempt"
      continue
    fi
    printf 'extract-host-image: %s request for %s failed after %d attempt(s): curl exit %d\n' \
      "$phase" "$repository" "$attempt" "$status" >&2
    return "$status"
  done
}

token=$(fetch token - \
  "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull")
auth=$(jq -er .token <<<"$token") ||
  die "token response for $repository did not contain a token"

mkdir -p "$out/rootfs"
manifest="$out/manifest.json"
fetch manifest "$manifest" \
  "https://registry-1.docker.io/v2/${repository}/manifests/${digest}" \
  -H "Authorization: Bearer $auth" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

resolved=$(sha256sum "$manifest" | cut -d' ' -f1)
[[ "sha256:${resolved}" == "$digest" ]] ||
  die "manifest digest mismatch for $repository: pinned $digest, served sha256:$resolved"

layers=$(jq -er '.layers[].digest' "$manifest") ||
  die "manifest for $repository has no layers"

index=0
while IFS= read -r layer; do
  [[ "$layer" =~ ^sha256:([0-9a-f]{64})$ ]] ||
    die "manifest for $repository lists an invalid layer digest"
  expected="${BASH_REMATCH[1]}"
  index=$((index + 1))
  blob="$out/layer-${index}.tar.gz"
  fetch blob "$blob" \
    "https://registry-1.docker.io/v2/${repository}/blobs/${layer}" \
    -H "Authorization: Bearer $auth"
  actual=$(sha256sum "$blob" | cut -d' ' -f1)
  [[ "$actual" == "$expected" ]] ||
    die "layer $index digest mismatch for $repository: manifest expects sha256:$expected, served sha256:$actual"
  tar -xzf "$blob" -C "$out/rootfs" --overwrite --exclude='dev/*' ||
    die "layer $index of $repository is not a readable tar archive"
done <<<"$layers"

host="$out/rootfs/CLIProxyAPI/CLIProxyAPI"
[[ -x "$host" ]] ||
  die "extracted image for $repository has no executable host at $host"
printf '%s\n' "$host"
