# Repository layout

`[now]` exists today. `[planned]` is a target for a milestone.

```text
epitrop/
  .github/workflows/ci.yml   [now]     go vet, go test -race
  LICENSE, LICENSES/         [now]     EUPL-1.2
  REUSE.toml                 [now]
  AGENTS.md                  [now]     canonical agent briefing (Praetor)
  Makefile                   [now]     verify-all target (Praetor)
  catalog/                   [now]     model catalog records and snapshots
  cmd/epitropctl/           [now]     operator CLI (show, sync)
  cmd/epitrop-server/       [now]     MCP server on stdio or loopback HTTP, probes, SSE, opt-in mutating tools
  internal/sources/          [now]     catalog sources (gateway, local, public)
  internal/sot/              [planned] task graph and state
  internal/mcp/              [now]     MCP tools over the run record
  internal/logsetup/         [now]     opens the shared event log for every binary
  internal/telemetry/        [now]     spans and metrics for runs, tasks and side effects
  crates/epitrop-graph/     [planned] Rust vector kernel
  ui/                        [planned] Svelte 5 operator UI
  docs/                      [now]
```

## Notes

- Config lives in files under Git. A Epitrop config format is defined in milestone 0.2; Paperclip files are not required.
- `internal/` packages follow the HISS code standards (function limit 60 lines).
- Runtime invariants RT-01 to RT-04 are checked by fixtures, not by HISS scans.

## Build targets (planned)

```makefile
.RECIPEPREFIX = >
verify-all: lint test reuse
build:
> go build -o bin/epitropctl ./cmd/epitropctl
> cargo build --release --workspace
> npm run --prefix ui build
```
