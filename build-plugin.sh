#!/usr/bin/env bash
set -euo pipefail

[[ "$EUID" == 0 ]] || { printf 'Run this installer in a root terminal.\n' >&2; exit 1; }
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
plugin_name=${1:-container-log-sanitizer:dev}
data_root=${2:?Usage: bash build-plugin.sh PLUGIN_NAME /absolute/dedicated/log-directory}
[[ "$data_root" == /* && "$data_root" != / ]] || { printf 'Use an absolute dedicated data directory.\n' >&2; exit 1; }
command -v docker >/dev/null
docker version --format 'Docker server {{.Server.Version}}'
if docker plugin inspect "$plugin_name" >/dev/null 2>&1; then
  printf 'Plugin %s already exists. Use a new versioned name; do not replace an active plugin.\n' "$plugin_name" >&2
  exit 1
fi

staging=$(mktemp -d "$PWD/.plugin-build.XXXXXX")
build_container=
build_image=
cleanup() {
  if [[ -n "$build_container" ]]; then docker rm "$build_container" >/dev/null || true; fi
  if [[ -n "$build_image" ]]; then docker image rm "$build_image" >/dev/null || true; fi
  rm -rf -- "$staging"
}
trap cleanup EXIT
if [[ -e "$data_root" ]]; then
  [[ -d "$data_root" && ! -L "$data_root" ]] || { printf 'Log directory must be a real directory.\n' >&2; exit 1; }
  [[ "$(stat -c %u "$data_root")" == 0 ]] || { printf 'Log directory must be owned by root.\n' >&2; exit 1; }
  if [[ -n "$(find "$data_root" -mindepth 1 -maxdepth 1 -print -quit)" && ! -f "$data_root/.sanitizer-store" ]]; then
    printf 'Refusing a nonempty directory not marked as sanitizer storage.\n' >&2
    exit 1
  fi
fi
mkdir -p "$staging/rootfs" "$data_root"
chmod 0700 "$data_root"
touch "$data_root/.sanitizer-store"
if [[ -f dist/container-log-sanitizer ]]; then
  cp dist/container-log-sanitizer "$staging/rootfs/container-log-sanitizer"
else
  build_image="container-log-sanitizer-build:$(date +%s)-$$"
  docker build -t "$build_image" .
  build_container=$(docker create --network none --log-driver none "$build_image")
  docker export "$build_container" | tar -xf - -C "$staging/rootfs"
fi
install -D -m 0644 README.md "$staging/rootfs/usr/share/doc/container-log-sanitizer/README.md"
install -D -m 0644 LICENSE "$staging/rootfs/usr/share/doc/container-log-sanitizer/LICENSE"
cp plugin.json "$staging/config.json"
docker plugin create "$plugin_name" "$staging"
docker plugin set "$plugin_name" "logdata.source=$data_root"
docker plugin enable --timeout 0 "$plugin_name"
printf 'Installed %s. No containers have been changed.\n' "$plugin_name"
printf 'Run: bash canary.sh %q EXISTING_SHELL_IMAGE local\n' "$plugin_name"
printf 'Run: bash canary.sh %q EXISTING_SHELL_IMAGE json-file\n' "$plugin_name"
