<!-- SPDX-FileCopyrightText: 2026 lusoris <lusoris@pm.me> -->
<!-- SPDX-License-Identifier: EUPL-1.2 -->

# Telemetry

Epitrop instruments the control plane using OpenTelemetry traces and metrics. Telemetry provides visibility into unit slot runs, outbound side effects, supervised job executions, and task graph queue depth.

## Enabling Export

Telemetry export is disabled by default. With no OTLP endpoint set, nothing is exported and no connection is opened.

To export traces and metrics to an OpenTelemetry collector, set the standard OpenTelemetry endpoint environment variable:

```bash
export OTEL_EXPORTER_OTLP_ENDPOINT="http://localhost:4317"
```

Signal-specific endpoints such as `OTEL_EXPORTER_OTLP_TRACES_ENDPOINT` or `OTEL_EXPORTER_OTLP_METRICS_ENDPOINT` are also honored. Export uses OTLP over gRPC; epitropctl opens no metrics listener or scrape endpoint.

`epitropctl` sets up the exporters once per command and flushes them when the command exits, waiting at most five seconds. The internal job shim and the egress relay are child processes and export nothing. Once an endpoint is set, the catalog sync's HTTP client spans (golusoris `httpx/client`, otelhttp) are exported as well.

## Spans and Traces

Traces record execution duration and status across the primary lifecycle paths of the runtime. Spans capture contextual attributes such as unit identifiers, job names, and effect idempotency keys. High-cardinality keys are attached to spans rather than metric series.

| Span Name | Attributes | Description |
| :--- | :--- | :--- |
| `effects.do` | `epitrop.effect.key` | Execution of an outbound side effect under an idempotency key. |
| `slot.run` | `epitrop.unit` | Execution and verification of a unit workspace run. |
| `supervisor.job.start` | `epitrop.job` | Attempted start and preflight of a supervised background job. |

## Metrics Catalog

Metric definitions are declared in the central catalog with bounded label cardinality. Any unrecognized label value is safely folded into `other` to protect metric storage.

| Name | Kind | Labels | Help |
| :--- | :--- | :--- | :--- |
| `epitrop_effect_runs_total` | counter | `outcome` (`executed`, `failed`, `refused`, `skipped`) | Side effect runs through the effects ledger, by outcome. |
| `epitrop_slot_runs_total` | counter | `outcome` (`blocked`, `done`, `failed`, `not-taken`, `preempted`, `refused`) | Slot runs that were recorded, by outcome. |
| `epitrop_job_runs_total` | counter | `outcome` (`failed`, `refused`, `started`) | Supervisor job start attempts, by outcome. |
| `epitrop_checkpoint_writes_total` | counter | `outcome` (`failed`, `written`) | Event log checkpoint write attempts, by outcome. |
| `epitrop_admission_decisions_total` | counter | `outcome` (`admitted`, `refused`, `unknown`) | Admission decisions for router aliases, by outcome. |
| `epitrop_budget_used_frontier_tokens` | gauge | `layer` (`platform`) | Frontier tokens a budget layer has used, as the Praetor efficiency ledger measured them; no value while the ledger has not measured them. |
| `epitrop_graph_tasks` | gauge | `status` (`blocked`, `claimed`, `dropped`, `fix-round`, `implementing`, `landed`, `landing`, `proposed`, `queued`, `ready`, `review`) | Tasks in the task graph per status after the last committed change (queue depth). |

The admission gate (`internal/admission`) counts one decision each time it admits or refuses work for a router alias.

The budget check (`internal/budget`, [#6](https://github.com/tekmora/epitrop/issues/6)) writes `epitrop_budget_used_frontier_tokens` each time it reads a ledger that measured frontier tokens. While the ledger reports them as not measured, the check refuses the task and the gauge keeps no value, so a dashboard shows a gap rather than zero use. With the platform budget off (`budgets.platform.frontier_tokens: 0`), the check reads nothing and writes nothing.
