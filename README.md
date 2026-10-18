# Epitrop

<!-- praetor:readme-governance:start -->
[![HISS Adopted][praetor-hiss-badge]][praetor-hiss-agents]
[![Documentation Governance][praetor-docs-badge]][praetor-docs-runs]

Praetor manages this repository's declared governance policy. This managed
block records adoption state; it is not a verification certificate.

**Verification**: `make verify-all` runs the repository's configured
verification cascade.

**HISS Audit**: `praetorctl audit` enforces policy, generated-surface
integrity, and the debt ratchet.

**Context Sync**: `praetorctl compile-context --verify` verifies every
generated agent context against `AGENTS.md`.

**Documentation**: `make docs-lint` enforces locked Markdown style and the
private scratch-link policy.

**Debt Baseline**: `.standards-baseline.json` anchors the debt ratchet at
0 recorded infractions; audit forbids growth.

[praetor-hiss-badge]: https://img.shields.io/badge/Standards-HISS%20Adopted-blue
[praetor-hiss-agents]: https://github.com/tekmora/epitrop/blob/HEAD/AGENTS.md
[praetor-docs-badge]: https://github.com/tekmora/epitrop/actions/workflows/praetor-docs.yml/badge.svg
[praetor-docs-runs]: https://github.com/tekmora/epitrop/actions/workflows/praetor-docs.yml
<!-- praetor:readme-governance:end -->

Agent runtime and task-graph engine for the Tekmora ecosystem.

Status: early. Today the repository holds the model catalog (`catalog/`, `cmd/epitropctl`, `internal/sources/`), moved from `cordanaLLM/praetor` with its history. The runtime is planned in milestones 0.1 to 0.6; see the [roadmap](docs/roadmap.md).

## The name

The runtime is called **Epitrop** (eh-PEE-trop), from Greek *epitropos*, the
appointed trustee. Tekmora (tek-MOH-ra), the family the name belongs to, comes
from Greek *tekmerion*, "decisive proof". The repository was first named
`tribunus`, in another organisation; its old address redirects.

## Build and test

```bash
go vet ./...
go test -race ./...
```

## CLI usage

```bash
epitropctl sync [--sources=a,b] [--out=file]
epitropctl show [--in=file]
epitropctl headroom --max-age=duration [--in=file]
epitropctl schema
epitropctl version
epitropctl jobs start|status|stop --config=file [name]
epitropctl jobs history --config=file [--since=RFC3339] [name]
epitropctl jobs supervise --config=file [--poll-interval-seconds=1]
```

`epitropctl sync` writes a versioned catalog snapshot with `schema_version: 1`.
The contract is described by `catalog/snapshot.schema.json`, and
`epitropctl schema` prints the embedded copy for downstream pin checks.
`epitropctl version` prints the release version, or `dev` for a source build.
`epitropctl jobs supervise` runs until SIGTERM or an interrupt, which end it with exit 0, so a host service unit can hold it.
Releases, their signed artefacts and how to verify them are described in [Releasing](docs/releasing.md).

`epitropctl` exports OpenTelemetry traces and metrics over OTLP once
`OTEL_EXPORTER_OTLP_ENDPOINT` or a signal-specific endpoint is set. With no endpoint set, nothing is exported.
[Telemetry](docs/telemetry.md) lists the spans and metrics.

## MCP server

```bash
epitrop-server --config=file
```

`epitrop-server` serves the run record over the Model Context Protocol on stdin and stdout, for a client that starts it as a subprocess. By default its tools only read, and no tool appends to the event log or signals a job:

- `jobs_status` lists the configured jobs as `epitropctl jobs status` does, including `stale` and `last_beat` for a job that declares `heartbeat.max_age_seconds`. On a platform without the supervisor (anything but Linux) the tool returns the reason as a tool error.
- `units_list` lists the work units of the run record, sorted by id, optionally narrowed to one status.
- `unit_status` shows one unit as `epitropctl units show` does, with the count of unread notes in place of the notes. Reading does not mark a note read.
- `runs_list` lists the slot runs, one per unit whose evidence pointer names a run's start file (`started`) or report (`ended`), sorted by unit id, optionally narrowed to one phase.
- `run_status` shows one unit's slot run: the start record while it runs, the runner's report once it has ended. A report whose digest does not match the unit's evidence pointer is refused.
- `watches_list` lists the release watch routes of `release_watch.routes` with their sources and sink kinds.
- `watch_status` shows one route: per source, how many releases the seen-set holds and the one seen last, and its sinks. An ntfy sink shows only its kind, since its topic is all a reader needs to read the notifications.

With `server.transport: http` in the config, the server serves the same tools over streamable HTTP on `server.listen` (default `127.0.0.1:8899`) and prints the address it serves. The server has no authentication, so the config refuses a listen address whose host is not loopback, and the server refuses a request whose `Host` header names another host. Beside the tools at `/mcp` it serves:

- `/livez`, `/readyz` and `/startupz`. `/readyz` answers 503 until the event log has replayed, and again whenever a replay fails; the server replays every two seconds.
- `/events`, a server-sent event stream with one `unit` event per unit and one `job` event per job whose summary changed since the last replay. The stream starts with the state at the first replay and sends no snapshot; a client that falls behind loses events and reads the state back with the tools.

With `server.allow_mutations: true` the server also serves three tools that change state:

- `jobs_start` starts one configured job and leaves a running job alone; `jobs_stop` stops one with its stop signal, the grace period and then a kill. The supervisor starts the job shim as `epitrop-server` itself.
- `unit_set` records a unit's status, under the same transition rules as `epitropctl units record`.

Each call names a `key` for the request. The effect ledger runs a key with the same arguments at most once: a repeat answers `skipped` and changes nothing, and the same key with other arguments is a new request. A call that failed leaves its key open, and a repeat is refused until the claim is resolved with `epitropctl effects resolve`.

The tool names and config keys are fixed; the HTTP paths stay provisional until the first release that ships the server.

## Documents

- [Agent onboarding](docs/agent-onboarding.md)
- [Roadmap](docs/roadmap.md)
- [Cutover 1](docs/cutover-1.md): the background jobs Epitrop takes over, and the parallel run before each switch-over
- [Catalog data sync](docs/data-sync.md): installing `epitropctl`, the sources and their limits
- [Releasing](docs/releasing.md)
- [Architecture](docs/architecture-v2.md)
- [Telemetry](docs/telemetry.md): spans and metrics, and how to export them
- [Repository layout](docs/repository-layout-v2.md)
- [Description](docs/epitrop-description-v2.md)
- [Sources](docs/sources-list-v2.md)

## Licence

EUPL-1.2, for code and documents alike. See `LICENSE` and `REUSE.toml`.
Third-party code that `epitropctl` includes, with the notice each licence asks to carry, is listed in
`NOTICE.md`.
