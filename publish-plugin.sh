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

for dependency in dotnet-gitversion docker go jq regctl; do
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
created=$(date -u +%Y-%m-%dT%H:%M:%SZ)
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

stage='adding Zot manifest annotations'
regctl manifest get "$plugin" --format raw-body > "$staging/manifest.json"
jq -e --slurpfile config "$staging/config.json" --arg version "$version" --arg created "$created" '
  if .mediaType != "application/vnd.docker.distribution.manifest.v2+json" or
     .config.mediaType != "application/vnd.docker.plugin.v1+json" then
    error("Expected a Docker managed-plugin manifest")
  else
    .annotations = ((.annotations // {}) + {
      "org.opencontainers.image.title": "Container Log Sanitizer",
      "org.opencontainers.image.description": $config[0].description,
      "org.opencontainers.image.documentation": $config[0].documentation,
      "org.opencontainers.image.source": "https://github.com/kritikos-io/container-log-sanitizer",
      "org.opencontainers.image.licenses": "Apache-2.0",
      "org.opencontainers.image.version": $version,
      "org.opencontainers.image.created": $created
    })
  end
' "$staging/manifest.json" > "$staging/manifest-annotated.json"
regctl manifest put "$plugin" \
  --content-type application/vnd.docker.distribution.manifest.v2+json \
  < "$staging/manifest-annotated.json"
printf 'Published %s; the local plugin remains disabled.\n' "$plugin"
