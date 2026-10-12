#!/usr/bin/env bash
set -Eeuo pipefail

stage=initialization
trap 'status=$?; printf "Publishing stopped during %s (exit %s).\n" "$stage" "$status" >&2; exit "$status"' ERR

fail() {
  printf '%s\n' "$1" >&2
  exit 1
}

[[ $# == 0 ]] || fail 'Usage: bash publish-plugin.sh'
[[ "$EUID" == 0 ]] || fail 'Run bash publish-plugin.sh from a root Bash terminal on the Linux Docker host.'
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"

for dependency in git docker go jq; do
  command -v "$dependency" >/dev/null || fail "Required command not found: $dependency"
done

stage='checking the checkout'
shallow=$(git rev-parse --is-shallow-repository)
[[ "$shallow" == false ]] || fail 'The clone is shallow. Run git fetch --unshallow origin, then retry.'
checkout_status=$(git status --porcelain)
[[ -z "$checkout_status" ]] || fail 'The working tree has uncommitted changes. Run git status --short and commit or resolve them before publishing.'

stage='fetching version tags'
git fetch --tags origin

stage='checking the Docker daemon'
docker version --format 'Docker server {{.Server.Version}}'
operating_system=$(docker info --format '{{.OSType}}')
[[ "$operating_system" == linux ]] || fail 'Publishing requires a Linux Docker daemon with managed-plugin support.'

stage='logging in to the registry'
docker login containers.kritikos.io

stage='calculating the version'
version=$(docker run --rm --network none --log-driver none \
  --volume "$PWD:/repo" \
  gittools/gitversion:6.8.2-alpine.3.23-10.0 \
  /repo -config /repo/GitVersion.yml -nofetch -nocache -showvariable SemVer)
[[ "$version" =~ ^[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$ ]] || fail 'GitVersion did not return a valid SemVer tag.'
plugin="containers.kritikos.io/system/container-log-sanitizer:$version"
data_root="/var/lib/container-log-sanitizer-publish-$version"
printf 'Publishing %s from %s\n' "$plugin" "$(git rev-parse HEAD)"

stage='running the tests'
go test -race ./... -count=1 -timeout=120s

stage='building the Linux binary'
CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/container-log-sanitizer .

stage='creating the local plugin'
bash build-plugin.sh "$plugin" "$data_root"

stage='preparing the canary image'
docker pull busybox:1.37.0

stage='testing the local backend'
bash canary.sh "$plugin" busybox:1.37.0 local

stage='testing the json-file backend'
bash canary.sh "$plugin" busybox:1.37.0 json-file

stage='pushing the plugin'
docker plugin push "$plugin"
printf 'Published %s\n' "$plugin"
