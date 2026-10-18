# ADR-0012: Compact the event log onto a signed base once Git holds its segments

- **Status**: Accepted
- **Date**: 2026-10-17, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

The event log only grows. Segments ([ADR-0010](0010-event-log-segments.md)) keep the number of files small, but every replay still reads every record since seq 1, and the per-replay record limit (`event_log.max_replay_records`) caps how long a log may become. Issue #58 asks for compaction: records that every reader has folded may leave the log directory, once something else keeps them. [ADR-0011](0011-event-log-git-commits.md) gives that something: a commit on the log's own ref.

Several reducers read the same log (units, the task graph, the effect ledger, the supervisor's jobs). A per-reducer checkpoint ([architecture, Event log](../architecture-v2.md)) speeds up one reader but still needs the records after it, and a reader without a checkpoint needs every record. Removing records is only safe when every reader can start without them.

## Decision

1. **One signed base holds the state of every fold.** `Writer.Compact` (`internal/eventlog/compact.go`) writes `checkpoints/base/<seq>.json`: the seq of the last record it covers, that record's hash, and the canonical state of each registered fold after that record, signed with the log's key under the command `epitrop event-log base`. A fold is a name and a reducer, at most 8 per base. The names are the ones the checkpoints already use (`units`, `graph`, `effects`, `jobs`), and each package declares its own as `FoldName`. The name `base` is reserved: no checkpoint or fold may take it. A replay reads the base with the highest seq and fails when that file fails its check or when the seq it holds differs from its file name; it never falls back to an older base. Any file in `checkpoints/base/` other than a base or a dot file fails the replay with its name.

2. **A segment leaves `events/` only when a commit holds it.** `Compact` moves the base to the last record of the longest run of segments, starting after the old base, that the commit on `event_log.git.ref` holds with the bytes on disk (`git ls-tree` against `git hash-object --no-filters`). It never covers the record `HEAD.json` names, because rotation never moves that record into a segment. A segment that changed after its commit, or that no commit holds yet, ends the run.

3. **A fold can hold the base back.** A fold may give a floor: the highest seq it can lose. The supervisor's jobs fold keeps, beside its state, the seq after which an event trigger looks for new records; its floor is the lowest such cursor of the configured, armed event jobs, so a base never covers a record a trigger has not seen.

4. **Replay starts at the base and fails closed.** With a base present, `ReplayFold` and `ReplayCheckpointed` start from the fold's state in the base, with the base's seq and hash as the chain's start, so the first record after the base must name that hash as `prev`. A replay that names no fold, or a fold the base lacks, fails with the base's seq. A base that is not canonical, not signed by the log's key, signed under another command, or without seq and hash fails with the file. A checkpoint at or below the base is skipped. A replay that races a compaction is retried as transient.

5. **The base is written before anything is removed.** `Compact` writes and syncs the new base, then removes the covered segments and loose record files, then the older bases, and syncs each directory. A crash in between leaves records or bases the newest base covers; replay and the next `Compact` pass over them and remove them.

6. **Compaction is opt-in.** With `event_log.git.compact` (default `false`, requires `event_log.git.enabled`), the units service calls `Compact` after each `RecordInGit` with the folds `epitropctl` registers, and only when that commit succeeded. Without the key nothing is removed. The base itself is committed with the checkpoints by the next `RecordInGit`.

## Consequences

- A compacted log replays in time bounded by the records since the base, and its length is bounded by the records since the last commit, not since seq 1.
- A compacted log cannot be replayed without a fold name. A reader that reads the log with a reducer the base does not hold fails until it is registered as a fold; it cannot silently start from an empty state.
- The removed records stay in the repository's object store on `refs/epitrop/events`; reading them back is a separate step.
- A log written by v0.1.0 replays to the same state before and after compaction. A v0.1.0 binary cannot read a compacted log.

## Alternatives considered

- **A base per fold.** Rejected: the records may only leave once every fold holds them, so separate bases would have to agree on one seq anyway, and a reader whose base is missing would have to fail at a different file.
- **Compacting into a summary record appended to the log.** Rejected: the summary would sit after the records it replaces, so the chain could not start at it, and every reducer would need a rule for it.
- **The base in `events/`.** Rejected: `events/` holds the records and their chain, and a base is a signed snapshot like a checkpoint; beside the checkpoints it is committed by the same rule and needs no new name in the records' directory.
- **Compacting without Git.** Rejected: nothing would keep the removed records, and the signed history the log exists for would be lost.

## Verification

`internal/eventlog/compact_test.go` (including `TestV010LogReplaysTheSameAfterCompaction`), `TestStatusChangeCompactsTheLogWhenAsked` in `internal/units/checkpoint_test.go`, `internal/supervisor/fold_test.go`, `TestRunUnitsSetCompactsTheLogWhenAsked` and `TestLogFoldsNameEveryReaderOfTheLog` in `cmd/epitropctl/units_test.go`, `TestStatusChangeWithoutCompactKeepsTheSegments`, and the `event_log.git.compact` cases in `internal/config/config_test.go`.

Each of these tests was seen failing on a planted defect before this record was accepted: every base check (canonical bytes, signing command, seq against file name, unexpected entry, reserved name), the commit and floor conditions that hold a segment, the removal of older bases, the start of the chain at the base hash, the wiring of `event_log.git.compact` with its default off, and the jobs fold's floor.
