# Roadmap

Epitrop grows from the model catalog into an agent runtime in six milestones. Each milestone has one epic issue that lists its children; the GitHub milestones carry the same scope.

## Milestones

| Milestone | Epic | Outcome |
| --- | --- | --- |
| 0.1 Foundation | [#4](https://github.com/tekmora/epitrop/issues/4) | Praetor adoption and CI gates; a catalog without silent fallbacks that is the one model catalog; docs that match the code; golusoris as the pinned base. |
| 0.2 Durable core | [#8](https://github.com/tekmora/epitrop/issues/8) | Config with a published schema; an append-only event log in Git; the acyclic task graph; RT-01 to RT-03; budgets from the Praetor ledger; telemetry. |
| 0.3 Supervisor | [#13](https://github.com/tekmora/epitrop/issues/13) | Declared jobs that survive restarts; event watches with proven silence; checkpoint and resume; an MCP and SSE server. First cutover: watchers and supervision. |
| 0.4 Queue and runner | [#37](https://github.com/tekmora/epitrop/issues/37) | An in-process broker with backpressure and dead-letter classes; the sandboxed runner; task scoring; A2A. Cutovers: the landing queue, then the wave runner. |
| 0.5 Routing and admission | [#16](https://github.com/tekmora/epitrop/issues/16) | Router-alias routing; the quota and headroom collector; probe-based admission; ledger comparison against frontier-only routing. |
| 0.6 Operator surface | [#20](https://github.com/tekmora/epitrop/issues/20) | The Svelte 5 operator UI on sveltesentio; the Rust skill-vector kernel. |

## Order of work

The critical path runs through the event log: adoption (#26) and the golusoris base (#30) come first, then config (#31) and the event log (#32). Supervision (#21), checkpoints (#11) and the broker (#9) build on the log.

Inside 0.1, the catalog fixes (#27, #28) and the docs (#3) are independent of each other and can run in parallel with adoption.

The three cutovers each have their own issue and retire a set of operator scripts:

1. Watchers and supervision ([#36](https://github.com/tekmora/epitrop/issues/36)); plan in [cutover-1.md](cutover-1.md).
2. The landing queue ([#38](https://github.com/tekmora/epitrop/issues/38)).
3. The wave runner ([#39](https://github.com/tekmora/epitrop/issues/39)).

Each cutover runs in parallel with the script it replaces until the events match, then the script is retired.

## Interfaces with Praetor

Praetor owns governance: gates, the planning graph, the efficiency ledger, the invocation and agent-loop budget layers, the dispatch hook, issue claims and the plan notebook. Epitrop owns the runtime and the model catalog. Every interface Epitrop consumes from Praetor is listed, with its version contract, in [cordanaLLM/praetor#1045](https://github.com/cordanaLLM/praetor/issues/1045).

## Reuse

The Go control plane builds on golusoris, pinned. It uses these modules:

- `core/config`, `core/log`, `core/retry`, `core/clock` and `core/id`
- `otel`
- `httpx`, `realtime/sse` and `core/mcp`
- `k8s/health`
- `idempotency`
- `core/codec/jcs` and `core/crypto/receipt`

Epitrop writes only what golusoris does not ship: the task-graph store, the Git-backed event log and the in-process broker. The operator UI builds on sveltesentio. See [#30](https://github.com/tekmora/epitrop/issues/30) for the module map.

## Rules that bind every milestone

- Every model call goes through a router alias. No concrete model name appears in code or config.
- Accelerators are one dynamic pool, placed by measured free memory. Work is admitted only when the resolved model answers a bounded probe.
- Published upstream schemas are the source of truth for every external format.
- No silent fallback: an unavailable input fails, or the output names the substitution.
- Runtime invariants RT-01 to RT-04 live here; the HISS code standards come from Praetor.
