#!/usr/bin/env bash
set -euo pipefail

pin="${1:?image pin JSON is required}"
out="${2:?output directory is required}"

die() {
  printf 'extract-host-image: %s\n' "$*" >&2
  exit 1
}

# A pin file holding two image objects used to pass both objects through jq,
# which emitted two TSV rows, and the single `read` below then quietly took the
# first: the extractor fetched an image the pin no longer named. The pin must
# hold exactly one object, so a stream of values is rejected here instead of
# being truncated to its first value.
pin_error=$(jq -r '
  if length != 1 then
    "the image pin must hold exactly one image object"
  elif (.[0] | type) != "object" then
    "the image pin must hold an image object"
  else
    # `@tsv` stringifies whatever the pin holds, so a boolean or null would
    # become the valid-looking name `false` or `null` and reach the registry.
    .[0] as $image
    | [["repository", "manifest_digest", "platform"][]
       | select(($image[.] | type) != "string")]
    | first // empty
    | "image pin field \(.) must be a JSON string"
  end
' --slurp "$pin") || die 'cannot read the image pin JSON'
[[ -z "$pin_error" ]] || die "$pin_error"

fields=$(jq -er '
  if length == 1 then .[0] else empty end
  | [.repository, .manifest_digest, .platform] | @tsv
' --slurp "$pin") || die 'cannot read the image pin JSON'
IFS=$'\t' read -r repository digest platform <<<"$fields"

# The pin is interpolated into registry URLs, so validate it against Docker's
# reference name grammar rather than trusting the producer: this is stricter
# than a component being merely "not empty", so it rejects trailing separators
# (`example/repo-`) and every `..` path segment, and a name carrying `?` or `#`
# cannot rewrite the request path or the token scope.
repository_component='[a-z0-9]+((\.|_|__|-+)[a-z0-9]+)*'
[[ "$repository" =~ ^${repository_component}(/${repository_component})*$ ]] ||
  die "image pin has an invalid repository name: $repository"
[[ "$digest" =~ ^sha256:[0-9a-f]{64}$ ]] ||
  die "image pin has an invalid manifest digest for $repository: $digest"
[[ "$platform" == 'linux/amd64' ]] ||
  die "unsupported platform $platform for $repository: only linux/amd64 is supported"

mkdir -p "$out/rootfs" || die "cannot create the output directory $out/rootfs"

# A single transient transport failure used to abort the whole compatibility
# matrix: `curl: (35) Recv failure: Connection reset by peer` during an image
# fetch failed run-host-matrix.sh and `ci-ok` with it. Retry only transport
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
max_token_bytes=1048576

retryable() {
  case "$1" in
    6 | 7 | 18 | 28 | 35 | 52 | 55 | 56 | 92) return 0 ;;
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
  # Bound the token while receiving it, even without Content-Length, before
  # any shell capture. One extra byte distinguishes an oversized response from
  # one exactly at the limit. A failed attempt never contributes stdout bytes.
  local phase=$1 url=$2
  shift 2
  local -a extra=("$@")
  local attempt status size scratch
  scratch=$(mktemp "$out/.extract-host-token.XXXXXX") ||
    die "$phase: cannot create a token scratch file"
  for ((attempt = 1; attempt <= max_attempts; attempt++)); do
    status=0
    curl "${curl_args[@]}" ${extra[@]+"${extra[@]}"} "$url" |
      head -c "$((max_token_bytes + 1))" >"$scratch" || status=$?
    size=$(wc -c <"$scratch")
    if ((size > max_token_bytes)); then
      rm -f "$scratch"
      die "$phase response for $repository exceeds $max_token_bytes bytes"
    fi
    if ((status == 0)); then
      cat "$scratch" || status=$?
      rm -f "$scratch"
      return "$status"
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

# Validate whole JSON strings before raw output can split embedded newlines
# into otherwise valid digest records. The file is consumed only after jq
# succeeds, so an invalid descriptor stops every blob fetch.
# Do not use `jq -e`: an empty array must reach the "declares no layers" gate.
digests="$out/layer-digests.txt"
jq -sr '
  if length != 1 or (.[0] | type) != "object" then
    error("manifest must contain exactly one JSON object")
  else .[0] end
  | if (.layers | type) == "array" then
    .layers[].digest |
    if type == "string" and test("\\Asha256:[0-9a-f]{64}\\z") then .
    else error("invalid layer digest") end
  else error("layers is not an array") end
' "$manifest" >"$digests" ||
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
