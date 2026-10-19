# Architecture

Status: design draft. Only the catalog exists in code today.

## Overview

```text
[ Svelte 5 UI ] <--(SSE / REST / JSON-RPC)--> [ Go control plane ]
                                                  - task graph and state
                                                  - event bus
                                                  - budgets, telemetry
                                                        |
                                                  (FFI / IPC)
                                                        v
                                                [ Rust vector kernel ]
                                                  - skill-vector matching
```

## Components

### Go control plane

Owns task graphs, the event bus and budgets. Runtime traces and metrics are documented in [telemetry](telemetry.md).

`internal/graph` is the in-memory task graph store. It stages node, edge and
status edits, validates the staged result with Kahn acyclicity, configured depth
and vocabulary guardrails, then swaps the state only after every guard passes.
The event log rebuilds the graph: `internal/graphlog` reduces one
`graph.stage_committed` record per committed stage, whose payload holds the
stage's operations in order, through the pure `ApplyStage(events)` of this
package. A stage the graph refuses, which two racing writers can produce, is
not applied and is counted in the replayed state; a record that is no stage
stops the replay. Replay checks the hard depth cap of 64; the configured depth
binds new writes: `epitropctl graph apply` checks a stage against the graph
the log holds, under the configured depth, and appends it as one record, and
`epitropctl graph show` prints the graph rebuilt from the log. The check runs
under the log's lock (`eventlog.Writer.AppendChecked`), as the run record's and
an effect resolution's do, so a writer that another writer got ahead of is
refused instead of told its write succeeded. Removing a node with any incident
dependency edge is refused; callers remove edges first so history stays
explicit.

Task scoring (`internal/score`, #10) orders the frontier: ready units whose
dependencies have all landed. Every term comes from event-log records; the
reference time is the time of the newest record read, never the clock.
`ElapsedSeconds` is the reference time minus the time of the record that last
moved the node into ready, in whole seconds rounded down, and zero when that is
negative (clock skew between writers) or unknown. `Progress` counts the nodes,
neither landed nor dropped, that depend on the node directly or transitively.
`Cost` counts the records that moved the node into claimed, implementing, review
or a fix round: work already spent on it and handed back. `Confident` is false
for a node that already existed in the state the first folded record was applied
to (a checkpoint or compaction base), whose earlier history is unknown. The
order is Progress descending, Cost ascending, ElapsedSeconds descending,
confident nodes first, then node ID ascending; the ID makes the order total, so
the same log gives the same order.

The task-graph node vocabulary follows Praetor #883 as amended on 2026-10-09
and the claim-code source `feat/issue-claims` at `42f2a486b`
(`internal/forge/claim_marker.go`). Kinds are `epic`, `unit`, `decision`,
`research` and `gate`; statuses are `proposed`, `ready`, `claimed`,
`implementing`, `review`, `fix-round-N`, `blocked`, `queued`, `landing`,
`landed` and `dropped`.

- Task graphs are acyclic. A cycle is rejected before the graph changes.
- Traversal is bounded. The proposed depth limit is 12; it is a design parameter, not a standard.
- State changes are staged and roll back when a trajectory breaks a guardrail.

Control-plane process configuration is loaded by `internal/config.Load` from a YAML or JSON file validated against the published schema at `docs/config.schema.json`; the embedded copy at `internal/config/config.schema.json` is the package authority and a test keeps the two byte-identical. The loader uses `golusoris/core/config` with file watching off and environment overrides disabled, because schema validation runs before weak typed decoding and string-valued environment overrides would otherwise bypass or break numeric validation. A bad key, type or bound fails load with the JSON pointer named before unmarshal.

#### Event log

The event log is the source of truth ADR-001 names, and RT-04 rests on it: the task graph and every task state are rebuilt from it alone. It is a directory of plain files in a Git working tree, written only by the control plane (RT-03).

Declared background jobs use the same log as their supervisor ledger. Each job is loaded from config as an argv-only command with exactly one log path, schedule, restart policy, stop policy, optional `max_runtime_seconds` and `sandbox.mode`. The schedule allow-list accepts `always` (a daemon), `once` (manual or slot run), `event` (triggered by matching event log records, requiring `on_event`), or a standard 5-field cron expression evaluated in UTC using only digits and `* / , -` characters. Any other schedule string, an unreachable calendar date, an `on_event` block without an `event` schedule, an `event` schedule missing `on_event`, an event trigger matching the job's own records, or any restart policy other than `never` on a cron or event job is refused at configuration load and supervisor check. `restart.max_restarts` bounds the restarts recorded in the last `restart.window_seconds` (the record's own time against the supervisor clock); with 0, the default, it bounds every restart in the log. A restart of the supervisor's own unit ends the jobs it runs, by design: each shim runs in the supervisor's cgroup and its child gets a parent-death signal, which is the no-orphan guarantee of [ADR-0005](adr/0005-sandboxed-job-runner.md), so the job is recorded lost and restarted, and that restart counts against the cap. Reconcile anchors triggered jobs with a `job.armed` record naming the canonical trigger and its UTC time: schedule due times count only after that time and event matches only after that record's own sequence number, so new jobs never fire for historical due times or events, and changing a job's trigger re-arms it. When a cron due time or an event match occurs, the supervisor appends `job.triggered` before starting the process group, enforcing at-most-once semantics where a startup failure consumes the trigger rather than retrying indefinitely. If a cron job is already running at its due time, `job.triggered` records `outcome: skipped` with `reason: running` and consumes the tick without an overlapping run; missed due times while the supervisor was down produce a single start for the newest due time. Event triggers match incoming records in ascending sequence order; if the job is already running when a matching event arrives, the match queues without advancing the cursor and starts once the previous run exits. On Linux, `epitropctl jobs start|status|stop|supervise` starts jobs through the hidden shim path, which holds `<event_log.dir>/jobs/<name>.lock` with `flock` (through `github.com/gofrs/flock`) on a close-on-exec file descriptor that the job does not inherit (`TestJobLockIsNotInheritedByAStartedProcess`), records the shim and child pids, appends stdout and stderr to the declared log, starts the child in its own process group, and keeps the shim as the waiting parent. A job that is still running after `max_runtime_seconds` is stopped by its shim the way `jobs stop` stops it, with the job's stop signal, its grace period and a kill of the whole process group, and the exit is recorded with reason `deadline`. The supervisor that started a shim collects its exit status, so a long-running `jobs supervise` leaves no zombie per job start (`TestSupervisorCollectsItsShims`); the shim itself still outlives a supervisor that exits first. `jobs supervise` runs until SIGTERM or an interrupt, which end it with exit 0; only loading the configuration and the other jobs commands carry a 30 second bound (`TestJobsActionContextLetsSuperviseOutliveTheCommandTimeout`). An opt-in soak test, `TestSoakSupervisorStartStop` (`EPITROP_SOAK=1`, Linux), starts and stops a job 200 times and then checks that no child or zombie is left, that the open file descriptors did not grow and that the event log grew by at most four records a cycle. The default `sandbox.mode: enforce` wraps the job in the RT-03 runner; `sandbox.mode: off` runs the bare command only when the job carries a non-empty reason, and the `job.started` payload and status row name that choice. Before stop or escalation sends any process-group signal from a persisted record, the supervisor refuses the record unless the child pid is greater than 1, is not the supervisor pid or process group, is the process-group leader, has the recorded shim as parent, and the live shim can be proven to be the hidden `__job-shim` for that job and record while the job lock is held. The child receives a kernel parent-death signal if the shim dies, so the child does not outlive its shim and a restarted supervisor cannot create a second live copy. The kernel releases the lock of an exiting shim before it gives the child another parent, so for a moment the child, still a child of the shim, outlives the lock; a stop whose signal is refused in that moment, with the lock free, counts the job as stopped and signals nothing, while a refusal with the lock held stays an error (`TestStopOfAJobWhoseShimExitsDuringTheSignalIsDone`, `TestStopRefusalStandsWhileTheLockIsHeld`). A new supervisor replays job events, adopts a still-locked record without starting a second copy, records a lost exit when the lock is free, and applies bounded restart decisions from replayed state. Liveness is the held lock alone: a live process whose command line is the job's shim command line and whose pids the record names, but which holds no lock, is reported dead, is never signalled, and is replaced by a real start rather than adopted (`TestLookalikeShimIsNeverTheJob`). A job may declare `heartbeat.max_age_seconds` (0, the default, is off; at most 86400). Its last beat is the newer of its start and the last write to its log, so a sandboxed job needs no extra writable path; `jobs status` adds `stale since <last beat>` to a running job whose beat is older than that. Each reconcile appends `job.stale` (pids, last beat, bound) for such a job and ends it the way `jobs stop` does, without a `job.stopped` record, so the shim records the exit and the restart policy starts the next copy (`TestHungJobIsEndedAndRestartedOnAStaleHeartbeat`, `TestLogWriteIsAHeartbeat`). `epitropctl jobs history --config=file [--since=RFC3339] [name]` counts each job's `job.started` and `job.adopted`, `job.exited` (and of those the exits without code 0), `job.restarted`, `job.stale`, `job.refused` and `job.stopped` records written at or after `--since`, with the first and last counted time; a replay that the log's retry runs again counts each seq once (`TestHistoryCountsARetriedRecordOnce`), and a compacted log counts only after its base. Other platforms report `not-supported` for this orphan-prevention guarantee until file locks, process groups, signal delivery and parent-death signalling are implemented there.

