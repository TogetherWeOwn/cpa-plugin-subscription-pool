#!/usr/bin/env bash
set -euo pipefail

plugin="${1:?plugin shared library is required}"
images="${2:?resolved image matrix JSON is required}"
work="${3:?matrix work directory is required}"
root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." >/dev/null && pwd)
mkdir -p "$work"

mode=${HOST_MATRIX_NAMESPACE:-auto}
case "$mode" in
  auto)
    if unshare -Urnm --map-root-user true >/dev/null 2>&1; then
      mode=user
    elif [[ $(id -u) -eq 0 ]]; then
      mode=root
    else
      printf 'host matrix requires root or unprivileged user namespaces\n' >&2
      exit 1
    fi
    ;;
  user|root) ;;
  *)
    printf 'HOST_MATRIX_NAMESPACE must be auto, user, or root\n' >&2
    exit 1
    ;;
esac
if [[ $mode == user ]]; then
  command -v unshare >/dev/null
fi
command -v busybox >/dev/null

run_one() {
  local encoded name tag digest image_dir pin host
  encoded=$1
  name=$(base64 -d <<<"$encoded" | jq -er .name)
  tag=$(base64 -d <<<"$encoded" | jq -er .tag)
  digest=$(base64 -d <<<"$encoded" | jq -er .manifest_digest)
  image_dir="$work/$name"
  rm -rf "$image_dir"
  mkdir -p "$image_dir"
  pin="$image_dir/image.json"
  base64 -d <<<"$encoded" > "$pin"
  host=$("$root/.github/scripts/extract-host-image.sh" "$pin" "$image_dir/image")

  local -a prefix=()
  if [[ $mode == user ]]; then
    prefix=(unshare -Urnm --map-root-user)
  fi
  "${prefix[@]}" bash -euo pipefail -c '
    busybox ip link set lo up
    hosts=$(mktemp)
    printf "127.0.0.1 api.z.ai api.cloudflare.com integrate.api.nvidia.com\n::1 localhost\n" > "$hosts"
    mount --bind "$hosts" /etc/hosts
    exec "$@"
  ' bash "$root/.github/scripts/host-integration/host-integration" \
    -host-binary "$host" -plugin "$plugin" -image "$name:$tag@$digest"
}

while IFS= read -r encoded; do
  run_one "$encoded"
done < <(jq -cr '.[] | @base64' "$images")
