#!/usr/bin/env bash
set -Eeuo pipefail

stage=initialization
trap 'status=$?; printf "Publishing stopped during %s (exit %s).\n" "$stage" "$status" >&2; exit "$status"' ERR

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

[[ $# == 0 ]] || fail 'Usage: bash publish-plugin.sh'
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"

for dependency in dotnet-gitversion docker go; do
  command -v "$dependency" >/dev/null || fail "Required command not found: $dependency"
done

stage='calculating the version'
version=$(dotnet-gitversion . -config GitVersion.yml -nofetch -nocache -showvariable SemVer)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'GitVersion did not return a valid SemVer tag.'
plugin="containers.kritikos.io/system/container-log-sanitizer:$version"
printf 'Publishing %s\n' "$plugin"

stage='running the tests'
go test -race ./... -count=1 -timeout=120s

stage='building the Linux binary'
CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/container-log-sanitizer .

stage='packaging the disabled plugin'
staging=$(mktemp -d "$PWD/.plugin-build.XXXXXX")
trap 'rm -rf -- "$staging"' EXIT
install -D -m 0755 dist/container-log-sanitizer "$staging/rootfs/container-log-sanitizer"
install -D -m 0644 README.md "$staging/rootfs/usr/share/doc/container-log-sanitizer/README.md"
install -D -m 0644 LICENSE "$staging/rootfs/usr/share/doc/container-log-sanitizer/LICENSE"
cp plugin.json "$staging/config.json"

stage='logging in to the registry'
docker login containers.kritikos.io

stage='creating the disabled plugin'
docker plugin create "$plugin" "$staging"

stage='pushing the plugin'
docker plugin push "$plugin"
printf 'Published %s; the local plugin remains disabled.\n' "$plugin"