`epitrop-server` classifies failed job exits through the dead-letter classes of `broker.dead_letter` (`internal/jobfailure`). A `job.exited` record is a failed exit when its exit code is not 0, when it has no exit code (a lost job), or when it names a reason such as `deadline`; code 0 without a reason is a clean exit. The class is chosen from the text `exit code <c>`, the reason when set and the last 16 KiB of the job's declared log, matched against the class signatures, with the fallback class when none matches. The attempt is the number of consecutive failed exits of that job, counting this one; a clean exit or a `job.stopped` record resets it. The verdict is appended as one `job.dead_lettered` record with `exited_seq`, `class`, `retry`, `parked`, `attempt`, and `code` and `reason` when the exit had them; `parked` is false when the class queue was full. The check that no record for that `exited_seq` exists runs under the event log lock, so several servers on one log write one record per failed exit (`TestAnotherWriterWins`). A server starts after the newest `exited_seq` already classified, or at the log head when there is none, so exits from before the first deployment are not classified (`TestTheFirstPollStartsAtTheLastVerdict`).

- **One file per record.** Record `n` is `events/<n, 20 digits>.json`, so file order is log order and a Git diff shows each append as one new file. A record is written to a temporary file in the same directory, fsynced, then renamed into place, so a crash leaves either the whole record or none of it. A record is at most 64 KiB, and one replay reads at most the configured number of records.
- **Canonical bytes.** A record is JSON canonicalised by RFC 8785 (`core/codec/jcs`) with the fields `seq` (1, 2, 3, …), `prev` (the SHA-256 of the previous record's bytes; zeros for the first), `time`, `type`, `task_id`, `payload` and `receipt`. Replay re-canonicalises each file and refuses one whose bytes differ.
- **Signed.** `receipt` is an Ed25519 receipt (`core/crypto/receipt`) whose output hash is the SHA-256 of the record without its `receipt` field. Replay checks it with `receipt.VerifyOutput` against the public key in config, so an edited record fails even when its JSON stays valid.
- **Single writer.** An append takes an exclusive lock on `events/` (a lock file held with `flock` on Unix and `LockFileEx` on Windows, through `github.com/gofrs/flock`) for a bounded time. It allocates the next `seq`, writes the record and replaces `HEAD.json` while holding it, and fails closed when the lock is not granted in time. Two concurrent appenders can neither reuse a `seq` nor lose a record.
- **Lock timeout.** A writer waits for the lock at most `event_log.lock_timeout_seconds` (default 5, from 1 to 300). The supervisor, the job shim and `epitropctl units` all use that value. One append costs four syncs, two for the record and two for `HEAD.json`, and writers queue: the last of *n* waiting writers waits for *n* − 1 appends. `go test -run '^$' -bench BenchmarkQueuedAppenders ./internal/eventlog/` measures the queue: sixteen writers that start at once finish in about 0.15 s, about 9.4 ms an append, on tmpfs and on an NVMe disk alike. On a fast disk the cost is the lock's retry interval, not the syncs: a waiting writer tries the lock again every 10 ms (`internal/eventlog/lock.go`). The default therefore leaves room for several hundred queued writers. On a disk under heavy write load appends have been seen more than forty times slower (#83); at that rate about a dozen queued writers reach the default. The workflow `eventlog-load.yml` runs sixteen queued appenders ten times with the temporary directory on btrfs under CPU and write load (#146). In the first runs there (Actions runs 38908545658 and 38910086612) sixteen queued appends took up to 15 s, about 0.9 s an append, and the queue outlasted the 5 s default in most runs; the workflow therefore waits with a timeout that only ends a failing run, and its summary records what an append cost. A writer that times out appends nothing and reports the lock timeout, and no caller retries. A stable replay (`ReplayStable`, `ReplayCheckpointedStable` in `internal/eventlog/replay.go`) that lands inside an append waits for it as long, since the append holds the lock meanwhile; on Windows it also retries the sharing violation a reader gets while an append renames a file (#218). Raise the value on a slow disk.
- **Signing key.** The private key path comes from the config (#31). The writer refuses a key file inside the repository tree and one readable by group or others, as ssh does. On Windows Go reports every writable file as mode 0666, so the mode check does not run there: the key is only as private as the ACL of the folder that holds it, so keep it in a folder only your account can read. Replay needs only the public key; with the wrong one it fails at the first record and names it.
- **Head.** `events/HEAD.json` names the last `seq` and its hash, signed the same way and replaced atomically after each append. A deleted tail, which the hash chain alone cannot see, then fails replay. An uncommitted record past HEAD left by an append that failed before advancing HEAD is removed under the writer lock by `Writer.RepairTail` after verifying its prev link and signature, clearing half-written state without wedging the log.
- **Checkpoints.** Stored at `<event_log.dir>/checkpoints/<name>/<seq, 20 digits>.json` (JCS-canonical, signed with the log signer, at most 3 kept per name). Each checkpoint is bound to its sequence number and the content hash of the record at that sequence. Replay scans newest first and starts from the newest valid checkpoint whose signature, canonical format, and record hash match the log on disk, skipping records before that sequence and checking the tail and HEAD afterwards. If a checkpoint's record is missing or has a different hash, the log copy has diverged: replay refuses that checkpoint, the units service records `log.diverged` with both heads under lock, and renames the checkpoint file to `<file>.refused`. Log copies are never merged automatically.
- **Replay.** Records are read in order. Each is checked for contiguous `seq`, the `prev` chain, canonical form, receipt and size, then applied to a pure reducer. Any failure stops replay with the record's position (`seq` and file), never skipping it. The rebuilt state serialises through JCS, so two replays of the same log produce the same bytes, which the tests compare.

- **Segments.** Closed ranges of 256 records move into `events/segments/<first>-<last>.jsonl` ([ADR-0010](adr/0010-event-log-segments.md)): one record per line, each line the exact bytes of the record file it replaces, so receipts and the `prev` chain are unchanged. `Writer.Rotate` (`internal/eventlog/rotate.go`) moves at most 4 ranges per call under the log's lock, never the record `HEAD.json` names; the units service calls it after each checkpoint. Replay reads the segments, then the files; a record left in both places by an interrupted rotation replays when both copies are identical and fails with the seq, file and segment when they differ. A v0.1.0 log replays to the same state before and after rotation (`TestV010LogReplaysTheSameAfterRotation`); a v0.1.0 binary does not read segments.

- **Git commits.** With `event_log.git.enabled`, the units service commits `events/`, `events/segments/` and `checkpoints/` to `event_log.git.ref` (default `refs/epitrop/events`) after each checkpoint ([ADR-0011](adr/0011-event-log-git-commits.md)). `Writer.RecordInGit` (`internal/eventlog/gitrecord.go`) reads the files under the log lock byte for byte (`git hash-object --no-filters`), builds the commit with a private index and `git commit-tree` under the configured `author_name` and `author_email`, and moves the ref only when it still names the commit it built on. The operator's index, working tree and branch stay untouched and no commit hook runs; a log whose files did not change makes no commit. All Git commands of one commit share `event_log.git.timeout_seconds` (default 30). A failed commit leaves the log and the ref as they were and is tried again at the next checkpoint.

- **Compaction.** With `event_log.git.compact`, the units service calls `Writer.Compact` (`internal/eventlog/compact.go`) after each commit ([ADR-0012](adr/0012-event-log-compaction.md)). It writes `checkpoints/base/<seq>.json` (the name `base` is reserved for it), the state of every registered fold (`units`, `graph`, `effects`, `jobs`) after one record, signed under `epitrop event-log base`, and then removes the segments, record files and older bases at or below it. The base only covers segments the commit on `event_log.git.ref` holds byte for byte, never the record `HEAD.json` names, and never a record the supervisor's armed event triggers have not passed. Replays of a compacted log start at the fold's state in the base; a replay without a fold name, of a fold the base lacks, or over a base that fails its check fails with the base's seq or file. A v0.1.0 log replays to the same state before and after compaction (`TestV010LogReplaysTheSameAfterCompaction`); a v0.1.0 binary does not read a compacted log.

The run record keeps one entry per work unit in the same signed log (`internal/units`, `epitropctl units`), so that after a crash the open units are read back, not reconstructed from transcripts. `units set <id> --status=…` appends `unit.recorded` at each status change. The run record has no status rules of its own: the names and the allowed moves are the task graph's (`internal/graph`: `proposed`, `ready`, `claimed`, `implementing`, `review`, `fix-round-N`, `blocked`, `queued`, `landing`, `landed`, `dropped`). A record that keeps the status only updates fields; worktree, branch, pull request and lane keep their value until given again; a landed or dropped unit moves again only with `--reopen`. `--evidence="<path> sha256:<hex> lines:<n>"` records where the proof of a unit's result lies: the record keeps that pointer, never the file, and keeps it until a new one replaces it. `units resume` lists the open units with a relaunch verdict: `safe` only for a ready, claimed or implementing unit without a pull request, because anything later may already be under review or landed. `units preempt <id> --reason=… [--by=…]` appends `unit.preempted`, moving the unit to `blocked` and recording why. A note sent with `units note` is not pushed into a running agent: the next `units set` prints the unread notes and `units inbox` marks them read. `units handover --repo=<repository> [--answers=<file>] [--resume-out=<file>]` is the hand-over of one repository to the next session (`internal/slot/handover.go`). The repository is the part of a unit id before its `#` or `:`. For each open unit it prints the record (status, worktree, branch, pull request, lane, relaunch verdict, evidence pointer), the outcome of the last run when the pointer names that run's `run.json`, the pending decisions and the number of unread notes. The pending decisions are the ids of the run's `DECISION` and `ASK` lines that the answers file does not hold; a question line without an id stays pending as written. A report whose digest no longer starts with the recorded one leaves them unknown, never empty. The answers file holds one answer per line: the id, then optionally a tab and the answer's text; `#` lines and blank lines are skipped, and any other line or an id given twice refuses the whole file. The command only reads the log: unread notes stay unread. `--resume-out` writes the same as a slot's resume note, every field quoted, and refuses a note above the 64 KiB input bound instead of cutting it. A unit may carry Praetor's run identity under Praetor's own field names (`lane`, `target`, `resolved_model`, `branch`, `identity`, `identity_key`); the identity object is stored as given and the key is never recomputed here, so both records join without a mapping. Reducers share the log by domain prefix (`task.`, `job.`, `unit.`): each skips the other known domains and fails on an unknown one. The unit reducer also never fails on a rule that two writers can break by racing, such as moving a unit another writer just landed, because one such record in an append-only log would stop every later replay; it leaves the record unapplied and counts it, and `units resume` reports the count. The check runs under the log's lock (`internal/units/service.go`), so a writer that loses such a race is refused; the reducer rule and its count stay for logs written before that and as a second defence. The task graph and the run record meet in two record types that both reducers read (`internal/units/assign.go`, `internal/graphlog/assign_reducer.go`). `units frontier` lists the nodes a slot may take: kind `unit`, status `ready`, every direct dependency `landed`; a dropped dependency blocks. `units assign --repo=<repository> --lane=<lane> <node>` appends one `unit.assigned` record: the graph moves the node from `ready` to `claimed`, and the run record opens the unit `<repository>:<node>` as `claimed`, not `queued`, because `queued` may not move to `implementing` and a slot run starts there. `units release --reason=… <node>` appends `unit.released`: the node goes back to `ready` and the unit is dropped with the reason. The writer checks both sides under the log's lock, so two slots racing for one node leave one assignment. Each reducer applies its own side; a move its side refuses is left unapplied and counted, and a malformed record fails both.

#### Agent slot

A slot is one run of a headless agent harness on one work unit, started as a supervised sandboxed job (RT-03). `epitropctl slot run` runs one unit: it makes the run's workspace, starts the job, waits for its end, takes what the run left and records the unit (`internal/slot`).

```bash
epitropctl slot run --config=slot.yaml --base=main --brief=brief.md \
  --workspace=/var/tmp/slots/unit-5 --out=/var/tmp/slots/unit-5.out 'epitrop#5'
```

The configuration is the slot's template: a configuration with exactly one job, sandboxed (`sandbox.mode: enforce`), reaching its declared gateway or nothing (`sandbox.network: proxy` or `none`; `egress` is refused, [ADR-0006](adr/0006-proxy-socket-egress.md)), and with a deadline (`max_runtime_seconds` from 1 to 86400). `slot resume` checks the saved `config.json` the same way (`TestCheckRunJobLetsAJobReachItsGatewayOrNothing`). The job's command is the harness. The runner sets what only a run can know, the job's `log_path`, `schedule`, `restart` and `sandbox.workspace`, and refuses a template that sets one of them. It writes the result as `config.json` into the output directory and loads that file like any configuration, so the loader's checks, the protected paths among them, hold for the run's own workspace (`TestRunConfigKeepsTheEventLogOutOfTheWorkspace`). `epitropctl jobs status|stop --config=<out>/config.json` reaches the run's job.

A run goes through these steps (`slot.Run`):

1. The workspace is prepared and the output directory made. Neither may exist, and neither lies inside the other.
2. The host is checked for the sandbox's tools, and the slot's job must not run already: one slot runs one unit at a time.
3. The unit is recorded as `implementing`, with the workspace and the branch. The record comes before the start, so a unit that may not move to `implementing` starts no job. Up to here a failure leaves nothing: no workspace, no output directory, no record.
4. The job is started and watched until it no longer runs. The supervisor's shim ends it at its deadline; the runner waits one minute beyond the deadline and the stop grace.
5. The result is checked, the branch fetched and checked, the result and the evidence copied out, and the branch brought into the repository.
6. The runner writes its report, `run.json`, into the output directory and records the unit with an evidence pointer to it: `review` for a run that reports `done`, `blocked` for every other end.

- **Workspace.** `slot.Prepare` makes a fresh local clone of the repository for one unit. The clone is checked out on the branch `unit/<unit id>` at the named base commit, also when the repository itself is on a branch of that name; a `:` in the id is written `=`, because a branch name takes no colon. It has no remote and shares no object file with the repository (`git clone --no-hardlinks`). Git runs there without the caller's own `GIT_` variables and without global or system configuration: git exports `GIT_DIR` to every hook, and a runner started from one would otherwise act on the hook's repository (`TestPrepareIgnoresTheCallersGitEnvironment`). The inputs lie in `.slot/in/`: `brief.md`, `resume.md` and `decisions.tsv`, one answered decision per line as id, tab, answer. `.slot/` is excluded in the clone, so no commit of the run carries it. A request with a relative path, an existing workspace directory, a base that is no commit, an empty brief or an input above its bound (64 KiB for the brief and for the resume note, 256 decisions) is refused, and a workspace that could not be finished is removed again.
- **Result.** The run reports in `.slot/out/result.json`, validated against `internal/slot/result.schema.json`: the unit, an outcome of `done`, `blocked` or `failed`, a summary, the head commit of the unit's branch, evidence pointers and open questions. `ReadResult` refuses a document that breaks the schema, names another unit or claims `done` without a head. It refuses evidence that is listed twice, is no regular file under `.slot/out/evidence/`, leaves that directory through a symlink, or differs from its recorded SHA-256 or line count. The result is at most 64 KiB, one evidence file at most 8 MiB.
- **Branch.** The runner runs no git command inside the workspace after the run: its git directory is then the run's own writing, settings and hooks included. It fetches the unit's branch from the workspace, which serves it through `git-upload-pack`, into a ref of its own (`refs/epitrop/slot/…`), with the arriving objects checked (`fetch.fsckObjects`). The head has to be a commit, the one the result names, and has to descend from the base. Only then is the branch brought into the repository's branches, without force: a branch of that name that holds other commits, or that is checked out, stays as it is, and the run is not taken. A run that made no commit publishes nothing. `TestRunRunsNothingTheWorkspaceNames` plants settings that name a command and hooks in a workspace and shows that none runs.
- **Report.** `run.json` holds the outcome, the reason of a refusal, how the job ended (exit code, `deadline`), the base, the head, and the result and evidence copies with their digests. Three outcomes are the runner's own: `refused`, when the supervisor did not start the job; `not-taken`, when the run ended and the runner did not take its result: there was none, it was not valid, or the repository did not take the branch; and `preempted`, when the unit was preempted (see below). The command prints the unit, the branch and the report path, and fails for these three outcomes.
- **Detached run.** `epitropctl slot run --detach` checks the arguments and the template, starts the same `slot run` as a transient systemd user service, prints the service name and the output directory, and returns. The service belongs to the user manager, not to the caller's session or editor, so the run goes on after the caller ends (`TestSlotRunDetachedOutlivesItsCaller`). Its name follows the output directory, `epitrop-slot-<12 hex of the directory's SHA-256>.service`, so one directory always gives one name. The template must set `sandbox.memory_max` and `sandbox.memory_high` itself: the service takes them as `MemoryMax` and `MemoryHigh`, and the default the loader fills in for a missing `memory_max` is no limit anyone chose for the slot. A template without them, or with one outside the loader's bounds (64M up to 64G, the soft limit up to the hard one), starts nothing (`TestSlotRunDetachRefusesATemplateWithoutItsOwnLimits`, `TestSlotRunDetachTakesLimitsFromTheFloor`). The job runs in its own scope with the same limits, so a slot's worst case is twice `memory_max`. systemd-run gets `--expand-environment=no`, the caller's `PATH` and absolute paths only, since the service does not start in the caller's directory. It also gets each name in the job's `sandbox.env_allow` that the caller has set, as `--setenv=NAME` without a value, so systemd-run takes the value from its own environment: the job sees the same values as in an attached run, and no value stands in the command line (`TestSlotRunDetachForwardsTheAllowedEnvironmentByName`). A name the caller has not set stays unset. The service ends with the runner and is collected; what became of the run stands in the output directory, and `slot resume --out=<dir>` takes up a run whose runner stopped.
- **Client home.** A template's job may declare `sandbox.home` ([ADR-0009](adr/0009-slot-client-home.md)): a source folder in a declared root that is none of the operator's login locations, the variable that points the client at the home, the files to copy by SHA-256 and the files to bind read-only. Before each start the shim makes a fresh `.slot/home` in the workspace, copies each copy file only when it hashes as configured, and binds each bind file read-only on its own, so the client writes its sessions and caches but cannot change its login (`TestSandboxedJobReadsItsHomeAndCannotWriteTheLogin`). A bind file is one plain name or a login file measured in its client (Claude Code's `.credentials.json`); a login below a folder gets its folders made and pinned, and a home may take `HOME` (`TestSandboxedJobReadsItsLoginsBelowHOME`). Every bind names its token's expiry, as a JSON Web Token or as Unix milliseconds; the start is refused unless the expiry lies 10 minutes beyond the job's deadline and stop grace, because a client that refreshes inside the slot cannot save the rotated token and leaves the login outside spent. A home needs `max_runtime_seconds`. The branch step refuses a head whose commits, merge commits against each parent, touch `.slot/` or add a file with a bound file's content (`TestRunTakesNoBranchThatHandsBackTheLogin`), and `run.json` lists every bind with its source, target and mode.
- **Takeover.** Before the job starts, the runner writes `start.json` into the output directory and records the unit as `implementing` with an evidence pointer to it. When a runner is stopped while the job runs or leaves the unit `implementing`, `epitropctl slot resume --out=<dir>` takes the run up: it adopts the running or ended job without a second start, waits for its end, and takes the result as an uninterrupted run. A recorded exit older than the start file is not counted as this run's.
- **Preemption safe points.** The slot runner honours unit preemption (`unit.preempted`) at two safe points in code shared by `slot.Run` and `TakeUp`: (a) before the job start in `Run` (after the implementing record), where a preempted unit stops before launching the job, and (b) after `Wait` in both `Run` and `TakeUp`, where a preempted unit takes no result, publishes no branch, and copies no evidence. Both safe points record the outcome as `preempted` with the preemption reason, leave the unit `blocked` with an evidence pointer to `run.json`, and fail the CLI command like a not-taken run.

The result rule is stated by fixtures replayed in both directions (`internal/slot/testdata/result/`); `TestResultCheckerRefusesAFixtureThatClaimsTheWrongOutcome` shows that the replay fails on a fixture whose expectation is wrong. The whole run is proven with a stand-in harness, a shell script under the real sandbox (`cmd/epitropctl/slot_linux_test.go`): a run that commits, writes evidence and reports `done` is recorded as `review` with its branch published, and its attempts to write outside the workspace and to forge the runner's report fail; an unknown outcome, a missing result and a run ended by its deadline are each recorded as `blocked` and change nothing in the repository.

What this does not cover:

- One real harness has run in a slot: Codex CLI 0.145.0 on #157 (a one-line test fix, landed as #175), with `network: proxy`, `chatgpt.com:443` as the only destination and a client home whose login was bound read-only. Measured on that run:
  - The client needs only its own files, the workspace and the read-only inputs the template names (here a Go toolchain and a module cache, so the checks ran offline). It read nothing of the operator's own client folder.
  - It works through the proxy. It also asked for `ab.chatgpt.com:443` four times, was refused, and finished regardless. It never asked for the login service, so no token was refreshed.
  - Codex's own sandbox (`workspace-write`) runs inside the job's sandbox; no weaker mode was needed.
  - Codex keeps the workspace's top-level `.git` read-only in `workspace-write` (`codex-rs/protocol/src/permissions.rs` at `rust-v0.145.0`), so the run could not commit and its change was committed outside the slot. The slot's client settings now select a permission profile that extends `:workspace` with write access to `.git` alone. With it, a second run on #133 (landed as #177) committed inside the slot and the runner published its branch. The runner still runs no git inside the workspace and fetches the branch through `git-upload-pack`, so hooks and settings the run writes into `.git` do not act.
  - Codex's sandbox refuses sockets, so a test that listens on one fails inside the slot (`socket: operation not permitted`). Those checks run outside the slot.
  - Codex adds a trust entry for the workspace to its settings. It lands in the per-run copy of the home only; the source is never written.
  - With `HOME` in the workspace, Go writes its telemetry counters to `.config/go/` in the work tree. A client that stages everything would commit them.
  - A detached run (#139, unit 6: a planted off-by-one in `internal/config/home.go` that fails `TestValidateSandboxHomeRules`) first lost the job's `env_allow` values: the service got only `PATH`, so Go saw no `GOMODCACHE` and no `GOPROXY=off`, tried to download the modules, and the proxy refused it. The run ended `blocked` with its checks failed, as it should. With the names forwarded, the same brief on Codex CLI 0.145.0 fixed the defect in one commit, ran its checks offline, and was recorded as `review` with its branch published, within 79 seconds and with about 201,000 input tokens (176,000 of them cached) and 2,700 output tokens.
  - Claude Code 2.1.302 was refused before it started: its login expires 8 hours after it is written, and the shim refuses a bound login that expires before the run's deadline. The slot binds the login read-only, so the client cannot refresh it inside the slot; how a run gets a login that lasts is an open decision (`epitrop-claude-slot-token-lifetime`).
  - Antigravity CLI 1.4.2 has run in a slot on the same #139 unit 6 brief: it fixed the defect in one commit, ran its checks, and was recorded as `review` with its branch published, in about 2.5 minutes. Two fixes came first. The home now binds the login in its nested folder, `.gemini/antigravity-cli/antigravity-oauth-token`, with `env: HOME` and `expires_rfc3339: token.expiry` ([ADR-0009](adr/0009-slot-client-home.md), #296). The sandbox had no `/etc/hosts`, so the client could not listen on `localhost`; every sandbox now gets a fixed read-only hosts file that names only localhost (#300). The login lasts about one hour, so it is refreshed outside the slot right before the run, and the shim's expiry check passes only for a login that outlasts the run's deadline plus its margin. Through the proxy the client needs `cloudcode-pa.googleapis.com:443`, `daily-cloudcode-pa.googleapis.com:443`, `oauth2.googleapis.com:443`, `www.googleapis.com:443` (user info) and `lh3.googleusercontent.com:443` (the account picture: version 1.4.2 stops its start-up check when that fetch is refused). It also asked for `play.googleapis.com:443` and `antigravity-unleash.goog:443`, was refused, and finished regardless.
  - Other harnesses and other destinations are not measured.
- A job with `sandbox.network: egress` reaches the whole internet. `network: proxy` gives a job its declared destinations and nothing else ([ADR-0006](adr/0006-proxy-socket-egress.md)).
- A valid result is the run's own claim. The runner checks its form, the evidence files and the head of the branch. It does not check that the evidence shows what the summary says, and it reviews no commit.
- Nothing bounds the size of what a run commits and the runner fetches.
- The branch step finds a bound login only as a verbatim copy: an encoded or split copy passes, the files inside an input folder are not compared, and a run with network access can send the login elsewhere.
- A later run of the same slot started after this job ended would be read as this run's exit: one slot runs one unit at a time.
- The workspace and the output directory are kept after every run that started; removing them is the operator's.

#### Upstream schemas

Catalog parsers are checked against small pinned upstream schema fragments under `internal/sources/*/testdata/upstream/`, with the source revisions and full-document digests recorded in `internal/sources/upstream-schemas.json`. Run `make schemas-refresh` to regenerate those fragments from the pinned upstream documents, and `make schemas-check` to verify the committed bytes still match.

### Rust vector kernel

Matches tasks to agent skill embeddings with SIMD distance (cosine, Euclidean). Integer-indexed arena trees keep hierarchical task decomposition cache-friendly. A topological check (Kahn) verifies acyclicity in O(V + E).

### Svelte 5 UI

Operator view built with runes (`$state`, `$derived`, `$effect`) and snippets.

## Runtime invariants

| Id | Invariant |
| --- | --- |
| RT-01 | deterministic edges: temperature 0, schema-validated output |
| RT-02 | idempotency key `(task_id, action_type)` on every side effect |
| RT-03 | control-plane isolation |
| RT-04 | event-sourced state |

RT rules are separate from the HISS code standards. Source code in this repository also meets HISS-01, -02, -04 (function limit 60 lines), -07 and -10.

### RT-01: deterministic edges

`internal/edges.Check` is the check of RT-01 for one model call: the request carries a temperature of exactly 0, and the output validates against the JSON Schema its caller declared. A call without a temperature is refused, because it runs at the provider's default. A call without a schema, or with an empty one, is refused, because an output "validated" against a schema that accepts everything is not validated.

The rule is stated by fixtures replayed in both directions (`internal/edges/testdata/rt01/`): a temperature above 0 or missing, a schema missing or empty, and an output that is no JSON, lacks a field, breaks a bound or carries an extra field must be refused; a sound call must be allowed.

Coverage: the check is bound to no live edge. The runtime makes no model call of its own yet, so nothing in a running system passes through `Check` today, and RT-01 is not enforced anywhere. The fixtures prove the check, not a system. The first model edge has to route every call through it; that binding is tracked as its own issue.

### RT-02: the side-effect ledger

`internal/effects` records every outbound side effect in the event log under its key, the task id and the action type. `Ledger.Do(key, effect)` runs the effect at most once:

- A key that is done is not run again. The delivery is recorded as `effect.skipped`.
- A new key is claimed (`effect.claimed`), the effect runs, and its end is recorded (`effect.done` or `effect.failed`). Two writers that deliver the same new key both append a claim; the first claim in log order holds, and each writer replays after its own append to see whether it owns the claim. The other one does not run the effect.
- A claim without an end, which a crash between the claim and the end leaves behind, and a failed attempt are open. Whether the effect happened is not known then, so nothing runs it again by itself: `Do` returns an error naming the open claim, and `epitropctl effects list` shows it. It stays open until someone records what became of it with `epitropctl effects resolve`: `done` when the effect did happen, `retry` when it did not and may be attempted once more. An open claim does not age out.

The rule is proven by fixtures replayed in both directions (`internal/effects/testdata/rt02/`), in the format of the RT-03 fixtures: a second delivery, a delivery on an open claim and a delivery after a failed attempt must not run the effect; a first delivery and a delivery after a recorded `retry` must.

What it does not cover yet: a sandboxed job cannot write the event log (RT-03), so the release watch still guards its own side effect, one issue per release, with a marker on the forge and its own seen set. Its results pass through the ledger once the broker carries them to the control plane.

### RT-03: control-plane isolation

The control plane is the Go process that owns the task graph, the event log, the broker, budgets and the operator's credentials. Task execution is everything that runs a unit of work: the sandboxed runner ([#25](https://github.com/tekmora/epitrop/issues/25)), the agents it starts and the commands they run. RT-03 holds when task execution can affect the control plane only by sending messages it validates. A task never gets the control plane's process, credentials, writable paths or network reach:

| Boundary | The control plane never shares | A task gets instead |
| --- | --- | --- |
| Process | its process, process group or session; signals and `ptrace` access to it; its memory and open file descriptors | a separate process tree, started by the runner, that reports through the broker |
| Credentials | its tokens and keys (gateway admin, forge, receipt signing), its environment and its credential files | an environment built from an allowlist, plus a short-lived credential minted for that task and scoped to the router aliases and forge actions the task declares |
| Writable paths | the event log, the graph store, config, its working tree and `.git`, credential files and every path outside the task workspace | one writable workspace per task and read-only views of declared inputs; paths resolve inside them without symlink escape |
| Network reach | its own listeners (API, MCP, metrics) and loopback services it did not grant | egress to the destinations the task declares, such as the gateway behind a router alias |

A task's results reach the control plane as broker messages, and the control plane validates them before it appends to the event log. The task never writes control-plane state directly. A violation is refused, and the refusal is recorded as an event naming the boundary, so the run fails loudly instead of degrading.

#### RT-03 fixtures

RT-03 is proven by fixtures replayed in both directions, as HISS-20 asks of every enforcement claim. Each fixture is one case file:

| Field | Meaning |
| --- | --- |
| `id` | a stable name, such as `rt03-credentials-env-leak` |
| `boundary` | `process`, `credentials`, `paths` or `network` |
| `direction` | `violation` (the attempt must be refused) or `compliant` (the action must succeed) |
| `task` | the task spec under test: environment allowlist, mounts, declared egress, minted credential scope |
| `attempt` | the one action the task performs: read a variable or file, write a path, connect to a destination, or signal a process |
| `expect` | `refused` or `allowed`, and for `refused` the event the control plane must record (type `rt03.violation`, the boundary, the task id) |

Every boundary has at least one `violation` and one `compliant` case. The checker fails when a violation is allowed and when a compliant action is refused, so neither an open sandbox nor one that blocks everything passes. A fixture is trusted only after it is shown failing against a runner without the rule (rule 13).

The sandboxed runner is implemented for Linux jobs ([ADR-0005](adr/0005-sandboxed-job-runner.md)). In enforce mode the shim resolves the configured command outside the sandbox, then starts `systemd-run --user --scope --collect --expand-environment=no` with `MemoryMax`, `CPUWeight` and `TasksMax`, and `MemoryHigh` when the job sets a soft limit, under a scope unit it names itself, optionally starts `pasta` for `network: egress`, and runs `bwrap --die-with-parent --unshare-all` with only `/usr`, certificate stores, `/proc`, `/dev`, tmpfs scratch paths, the writable workspace and declared read-only inputs mounted. The job sees exactly `PATH`, `HOME` set to the workspace, `TMPDIR=/tmp`, `PWD` and its configured `env_allow` names; the values travel in the process environment, never on the argv. The job's arguments arrive as its config wrote them: `--expand-environment=no` stops `systemd-run` from replacing `${NAME}` and `$$` in them, and a `systemd-run` without that switch (before systemd 254) stops the start ([ADR-0007](adr/0007-job-arguments-unchanged.md)). `network: none` has no network namespace sharing; `network: egress` gives the namespace its own TEST-NET-1 address, disables host loopback mapping and automatic port forwarding, and binds the systemd-resolved upstream resolver file. bwrap and pasta start with SIGTERM ignored so that a graceful stop reaches the job, which gets the default TERM action back just before it starts. `network: proxy` ([ADR-0006](adr/0006-proxy-socket-egress.md)) has no network either: the shim runs an HTTP `CONNECT` proxy outside the sandbox, on a Unix socket bound read-only into it, and the proxy opens a tunnel only to a `host:port` the job's `destinations` list names. Inside the sandbox the Epitrop executable runs as a relay from `127.0.0.1:3128` to that socket, and the proxy variables name that address. Paths the job's `read_only` list names inside the workspace are bound read-only over it, and each directory between the workspace and such a path is bound onto itself so the job cannot rename it away; a listed path that is missing or resolves out of the workspace stops the load and the start. Workspace and inputs are checked against the event log and signing key at config load and again at job start. Each must also resolve inside a directory of the top-level `sandbox_input_roots`, and a login location below `HOME` (`.ssh`, a forge or cloud login, a coding client's folder) is refused unless a root names exactly that location ([ADR-0008](adr/0008-sandbox-input-roots.md)). Before the shim records `job.started` it confirms that the job's cgroup is its own scope unit; a job found outside it is killed and the start refused, with the reason in the job's log and in a `job.refused` event.

The current runner enforces RT-03 process isolation, writable-path isolation, environment credential filtering and network modes `none`, `egress` without host loopback and `proxy` with declared destinations. It does not yet implement minted short-lived credentials, the broker, or `rt03.violation` event emission. The fixtures in `internal/supervisor/testdata/rt03/`, replayed by `TestRT03Fixtures`, therefore assert only that violations are refused and compliant attempts are allowed; broker event recording lands with the later broker unit.

The release watch is the first job built for this runner (`epitropctl watch run`, `internal/releasewatch`). `epitropctl watch run` and `watch seed` accept `--config` (required), `--once` (run a single pass then exit), `--interval` (interval between poll passes), and test/mirror endpoint overrides `--github-base`, `--huggingface-base`, and `--ntfy-base` (validated as absolute `http(s)://` URLs). It polls the GitHub releases (tags when a repository has none) and Hugging Face organisations named in the `release_watch` config block, and files one issue per new release in the consuming repository of each route. Because a sandboxed job may not write the event log, its seen-set is a file in its own workspace, written atomically after each delivery; before filing, it searches the target repository for the marker `<!-- release-watch: <id> -->` (the newest 500 issues carrying the route's first label, read 10 to a page with each page capped at 4 MiB, so that long issue bodies fit; a page over that cap fails the delivery), so a lost seen-set does not re-file a release whose issue that search still reaches. The seen-set file carries `"version": 1`; a file of another version, or one without a version, fails the pass instead of being read and rewritten as version 1. A failed source or delivery is logged with its cause and fails the pass; an empty but valid answer does not. `GITHUB_TOKEN` is sent only to the configured GitHub API scheme and host (the `--github-base` value when one is given) and is dropped on any redirect away from it. Without `GITHUB_TOKEN`, a sink search answered with HTTP 401, 403 or 404 fails the delivery with the missing token as its cause. A `rate_limit_floor` at or above the limit GitHub reports (`X-RateLimit-Limit`) is logged once with both numbers; the pass keeps going, and its GitHub requests stay throttled. Upstream release notes go into a code block, so their mentions and references do not notify or cross-reference upstream projects. `watch.*` events in the signed log wait for the broker. An opt-in soak test, `TestSoakReleaseWatch` (`EPITROP_SOAK=1`), runs 300 passes against a stand-in GitHub API that publishes one release each pass, and checks that each release is filed once, that the seen-set file grows only by its entries and that the live heap stays flat; `.github/workflows/soak.yml` runs the soak tests weekly and on demand and writes the measured bounds to its summary.

## Models and accelerators

Every model call goes through a router alias. No concrete model name appears in code or config. Accelerators form one dynamic pool; an arbiter places models from measured free memory. Work is admitted only when the resolved model reports ready.

## Decisions

### ADR-001: graph and event log as source of truth

- Context: SQL-backed agent rosters add setup friction.
- Decision: keep an in-memory graph hydrated from Git files and an append-only event log.
- Consequence: fast start, no database setup, history in Git. Paperclip is not required.

### ADR-002: Go and Rust split

- Context: orchestration needs networking and API tooling; vector search needs memory control.
- Decision: Go control plane, Rust kernel.
- Consequence: fast service work, low-latency matching.

### ADR-003: Svelte 5 runes for the UI

- Decision: build the UI with runes and export reusable component bundles.
- Consequence: small bundles, usable on web and desktop.

### ADR-004: golusoris as the pinned base of the control plane

- Context: the control plane needs config, logging, telemetry, an HTTP, SSE and MCP server, health, idempotency and signed records. golusoris ships all of them as opt-in modules, pre-1.0.
- Decision: build on golusoris and golusoris/core, pinned to exact versions and moved by Renovate. Epitrop writes only the task-graph store, the Git-backed event log and the in-process broker, which golusoris does not ship without Postgres or NATS.
- Consequence: one implementation per capability across the ecosystem ([HISS-19](https://github.com/cordanaLLM/praetor/blob/main/docs/standards/hiss-spec.md#hiss-19-reuse-before-writing)). Breaking golusoris releases arrive as planned migration units (#30).

### ADR-005: Praetor governs, Epitrop runs

- Context: runtime work (job supervision, event watches, resume, sandboxing, quota collection) had been filed in Praetor next to governance work, and three copies of the model catalog existed.
- Decision:
  - Praetor owns gates, the planning graph and its vocabulary, the efficiency ledger, the invocation and agent-loop budget layers, the dispatch hook, issue claims and the plan notebook.
  - Epitrop owns the runtime, the orchestration and platform budget layers, and the model catalog.
  - The runtime issues moved here with their history (#21 to #25).
- Consequence: Praetor reads the Epitrop catalog snapshot (#29), and the interfaces between the two are versioned contracts (cordanaLLM/praetor#1045).

### ADR-006: cutover order

- Context: operator scripts run the watchers, the landing queue and the wave runner today, and a session restart can kill them without notice.
- Decision: replace them in three cutovers, smallest and most shared first:
  1. Watchers and supervision (#36).
  2. The landing queue (#38).
  3. The wave runner (#39).
- Consequence: each cutover runs beside the script it replaces until the events match. The first cutover already removes the restart losses.

## Code that waits for its command

`deadcode ./...` (golang.org/x/tools v0.52.0) lists functions no command reaches. `deadcode -test ./...` lists none, so tests reach all of them. Each waits for the issue named below.

| Package | What waits | Issue |
| :--- | :--- | :--- |
| `internal/admission` | gate, gateway prober | [#16](https://github.com/tekmora/epitrop/issues/16), [#14](https://github.com/tekmora/epitrop/issues/14) (0.5) |
| `internal/budget` | `Check` (platform budget refusal on the ledger's measured use) | [#16](https://github.com/tekmora/epitrop/issues/16) (0.5) |
| `internal/edges` | `Check` | [#114](https://github.com/tekmora/epitrop/issues/114) (0.5) |
| `internal/effects` | side-effect ledger `Do`, `Open` | [#38](https://github.com/tekmora/epitrop/issues/38) (0.4) |
| `internal/eventlog` | `TaskReducer` | [#39](https://github.com/tekmora/epitrop/issues/39) (0.4) |
| `internal/ledger` | `Read` (Praetor efficiency ledger reader for the budget layers) | [#6](https://github.com/tekmora/epitrop/issues/6) (0.2) |
| `internal/graph` | `StatusFromClaim` | [#38](https://github.com/tekmora/epitrop/issues/38) (0.4) |
| `internal/telemetry` | `EffectRun`, `AdmissionDecision`, `BudgetUsed` | [#38](https://github.com/tekmora/epitrop/issues/38), [#16](https://github.com/tekmora/epitrop/issues/16) |
| `internal/slot` | `ReadResult`, `ResultSchema` (reader and schema of a slot's result) | no issue yet |
| `internal/config` | `SchemaJSON` (embedded config schema, compared with `docs/config.schema.json` by tests) | no issue yet |
| `internal/releasewatch` | `Service.State`, `State.Count` (read-back for tests) | no issue yet |
| `internal/telemetry/telemetrytest`, `tools/lintexceptions` | test helpers, stay | - |

## Milestones

Milestones 0.1 to 0.6 and the interfaces with Praetor are in the [roadmap](roadmap.md).
