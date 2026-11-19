---
title: Epitrop documentation
description: Agent runtime and task-graph engine for the Tekmora ecosystem — Git-native control plane, signed event log, sandboxed job supervision and model catalog.
---

# Epitrop

Epitrop is the agent runtime and task-graph engine for the Tekmora ecosystem.
It reads its organisation, routines and budgets from files in Git and rebuilds
state from an append-only, Ed25519-signed event log, without a database setup
wizard.

Status: early. Today the repository ships the Go control plane (`cmd/epitropctl`,
`cmd/epitrop-server`), the model catalog (`catalog/`, `internal/sources/`, moved
from `cordanaLLM/praetor` with its history), the signed event log, the acyclic
task graph, the sandboxed job supervisor, the agent slot runner and the MCP
server. The runtime is planned across milestones 0.1 to 0.6; see the
[roadmap](roadmap.md).

## Why Epitrop

- **Git-native configuration, no relational database**: process configuration is
  validated against `docs/config.schema.json` before decoding, and organisation,
  routines and budgets live in files in Git ([Description](epitrop-description-v2.md)).
- **Event-sourced state (RT-04)**: every task-graph edit, work-unit transition,
  job lifecycle event and outbound side-effect claim is recorded in a
  JCS-canonicalised (RFC 8785), Ed25519-signed event log (`internal/eventlog`)
  with segment rotation ([ADR-0010](adr/0010-event-log-segments.md)), Git
  checkpoint commits onto `refs/epitrop/events`
  ([ADR-0011](adr/0011-event-log-git-commits.md)) and signed base compaction
  ([ADR-0012](adr/0012-event-log-compaction.md)).
- **Acyclic task graph and deterministic frontier**: `internal/graph` enforces
  Kahn acyclicity, bounded depth and staged rollback, and `internal/score`
  orders ready work units deterministically from event-log records alone
  ([Architecture](architecture-v2.md)).
- **Control-plane isolation and sandboxed supervision (RT-03)**: declared
  background jobs and headless agent slots (`internal/supervisor`,
  `internal/slot`) start through a per-job shim in Linux `systemd-run` user
  scopes and `bubblewrap` sandboxes ([ADR-0005](adr/0005-sandboxed-job-runner.md))
  with `none`, `egress` or Unix-socket `proxy` network modes
  ([ADR-0006](adr/0006-proxy-socket-egress.md)), unchanged arguments
  ([ADR-0007](adr/0007-job-arguments-unchanged.md)), declared input roots
  ([ADR-0008](adr/0008-sandbox-input-roots.md)) and read-only bound logins
  ([ADR-0009](adr/0009-slot-client-home.md)).
- **Deterministic edges and at-most-once side effects (RT-01, RT-02)**:
  `internal/edges` checks model calls for temperature 0 and JSON Schema
  validation, `internal/effects` runs each `(task_id, action_type)` side effect
  at most once, and `internal/broker` provides log-first fan-out with bounded
  subscriber queues and dead-letter classification (`internal/jobfailure`).
- **Router-alias model routing and measured budgets**: every model call goes
  through a router alias over one dynamic accelerator pool, platform budgets
  (`internal/budget`) read the Praetor efficiency ledger (`internal/ledger`),
  and the model catalog collects live usage windows and burn rates without
  silent fallbacks ([Catalog data sync](data-sync.md)).

## Documentation map

| Section | Page | What it covers |
| --- | --- | --- |
| Architecture | [Description](epitrop-description-v2.md) | Core idea, six design pillars and EUPL-1.2 licensing |
| Architecture | [Control Plane & Invariants](architecture-v2.md) | Go control plane, event log, agent slots and runtime invariants RT-01 to RT-04 |
| Architecture | [Repository Layout](repository-layout-v2.md) | Current packages, binaries and planned Rust and Svelte 5 directories |
| Architecture | [Sources](sources-list-v2.md) | Verified reference links behind the Epitrop design |
| Guides & Operations | [Agent Onboarding](agent-onboarding.md) | Package map, HISS limits, RT invariants and verification workflow |
| Guides & Operations | [Catalog Data Sync](data-sync.md) | Installing `epitropctl`, running `sync`, `show`, `headroom` and `claude-limits` |
| Guides & Operations | [MCP Server](mcp-server.md) | Serving the run record, health probes and `/events` SSE stream with `epitrop-server` |
| Guides & Operations | [Telemetry](telemetry.md) | OpenTelemetry spans and metrics exported over OTLP |
| Guides & Operations | [Cutover 1](cutover-1.md) | Moving fleet watchers and background jobs to the Epitrop supervisor |
| Guides & Operations | [Releasing](releasing.md) | Tagging, keyless cosign signing, SLSA provenance verification and upgrade fixtures |
| Guides & Operations | [Roadmap](roadmap.md) | Milestones 0.1 to 0.6, critical path and Praetor interfaces |
| Decisions (ADRs) | [ADR Index](adr/README.md) | Accepted architectural decision records ([ADR-0000](adr/0000-template.md) and [ADR-0004](adr/0004-golusoris-pinned-base.md) to [ADR-0012](adr/0012-event-log-compaction.md)) |
