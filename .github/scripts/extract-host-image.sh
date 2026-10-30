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

# The pin is interpolated into registry URLs, so validate it rather than
# trusting the producer: a name or digest carrying `?`, `#` or `..` would
# rewrite the request path or the token scope.
[[ "$repository" =~ ^[a-z0-9][a-z0-9._-]*(/[a-z0-9][a-z0-9._-]*)*$ && "$repository" != *..* ]] ||
  die "image pin has an invalid repository name: $repository"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
  die "image pin has an invalid manifest digest for $repository: $digest"
[[ "$platform" == 'linux/amd64' ]] ||
  die "unsupported platform $platform for $repository: only linux/amd64 is supported"

mkdir -p "$out/rootfs" || die "cannot create the output directory $out/rootfs"

# A single transient transport failure used to abort the whole compatibility
# matrix: `curl: (35) Recv failure: Connection reset by peer` on the token
# request failed run-host-matrix.sh and `ci-ok` with it. Retry only transport
# failures, a bounded number of times; HTTP errors, digest mismatches and every
# other failure still stop at once.
max_attempts=3
connect_timeout=10
# A registry that accepts the connection then never answers is not covered by
# --connect-timeout. Cap a stalled transfer instead of capping legitimate large
# layer downloads: abort only when throughput stays under 1 KiB/s for 30s, and
# keep a generous absolute ceiling.
stall_speed=1024
stall_time=30
max_time=600

retryable() {
  case "$1" in
    6 | 7 | 18 | 28 | 35 | 52 | 56) return 0 ;;
    *) return 1 ;;
  esac
}

curl_args=(
  --fail --location --silent --show-error
  --connect-timeout "$connect_timeout"
  --max-time "$max_time"
  --speed-limit "$stall_speed" --speed-time "$stall_time"
)

fetch_report() {
  # fetch_report <phase> <attempt> <status> — the shared failure diagnostic.
  printf 'extract-host-image: %s request for %s failed after %d attempt(s): curl exit %d\n' \
    "$1" "$repository" "$2" "$3" >&2
}

fetch_stdout() {
  # fetch_stdout <phase> <url> [curl args...] — print the body, or nothing.
  #
  # The body is captured in a variable, never streamed, so a failed attempt
  # cannot leave partial bytes in the caller's capture where they would be
  # concatenated with a successful retry's body.
  local phase=$1 url=$2
  shift 2
  local -a extra=("$@")
  local attempt status body
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    status=0
    body=$(curl "${curl_args[@]}" ${extra[@]+"${extra[@]}"} "$url") || status=$?
    if ((status == 0)); then
      printf '%s' "$body"
      return 0
    fi
    if ((attempt < max_attempts)) && retryable "$status"; then
      printf 'extract-host-image: %s request for %s failed (curl exit %d); retrying (%d/%d)\n' \
        "$phase" "$repository" "$status" "$attempt" "$max_attempts" >&2
      sleep "$attempt"
      continue
    fi
    fetch_report "$phase" "$attempt" "$status"
    return "$status"
  done
}

fetch_file() {
  # fetch_file <phase> <destination> <url> [curl args...]
  #
  # One scratch file per request, truncated by every attempt and moved into
  # place only after curl exits zero, so a partial body can never be mistaken
  # for a complete one. It lives beside the destination so the move is a
  # rename, not a copy across filesystems.
  local phase=$1 destination=$2 url=$3
  shift 3
  local -a extra=("$@")
  local attempt status
  local scratch
  scratch=$(mktemp "$(dirname "$destination")/.extract-host-image.XXXXXX") ||
    die "$phase: cannot create a scratch file beside $destination"
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    status=0
    curl "${curl_args[@]}" ${extra[@]+"${extra[@]}"} "$url" >"$scratch" || status=$?
    if ((status == 0)); then
      chmod 0644 "$scratch"
      mv "$scratch" "$destination" || die "$phase: cannot write $destination"
      return 0
    fi
    if ((attempt < max_attempts)) && retryable "$status"; then
      printf 'extract-host-image: %s request for %s failed (curl exit %d); retrying (%d/%d)\n' \
        "$phase" "$repository" "$status" "$attempt" "$max_attempts" >&2
      sleep "$attempt"
      continue
    fi
    rm -f "$scratch"
    fetch_report "$phase" "$attempt" "$status"
    return "$status"
  done
}

token=$(fetch_stdout token \
  "https://auth.docker.io/token?service=registry.docker.io&scope=repository:${repository}:pull")
auth=$(jq -er .token <<<"$token") ||
  die "token response for $repository did not contain a token"

manifest="$out/manifest.json"
fetch_file manifest "$manifest" \
  "https://registry-1.docker.io/v2/${repository}/manifests/${digest}" \
  -H "Authorization: Bearer $auth" \
  -H 'Accept: application/vnd.oci.image.manifest.v1+json, application/vnd.docker.distribution.manifest.v2+json'

resolved=$(sha256sum "$manifest" | cut -d' ' -f1) ||
  die "cannot hash the manifest for $repository"
[[ "sha256:${resolved}" == "$digest" ]] ||
  die "manifest digest mismatch for $repository: pinned $digest, served sha256:$resolved"

# Read the digests from a file, not `$(...)`: command substitution strips
# trailing newlines, which would silently drop a trailing empty digest and
# skip validating it.
digests="$out/layer-digests.txt"
jq -er '.layers[].digest' "$manifest" >"$digests" ||
  die "manifest for $repository has no readable layer list"

index=0
while IFS= read -r layer; do
  [[ "$layer" =~ ^sha256:([0-9a-f]{64})$ ]] ||
    die "manifest for $repository lists an invalid layer digest"
  expected="${BASH_REMATCH[1]}"
  index=$((index + 1))
  blob="$out/layer-${index}.tar.gz"
  fetch_file blob "$blob" \
    "https://registry-1.docker.io/v2/${repository}/blobs/${layer}" \
    -H "Authorization: Bearer $auth"
  actual=$(sha256sum "$blob" | cut -d' ' -f1) ||
    die "cannot hash layer $index of $repository"
  [[ "$actual" == "$expected" ]] ||
    die "layer $index digest mismatch for $repository: manifest expects sha256:$expected, served sha256:$actual"
  tar -xzf "$blob" -C "$out/rootfs" --overwrite --exclude='dev/*' ||
    die "layer $index of $repository is not a readable tar archive"
done <"$digests"
rm -f "$digests"

[[ $index -gt 0 ]] || die "manifest for $repository declares no layers"

host="$out/rootfs/CLIProxyAPI/CLIProxyAPI"
[[ -f "$host" && -x "$host" ]] ||
  die "extracted image for $repository has no executable host at $host"
printf '%s\n' "$host"
