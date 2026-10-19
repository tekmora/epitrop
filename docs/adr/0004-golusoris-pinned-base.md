# ADR-0004: Base the control plane on golusoris, pinned

- **Status**: Accepted
- **Date**: 2026-10-09, accepted 2026-10-13
- **Authors**: Epitrop maintainers

## Context

The Go control plane ([architecture](../architecture-v2.md)) needs configuration, logging, retry, clocks, identifiers, telemetry, an HTTP client and server, server-sent events, an MCP server, health probes, idempotency, canonical JSON and signed receipts. [golusoris](https://github.com/golusoris/golusoris) already ships each of these. Writing them again would break HISS-19 (one behaviour, one implementation).

golusoris publishes two Go modules from one repository, each with its own tags:

- `github.com/golusoris/golusoris` (root), tagged `vX.Y.Z`.
- `github.com/golusoris/golusoris/core`, tagged `core/vX.Y.Z`.

Release v0.13.0 and core v0.10.0 were cut from the same commit (`f3b3355`). The root `go.mod` at v0.13.0 still requires core v0.9.2 and uses a `replace => ./core` directive. Go ignores `replace` in a dependency, so for a consumer that names only the root module, the root's own requirement selects core v0.9.2. That version lacks `core/retry`, `core/codec/jcs` and `core/tlsx`, and `httpx/server` and `grpc` at v0.13.0 import `core/tlsx`. The go command then fails, or looks up the newest core release, which nothing pins. Only an explicit core requirement gives the pair golusoris tested together.

Epitrop keeps no relational database ([ADR-001](../architecture-v2.md#adr-001-graph-and-event-log-as-source-of-truth)): state lives in an in-memory graph hydrated from Git and an append-only event log.

## Decision

1. `go.mod` requires both modules directly, at exact versions: `github.com/golusoris/golusoris v0.13.0` and `github.com/golusoris/golusoris/core v0.10.0`. They move together, as one update.
2. A capability golusoris ships is taken from golusoris. Epitrop writes only what the [gaps](#gaps-epitrop-writes) section lists.
3. Modules that need Postgres, SQLite, Redis, NATS or another external state service stay out (see [Excluded modules](#excluded-modules)).
4. Existing duplicates move now:
   - [`internal/sources/httpfetch`](../../internal/sources/httpfetch/httpfetch.go) builds its client with `httpx/client.New` and caps bodies with `httpx/client.ReadAllBounded`. It gains `GetWithHeader` for authenticated sources.
   - [`internal/sources/litellmgateway`](../../internal/sources/litellmgateway/source.go) drops its own copy of the bounded GET and calls `httpfetch.GetWithHeader`.
   - The token file and snapshot file reads use `ReadAllBounded`. Every deferred close that joined its error now uses `core/errors.CloseJoin`.

Behaviour kept: no retry and no circuit breaker (zero `RetryOptions` and `BreakerOptions`), the same per-call deadlines and byte caps. Behaviour changed:

- An oversized body fails with an error wrapping `client.ErrBodyTooLarge`.
- A non-positive byte cap is refused instead of accepting an empty body.
- Close errors name the operation.

Tests: `TestGet_Boundary` and `TestGetWithHeader` in [`httpfetch_test.go`](../../internal/sources/httpfetch/httpfetch_test.go), `TestListModels_ResponseCap` in [`source_test.go`](../../internal/sources/litellmgateway/source_test.go).

## Module map

Weights are the third-party modules (golusoris modules excluded) in each package's import closure at the pinned pair, measured with `go list -deps -f '{{with .Module}}{{.Path}}{{end}}' <package>`. "httpx" in issue #30 is a directory, not a package; the rows name its packages.

| Need | golusoris module | Owning go.mod | Used now / planned | Notes |
| :--- | :--- | :--- | :--- | :--- |
| Outbound HTTP | `httpx/client` | root | used now: `internal/sources/httpfetch`, file reads in `litellmgateway` and `cmd/epitropctl` | 25 modules (otelhttp, go-retryablehttp, gobreaker, fx). Retry and breaker off, as before. |
| Close-and-join errors | `core/errors` | core | used now: `httpfetch`, `litellmgateway`, `codexlocal`, `cmd/epitropctl` | `CloseJoin`; 1 module (go-faster/errors). |
| Configuration | `core/config` | core | planned: `cmd/epitrop-server` | koanf: files, environment, Kubernetes secret directories, `*_FILE`; 16 modules. Organisation files under Git (agents, routines, budgets) are domain data for the task-graph store, not process configuration. |
| Logging | `core/log` | core | planned: `cmd/epitrop-server` | slog; 26 modules. `epitropctl` operator output stays plain text on stdout; it is not logging. |
| Retry | `core/retry` | core (new in core v0.10.0) | used now: `internal/eventlog` replay (#221) | Capped exponential backoff with jitter on an injected clock; 6 modules. HTTP calls retry through `httpx/client` `RetryOptions`, not through `core/retry`. |
| Clock | `core/clock` | core | used now: `cmd/epitropctl` sync | clockwork plus fx; 6 modules. `epitropctl sync` reads the clock once and passes that time to every source's `Fetch` (#291). |
| Identifiers | `core/id` | core | planned: task and event ids | UUIDv7 (time-ordered) and KSUID; 7 modules. |
| Telemetry | `otel` | root | used now: `internal/telemetry`, `cmd/epitropctl` | Heaviest: 59 modules (OTLP gRPC exporters, Prometheus). No-op when no OTLP endpoint is set. `httpx/client` emits otelhttp spans to the global provider; `epitropctl` builds the providers with `otel.New`, so they export once an OTLP endpoint is set. |
| REST server | `httpx/server`, `httpx/router`, `httpx/middleware` | root | planned: `cmd/epitrop-server` | `httpx/server` 25 modules, `httpx/router` 6. |
| Server-sent events | `realtime/sse` | root | planned: UI event stream | Standard library only. Buffers 16 events per client and drops on overflow; no `Last-Event-ID` replay (see gaps). |
| In-process pub/sub | `realtime/pubsub` | root | planned: broker fan-out | `LocalBus`: synchronous fan-out in the publisher's goroutine, exact topic match, no buffering, standard library only. |
| MCP server | `core/mcp` | core | planned: `internal/mcp` | Official MCP Go SDK v1.8.0; stdio and streamable HTTP; 32 modules. |
| Health probes | `k8s/health` | root | planned: `cmd/epitrop-server` | `/livez`, `/readyz`, `/startupz` and a drain gate; 26 modules. Does not import the Kubernetes client. |
| Idempotency | `idempotency` | root | planned, partial: inbound REST requests | `Idempotency-Key` middleware with `MemoryStore` or a Epitrop `Store` (atomic `Claim`/`Commit`/`Release`). Does not cover RT-02 (see gaps). The package compiles its Postgres and Redis stores in, so importing it links pgx, rueidis and gRPC: 51 modules. |
| Canonical JSON | `core/codec/jcs` | core (new in core v0.10.0) | planned: event-log records | RFC 8785; standard library only. Canonical bytes before an event is hashed or signed. |
| Signed receipts | `core/crypto/receipt` | core | planned: run receipts | Ed25519 Exit-0 receipts, payload wire-compatible with Praetor's execution receipt; refuses a non-zero exit; 25 modules. |

`httpx/extclient` was considered for `litellm-gateway` and rejected. Its `APIError` puts up to 512 bytes of the response body into the error text, and that source must never surface a gateway reply that can echo a credential. Its 8 MiB cap is also fixed.

## Gaps Epitrop writes

golusoris v0.13.0 has no graph store and no event-sourcing log. In Go source, `git grep -i -E 'topolog|acyclic|kahn' v0.13.0 -- '*.go'` finds a HISS test fixture, a topology comment in `ai/tiny/serve/fleet` and a recursive-type check in `jsonschema`; none of them stores a graph. Epitrop therefore writes:

- **Task-graph store** (`internal/sot`, planned): an in-memory directed acyclic graph hydrated from Git files. It rejects a cycle before the graph changes, bounds traversal depth and stages changes with rollback.
- **Git-backed append-only event log** (RT-04). golusoris `audit` is an append-only audit trail (actor, action, target) behind a store interface meant for Postgres, with a memory store for tests. It has no Git persistence and no replay into state, so it cannot be the source of truth. The event log may feed it later.
- **Broker semantics on top of `realtime/pubsub`.** Issue #30 lists the whole in-process broker as a gap. That is not quite right: the in-process fan-out ships as `realtime/pubsub.LocalBus`, and Epitrop uses it through the `pubsub.Bus` interface. Epitrop adds:
  - log-first publishing: append, then fan out;
  - a bounded queue per subscriber (HISS-06);
  - cursor replay from the event log, which also serves SSE reconnects with `Last-Event-ID`.
- **RT-02 side-effect ledger**: idempotency keys `(task_id, action_type)` on outbound side effects, recorded in the event log. golusoris `idempotency` guards inbound HTTP and gRPC requests only.
- **Regular-file guard for operator-supplied files** (token, snapshot): open, regular-file check and size check before `ReadAllBounded`. golusoris `core/astx.ReadFileBounded` checks the size with a stat, then reads without a read cap or a regular-file check.

## Excluded modules

| Module | Reason |
| :--- | :--- |
| `jobs` (River on Postgres; `jobs/sqlite`) | Relational database (ADR-001). The task graph schedules work. |
| `outbox` (Postgres table, River drainer, `outbox/cdc`) | Relational database (ADR-001). The event log is the outbox. |
| `pubsub/nats`, `pubsub/kafka`, `pubsub/gcp`, `realtime/pubsub/redis` | External broker or store. Durable history is the Git event log; fan-out is in process. |
| `jobs/workflow` | Needs a Temporal service; the task graph is Epitrop's own. |
| `idempotency` Postgres, Redis and SQLite stores | External or relational store (ADR-001). The REST API uses `MemoryStore` or a Epitrop `Store`. |
| `httpx/extclient` | Error text carries response bytes; cap fixed (see [Module map](#module-map)). |

## Updates and Renovate

Renovate is onboarded (`renovate.json`, `config:recommended`). Its `gomod` manager reads the two `require` lines and proposes each new tag as a pull request. `config:recommended` does not know golusoris as a monorepo, so it proposes the two modules separately until a `packageRules` entry groups them. Treat the two modules as one update:

- group them in one pull request;
- run `go mod tidy` after the bump;
- require the repository gates on that pull request.

Root and core releases are cut from one commit, and the root `go.mod` lags the core version it was tested with (see [Context](#context)). Without the grouping rule, a consistent update is a manual `go get github.com/golusoris/golusoris@<tag> github.com/golusoris/golusoris/core@<core tag>` of a release pair, followed by `go mod tidy` and the gates.

## v0.13.0

v0.13.0 is released, so Epitrop starts at v0.13.0. No earlier golusoris code exists here to convert, and the migration plan reduces to that starting point. From [`docs/migrations/v0.13.0.md`](https://github.com/golusoris/golusoris/blob/v0.13.0/docs/migrations/v0.13.0.md), these parts become the starting contract for planned modules:

- `httpx/client`: unsafe methods retry only with an `Idempotency-Key` header or `Retry.AllowUnsafe`.
- `httpx/server`: a zero request-body limit means 10 MiB.
- `realtime/sse`: event IDs and names reject carriage return, line feed and NUL, so event-log IDs must be plain tokens.
- `idempotency`: a custom `Store` implements atomic `Claim`, `Commit` and `Release`; keys include method, host, target, tenant and an optional principal; capture defaults to 1 MiB.

The golusoris epic for v0.13.0, golusoris/golusoris#429, stays open after the release, with children for dependency substitution, gated verification and governance lockdown. A later release can therefore change transitive dependencies; it arrives through the update path above.

The `go` line moves from 1.27 to 1.27.2, because both golusoris modules require Go 1.27.2 or later. A Go 1.27.1 toolchain with `GOTOOLCHAIN=local` refuses the module (`go: go.mod requires go >= 1.27.2`). Both the `golang` builder image and `actions/setup-go` set `GOTOOLCHAIN=local`, so:

- the [`Dockerfile`](../../Dockerfile) builder moves to the `golang:1.27.2-trixie` index digest;
- [`ci.yml`](../../.github/workflows/ci.yml) reads the version from `go.mod` (`go-version-file`), as `security.yml` already does.

`praetor-api.yml` asks for `stable`, which setup-go resolves through the actions/go-versions manifest; it needs that manifest to list Go 1.27.2.

## Licence

golusoris code is EUPL-1.2 from v0.9.0 on (`LICENSING.md` at v0.13.0), the same licence as Epitrop. The licence texts are byte-identical (`cmp LICENSES/EUPL-1.2.txt`). The third-party modules now built into Epitrop (`go list -deps -test ./...`) are under MIT, BSD-3-Clause, Apache-2.0 and MPL-2.0 (go-cleanhttp, go-retryablehttp). MPL-2.0 is on the EUPL-1.2 Appendix of compatible licences; the permissive licences carry notice obligations only. No conflict.

## Consequences

- **Positive**: The control plane starts on tested modules for config, logging, telemetry, server, health, MCP and idempotency instead of new code. The catalog sources share one HTTP client stack with OTel spans, ready for retry and a breaker when a source needs them. Oversized bodies are now distinguishable with `errors.Is`.
- **Negative**:
  - Epitrop's build list grows from zero to 25 third-party modules for the code that exists today; each planned module above adds its weight.
  - Importing `idempotency` links Postgres and Redis drivers that Epitrop never uses.
  - Two modules must move in step, and golusoris is pre-1.0, so a minor release can break the API (v0.13.0 did).

## Follow-ups

- [x] Inject `core/clock` into the sources and `sync`: done in #291.
- [x] Ask golusoris to move the `idempotency` stores into subpackages, so the middleware links no database driver: asked in golusoris/golusoris#818; the stores moved into their own packages in golusoris/golusoris#939, released in v0.18.0. Epitrop imports `idempotency` nowhere yet.
- [x] Ask golusoris for a core-level bounded reader with a regular-file guard, so `ReadAllBounded` need not come from `httpx/client` for file reads: asked in golusoris/golusoris#1011.
- Switch the file reads in `litellmgateway` and `cmd/epitropctl` to that reader once golusoris/golusoris#1011 ships.
- [x] Add the Renovate `packageRules` entry that groups `github.com/golusoris/golusoris` and `github.com/golusoris/golusoris/core` into one pull request: `renovate.json` group `golusoris`, done in #291.
