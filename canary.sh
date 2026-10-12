#!/usr/bin/env bash
set -euo pipefail

[[ "$EUID" == 0 ]] || { printf 'Run this canary in a root terminal so it can inspect stored bytes.\n' >&2; exit 1; }
cd -- "$(dirname -- "${BASH_SOURCE[0]}")"
plugin_name=${1:-container-log-sanitizer:dev}
image=${2:?Usage: bash canary.sh PLUGIN_NAME EXISTING_SHELL_IMAGE [local|json-file]}
backend_name=${3:-local}
case "$backend_name" in
  local|json-file) ;;
  *) printf 'Unsupported backend %s; choose local or json-file.\n' "$backend_name" >&2; exit 1 ;;
esac
command -v cmp >/dev/null
command -v jq >/dev/null
command -v timeout >/dev/null
docker version --format 'Docker server {{.Server.Version}}'
plugin_enabled=$(docker plugin inspect "$plugin_name" --format '{{.Enabled}}')
if [[ "$plugin_enabled" != true ]]; then
  printf 'Plugin %s is disabled; the canary has not created a container.\n' "$plugin_name" >&2
  printf 'Install the corrected build under a new plugin name and a separate data directory before retrying.\n' >&2
  exit 1
fi
data_root=$(docker plugin inspect "$plugin_name" --format '{{range .Settings.Mounts}}{{if eq .Name "logdata"}}{{.Source}}{{end}}{{end}}')
[[ -n "$data_root" ]] || { printf 'Plugin has no logdata mount.\n' >&2; exit 1; }
docker image inspect "$image" >/dev/null
results=$(mktemp -d)
container=
follow_container=
cleanup() {
  if [[ -n "$follow_container" ]]; then docker rm -f -v "$follow_container" >/dev/null || true; fi
  if [[ -n "$container" ]]; then docker rm -v "$container" >/dev/null || true; fi
  rm -rf -- "$results"
}
trap cleanup EXIT

printf 'Testing backend %s with plugin %s.\n' "$backend_name" "$plugin_name"
container=$(docker create --pull=never --network none --log-driver "$plugin_name" \
  --log-opt "backend=$backend_name" --log-opt max-size=1m --log-opt max-file=2 --log-opt cache-disabled=true \
  --entrypoint /bin/sh "$image" -c '
    count=0
    while test "$count" -lt 1000; do
      printf "\000\000\000\n"
      count=$((count+1))
    done
    printf "\033[32mREADY\033[0m trace_id=0123456789abcdef0123456789abcdef span_id=0123456789abcdef\n"
    printf "keep\000mixed\n" >&2
  ')
printf '\033[32mREADY\033[0m trace_id=0123456789abcdef0123456789abcdef span_id=0123456789abcdef\n' > "$results/stdout.expected"
printf 'keep\000mixed\n' > "$results/stderr.expected"

docker start "$container" >/dev/null
exit_code=$(docker wait "$container")
[[ "$exit_code" == 0 ]] || { printf 'Canary exited with %s\n' "$exit_code" >&2; exit 1; }
docker logs "$container" > "$results/stdout.actual" 2> "$results/stderr.actual"
cmp "$results/stdout.expected" "$results/stdout.actual"
cmp "$results/stderr.expected" "$results/stderr.actual"
jq -e --arg backend "$backend_name" '.backend == $backend' "$data_root/$container/options.json" >/dev/null
if [[ "$backend_name" == json-file ]]; then
  jq -se 'length == 2 and all(.[]; (.log | type) == "string")' "$data_root/$container/container.log" >/dev/null
fi
stored_bytes=$(stat -c %s "$data_root/$container/container.log")
(( stored_bytes < 4096 )) || { printf 'Unexpected log growth: %s bytes\n' "$stored_bytes" >&2; exit 1; }
printf 'PASS [%s]: 1,000 placeholders absent; ANSI, trace/span IDs, stderr and mixed NUL content intact.\n' "$backend_name"
printf 'PASS [%s]: actual log file contains only %s bytes after the placeholder burst.\n' "$backend_name" "$stored_bytes"

docker start "$container" >/dev/null
exit_code=$(docker wait "$container")
[[ "$exit_code" == 0 ]] || exit 1
docker logs "$container" > "$results/stdout.actual" 2> "$results/stderr.actual"
cat "$results/stdout.expected" "$results/stdout.expected" > "$results/stdout.twice"
cat "$results/stderr.expected" "$results/stderr.expected" > "$results/stderr.twice"
cmp "$results/stdout.twice" "$results/stdout.actual"
cmp "$results/stderr.twice" "$results/stderr.actual"
printf 'PASS [%s]: stopped-container history persists across container restart.\n' "$backend_name"
printf 'Canary container: %s (removed on exit; its small retained log directory is not deleted).\n' "$container"

printf 'Testing live follow for 35 seconds; this must outlast Docker\x27s default 30-second plugin timeout.\n'
follow_container=$(docker create --pull=never --network none --interactive --log-driver "$plugin_name" \
  --log-opt "backend=$backend_name" --log-opt max-size=1m --log-opt max-file=2 --log-opt cache-disabled=true \
  --entrypoint /bin/sh "$image" -c 'printf "FOLLOW-READY\n"; read -r ignored')
docker start "$follow_container" >/dev/null
if timeout --foreground --kill-after=2s 35s docker logs --follow "$follow_container" > "$results/follow.stdout" 2> "$results/follow.stderr"; then
  follow_status=0
else
  follow_status=$?
fi
if [[ "$follow_status" != 124 && "$follow_status" != 137 ]]; then
  printf 'Live follow ended before the canary deadline (exit %s).\n' "$follow_status" >&2
  cat "$results/follow.stderr" >&2
  exit 1
fi
printf 'FOLLOW-READY\n' > "$results/follow.expected"
cmp "$results/follow.expected" "$results/follow.stdout"
if grep -Eq 'context deadline exceeded|Error grabbing logs|error decoding log message' "$results/follow.stderr"; then
  cat "$results/follow.stderr" >&2
  exit 1
fi
printf 'PASS [%s]: docker logs --follow survived 35 seconds without a plugin-client timeout.\n' "$backend_name"
