# Container Log Sanitizer: Selectable-Backend Prototype

A Docker logging-driver prototype based on [bgalek/container-log-sanitizer](https://github.com/bgalek/container-log-sanitizer), retaining the Apache-2.0 license. It filters NUL placeholders and optionally redacts complete records before storing them through Moby's `local` or `json-file` backend. This is not an upstream release or a compliance boundary.

## Data Path

```text
application + optional Beyla -> Docker FIFO -> placeholder filter -> optional redaction
  -> rotating Moby local or json-file store
docker logs / compose logs / collectors <- ReadLogs endpoint <- stored records
```

The filter runs before persistence. It drops complete records only when they contain at least one NUL and all other bytes are ASCII whitespace. It does not strip ANSI codes, rewrite trace/span IDs, drop normal blank lines, or alter mixed text/NUL records. No application changes are needed. Beyla enrichment and its required capabilities can stay enabled.

## Select A Backend

Set `--log-opt backend=local` or `--log-opt backend=json-file` on containers using this plugin. Omitting the option selects `local`. Both backends share the same pre-storage filter, optional redaction, and Docker `ReadLogs` API; unsupported backend names are rejected.

The plugin delegates to the bundled Moby storage implementations, not to another installed logging plugin. The container must still select the sanitizer as its logging driver. It has no remote destination and does not require Alloy; any compatible Docker log collector can read the retained records.

Both formats live at `<log-root>/<container-id>/container.log`, with rotation files beside them. `local` uses Moby's compact binary format; `json-file` writes newline-delimited JSON. The saved `options.json` records the backend for readback and plugin restart. Legacy options without a backend mean `local`. Changing the backend for the same stored container ID is rejected to avoid mixing file formats; recreate the container to switch, preserving any history you need first.

Only the common storage options listed below are supported. Their defaults belong to this wrapper and do not inherit Docker daemon defaults. Backend-specific remote forwarding, tags, and arbitrary driver chaining are not implemented.

`docker attach` and attached `docker compose up` bypass logging-driver readback and can still display Beyla placeholders. Use `docker logs` or `docker compose logs` for filtered history. This driver cannot recover missing Beyla replacement writes; keep monitoring the enricher separately.

## Build And Test

Use Go 1.24 or newer on Linux or in WSL. Shell scripts are checked out with LF line endings.

```sh
go test ./...
go test -race ./...
CGO_ENABLED=0 GOOS=linux go build -trimpath -o dist/container-log-sanitizer .
```

Race tests need a C compiler. On hosts where `/tmp` is mounted `noexec`, point `GOTMPDIR` at an executable directory owned by your user. Dependencies are pinned in `go.mod` and `go.sum`; the codec and both Moby stores use Docker 28.5.1 to avoid duplicate protobuf registration across module versions. These internal Moby APIs require compatibility testing when updating dependencies. Both backends must pass the canary on the target Docker daemon before rollout.

## Install A Canary Plugin

Run from this directory in a root terminal on the target Linux Docker host. This creates a managed plugin and a dedicated root-owned log directory, not a daemon-wide logging change. The plugin needs no network access, Docker socket mount, or extra capabilities. The installer requires Docker and `jq` and refuses unmarked nonempty data directories.

```sh
run_id=$(date +%s) &&
plugin="container-log-sanitizer:backends-$run_id" &&
bash build-plugin.sh "$plugin" "/var/lib/container-log-sanitizer-canary-$run_id"
```

The installer uses `dist/container-log-sanitizer` if present; otherwise it builds and tests with the pinned Go Docker image. Only use a Linux binary matching the host architecture. Existing plugin names are rejected instead of overwritten. Do not point the data directory at existing application data.

The installer enables the plugin with `docker plugin enable --timeout 0`. Docker's default is a 30-second total HTTP client timeout, which also applies while reading the `ReadLogs` response body and disconnects long-running `docker logs -f` streams. Zero removes that total deadline; flushing or sending heartbeats cannot extend a total client timeout. This is an enable-time setting, not a logging option or a socket timeout in the plugin. Removing the client deadline also affects other plugin RPCs, so operational commands against an unresponsive plugin may need an external timeout.

Existing plugins enabled with the old installer must be disabled and re-enabled with `--timeout 0` only after all containers using them have been stopped, without `--force`. Restart those containers afterward so Docker creates new logging clients. Alternatively, install a fresh plugin name with this installer and recreate only the pilot service against that name. Changing this script does not update an already enabled plugin.

Use an already present image containing `/bin/sh`. For example, if a container named `postgres` exists, reuse its image without its entrypoint or data mounts:

```sh
image=$(docker inspect -f '{{.Image}}' postgres) &&
bash canary.sh "$plugin" "$image" local &&
bash canary.sh "$plugin" "$image" json-file
```

Each canary requires `cmp`, `jq`, and `timeout`. It emits 1,000 NUL placeholders, one colored stdout record with IDs, and a mixed-NUL stderr record. It compares exact `docker logs` bytes, checks the saved backend and storage size, and verifies retained history after restarting only that canary. The JSON canary also parses the actual file and checks that it contains only the two retained records. No image is pulled by the canary. The stopped canary is removed; its small retained log directory remains for inspection. The third argument chooses the backend and defaults to `local`.

Each backend also gets a separate interactive test container to hold a live log stream open for 35 seconds. The canary verifies that it receives the initial marker and that Docker does not terminate the stream at its former 30-second deadline. The external timeout intentionally ends the reader; cleanup removes only the disposable canary containers. Allow about 70 seconds for the two-backend follow checks.

Unit/integration tests use real FIFOs, HTTP RPCs, and the real Moby store. The disk-size test proves that 1,000 rejected records add zero bytes to the store. Host installation, reboot behavior, and a real Beyla-instrumented pilot require separate validation on the target Docker daemon.

## One-Service Pilot And Rollback

Only after both synthetic canaries pass, set `SANITIZER_PLUGIN` to the new installed plugin name, apply this logging configuration to one noncritical service, and recreate that service. A restart alone does not change its logging driver or backend. Preserve existing volumes and archive any history needed before recreation. Do not use `down -v`.

```yaml
logging:
  driver: "${SANITIZER_PLUGIN:?Set the installed sanitizer plugin name}"
  options:
    backend: "json-file"
    max-size: "10m"
    max-file: "3"
    compress: "true"
    cache-disabled: "true"
```

The plugin advertises native `ReadLogs`, so Docker's unfiltered dual-logging cache is not used. The explicit cache option documents that requirement. Existing `loki.source.docker` collection can continue; its container selector must include the pilot service.

Before wider rollout verify colors in a real terminal, trace/span IDs in Loki, `logs -f`, time/tail selection, plugin and daemon restart recovery, and disk limits. Do not force-disable a plugin used by running containers. To roll back, restore the service's original logging stanza and recreate only that service. Disable/remove the plugin only after no containers use it. Retained files are not automatically deleted when containers are removed.

## Options And Limits

| Option | Default | Meaning |
| --- | --- | --- |
| `backend` | `local` | Bundled storage implementation: `local` or `json-file` |
| `max-size` | `10m` | Rotation size, plus at most one record |
| `max-file` | `3` | Retained files per container |
| `compress` | `true` | Compress rotated logs |
| `drop-nul-placeholders` | `true` | Drop only complete NUL/ASCII-whitespace records containing a NUL |
| `sanitize-patterns` | `[]` | JSON array of Go regexes; matched bytes become `***` |

Redaction is opt-in and applies to complete records only. Partial fragments pass through unchanged, including NUL-only fragments. There is no cross-fragment secret-redaction guarantee; this prototype is not a compliance boundary. Numeric byte content is never interpreted as severity. Application log content and regex values are not printed in plugin diagnostics.

Readback follows the selected backend's native format: `local` retains Moby partial-record metadata, while `json-file` retains fragment bytes but does not store that metadata. Complete-record colors, timestamps, stream identity, and trace/span IDs are preserved by both backends.

Storage is bounded per container, not across an unlimited number of removed containers. Retained directories need an operator-approved cleanup policy. The plugin preserves active FIFO registrations across its own restart. In-flight messages can still be lost on abrupt process/host failure. A failed or unavailable plugin may eventually backpressure applications through Docker's FIFO; nonblocking Docker logging trades that risk for possible dropped messages and must be chosen deliberately.

The Unix-only `/healthz` endpoint reports stored records, dropped placeholders, and failed streams. A malformed or oversized frame terminates that stream with a diagnostic instead of guessing a new framing boundary. Readback errors are reported in plugin diagnostics and close the read stream.

## Redactor Package

`redactor.New(patterns)` rejects empty or invalid expressions and applies Go regular expressions in order. `RedactLine` handles strings and `RedactBytes` handles byte slices; unmatched content is unchanged. The legacy `NewRedactor()` constructor reads semicolon-separated `SANITIZE_PATTERNS`, trimming whitespace and ignoring empty entries. The managed plugin uses the per-container `sanitize-patterns` logging option instead of that environment variable.

## Maintenance

Choose a versioned plugin name before sharing builds. Review plugin upgrades, storage compatibility, and failure tests before changing shared logging configuration. Keep the existing Apache-2.0 license and upstream attribution when distributing derived builds.
