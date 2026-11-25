# F4 ranked frontier

**Verdict:** Steps 1–3 implemented on `feat/ranked-frontier-next` at `99f201d`. Ready for the dispatching judge. Verification is pending. Tier: reversible. No commit or push.

## Ranked read and claim

`units frontier` prints ranked ID/tab/quoted-title lines. `units frontier --json` returns the following envelope for the five-node fixture after its one stage.

```json
{
  "algorithm_version": "v1",
  "log_position": 1,
  "rows": [
    {"id":"z","title":"Task z","terms":{"elapsed_seconds":0,"progress":2,"cost":0,"confident":true}},
    {"id":"a","title":"Task a","terms":{"elapsed_seconds":0,"progress":0,"cost":0,"confident":true}},
    {"id":"b","title":"Task b","terms":{"elapsed_seconds":0,"progress":0,"cost":0,"confident":true}}
  ]
}
```

The position is the sequence checked against HEAD by the successful graph-fold replay. Each replay attempt allocates fresh facts. With no records after a compacted base, its cursor still supplies the sequence, the graph supplies titles and progress, and historical cost and elapsed time are unknown (zero, with confidence false). `internal/score/score.go` uses the same ordering function for the read and `Rank`. Eligibility comes from `graph.Frontier` and `graph.FrontierRefusal`.

`units assign --next` selects the first row. A named assignment selects that node's own row. The checked append compares source and current sequence under the lock, checks graph eligibility and unit availability, and writes one `unit.assigned` record. The following are its decision fields, shown without the event log's signature and hash-chain fields.

```json
{
  "type": "unit.assigned",
  "task_id": "r:z",
  "payload": {
    "node": "z",
    "lane": "lane-1",
    "by": "actor-1",
    "selection": {
      "mode": "next",
      "algorithm_version": "v1",
      "log_position": 1,
      "terms": {"elapsed_seconds":0,"progress":2,"cost":0,"confident":true}
    }
  }
}
```

The named override in the fixture records `mode: named`, node `b`, task ID `r:b` and progress zero. The actor is optional. The unit fold retains the original payload under `assignment`. Show returns that object and resume appends node, current actor and assignment columns. Later updates to the current lane and actor preserve the original assignment. An older assignment event decodes with its actual node, lane and optional actor and no selection. Older checkpoint states can lack the original payload.

## Changed paths

| Paths | Change |
| --- | --- |
| `internal/eventlog/replay.go`, `internal/graphlog/service.go` | Verified sequence and per-attempt graph reducer |
| `internal/score/score.go` | Version, titled rows, ranked read and row selection |
| `cmd/epitropctl/units_frontier.go` | Text and JSON output, exclusive named/next selection |
| `internal/graphlog/assign.go`, `internal/graphlog/assign_reducer.go` | Locked position check, retryable outcome and shared event decoder |
| `internal/eventlog/types.go`, `internal/units/assign.go` | Wire terms, selection evidence and immutable assignment payload on the unit |
| `internal/units/units.go`, `internal/units/service.go`, `cmd/epitropctl/units.go` | Resume rows and CLI rendering |
| `cmd/epitropctl/units_frontier_test.go`, `internal/score/score_test.go` | Signed replay fixtures, race, compatibility and refusal cases |
| `docs/agent-onboarding.md`, `docs/architecture-v2.md`, `changelog.d/20261125-38-ranked-frontier.yaml` | CLI usage, data contract and changelog fragment |
| `.tmp/result-epitrop-f4-ranked-frontier-r1.md` | Untracked judge handoff |

## Expected case results

These are expectations from the source. Neither revision was executed in this lane. The new tests use existing types or public wire fields so the judge can run them against the baseline.

| Fixture / test | Expected on 99f201d | Expected with the change |
| --- | --- | --- |
| `TestRankedFrontierInsertionOrder`, both insertion orders | Text lists eligible nodes as a,b,z. JSON flag rejected. | Text and JSON order z,a,b. Progress 2,0,0. The leaf terms tie. Neither x nor y appears. Repeated reads keep sequence 1. |
| `TestRankedFrontierEmptyAndSingleton` | Empty and singleton text listing available. JSON and next flags rejected. | Empty array at sequence 0. Next refuses without appending. Singleton s at 1, one claim, empty array at 2. |
| `TestRankedAssignmentReplay/next` | Unknown next flag. | One assignment of r:z at 2 from prefix 1. Mode next, actor-1, lane-1 and the winner's four terms. |
| `TestRankedAssignmentReplay/named` | Named b succeeds, but show lacks original assignment evidence and resume omits node and actor. | One assignment of r:b from the same input on an independent log. Mode named and b's terms. |
| Reopen and release in both replay subtests | Resume lists an open unit. Rank evidence and ranked frontier JSON unavailable. | Each fresh show/resume sees exactly the selected unit and decision. Reading them appends nothing. A later actor/lane update preserves the original decision. Release returns the node with cost 1 at sequence 4. |
| `TestRankedAssignRefused`, missing, empty, extra or mixed selection | Missing node refused. Extra positional nodes can be ignored. Next flag rejected. | Exactly one named node or next required. Each invalid combination leaves the source sequence alone. |
| Same test, blocked, missing and open-unit cases | Named x is already refused for its dependency. Named missing is refused. Automatic selection unavailable. | Refuse x with its dependency on z, refuse a missing node, refuse next when z's unit is already open. No alternate claim or extra event. |
| `TestRankedSameHeadRace` | Checked named assignment already excludes a second claim. Ranked JSON and the ranked writer are unavailable. | Two readers selecting z from sequence 1 produce one success, one exit 75 and one assignment. Reopened state agrees with that event. |
| `TestRankedUnrelatedAppend` | No saved rank position is checked. | A non-graph event moves the head while s stays ready. The saved claim returns 75 without writing. A fresh selection succeeds from sequence 2. |
| `TestLegacyAssignmentReplay` | Show has node/actor/lane at top level and resume lacks historical assignment columns. | A signed legacy event reopens with its recorded identity and no invented rank. The event remains the only claim. |
| `TestRankedCompactedBase` | No CLI JSON envelope or base position. | A signed base at HEAD returns a at position 1, confidence false, elapsed and cost zero. With a new ready z at 2, equally scored z leads on confidence. A named claim of inherited a records its low confidence. |
| `TestGraphReplayDiscardsFailedAttemptFacts` | Graph service lacks the position-returning reducer factory. | A real append during reduction forces two attempts. Sequence 4 and cost 1 identify the successful fold. Reusing observations would count the claim twice. |
| `TestRankedAssignmentRejectsMalformedEvidence` | Unknown selection fields are ignored by the old decoders. | Both graph and unit folds refuse invalid mode, missing version, zero or mismatched position, and negative cost, elapsed time or progress. |

## Judge checks

Test command and exit code: **not run in this lane; no exit code**. The brief assigns these checks to the dispatching judge.

- `go test -race -timeout 45m ./cmd/epitropctl/... ./internal/score/... ./internal/graphlog/... ./internal/graph/... ./internal/units/... ./internal/eventlog/...`
- New tests on `99f201d` for red evidence
- `golangci-lint run`
- `gosec ./...`
- `govulncheck ./...`
- Repository documentation and changelog checks
- Push gate

Tools used: local Bash, Git for read-only status/diff/history, rg, file readers, and Python for source edits. The flag API was checked in the installed Go 1.27.4 source. The handoff and implementation were written in this worktree.

**Open questions:** None for this round. Runtime, race, lint and gate verdicts await the judge. The baseline frontier service remains available for the separate parity and retirement review.
