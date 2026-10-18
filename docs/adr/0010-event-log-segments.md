# ADR-0010: Move closed ranges of the event log into segment files

- **Status**: Accepted
- **Date**: 2026-10-17, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

The event log ([architecture, Event log](../architecture-v2.md)) keeps one file per record in `events/`: `<seq, 20 digits>.json`, JCS-canonical, signed, chained to the record before it by `prev`, with a signed `HEAD.json` naming the last record. One file per record is simple to write atomically and to check, but the folder grows by one file per append without end. Replay lists the whole folder, and a folder of tens of thousands of small files is slow to list, to copy and to commit to Git, which #58 plans next.

Issue #58 asks for three steps: segment rotation, compaction, and Git commits at checkpoints. This record covers the first. It must not change what a record is: every record keeps its bytes, its receipt and its `prev` link, so a log written by v0.1.0 (`testdata/v0.1.0/eventlog`) still replays to the same state.

## Decision

1. **A segment holds a closed range of records, byte for byte.** `events/segments/<first>-<last>.jsonl` (both 20 digits) holds the records `first` to `last`, one per line, each line the exact bytes of the record file it replaces. A JCS-canonical record holds no raw line break, and the writer refuses to rotate one that does. A segment has no signature of its own: each line keeps its receipt and its `prev` link, and the file name's range is checked against the lines, so a segment cut short, extended or with a changed line fails replay with the seq and the file.

2. **Rotation runs at quiet points, under the log's lock, never on the append path.** `Writer.Rotate` (`internal/eventlog/rotate.go`) moves ranges of 256 records, at most 4 ranges per call, and only ranges that end before the record `HEAD.json` names. That record stays a file of its own, because the writer checks it against `HEAD.json` before every append. The units service calls `Rotate` after it writes a checkpoint at a status change (`internal/units/service.go`); a failed rotation leaves the log as it was and is tried again at the next checkpoint.

3. **A segment is written before its files are removed.** Rotation writes the segment to a temporary file, syncs it, renames it and syncs the folder, then removes the record files and syncs `events/`. A rotation stopped in between leaves a record in both places. Replay accepts that when both copies are identical and fails with the seq, the file and the segment when they differ. The next `Rotate` removes the identical copies first and refuses to go on past one that differs.

4. **Replay reads segments, then files.** Replay lists the record files first and the segments second. A rotation writes its segment before it removes files, so a file the first listing missed lies in a segment the second one finds; a file that the listing showed and that is gone when it is read was moved meanwhile, and a stable replay (`ReplayStable`, `ReplayCheckpointedStable`) tries again. Segments must follow each other from seq 1 without a gap. A checkpoint whose record lies in a segment is checked against the segment's line. Names that start with a dot are the temporary files of an interrupted write and are passed over; any other file in `segments/` fails replay.

## Consequences

- `events/` keeps at most 256 × 4 loose records after each rotation plus the records since, and one file per 256 records otherwise.
- The log format gains a folder. A v0.1.0 binary does not read segments: a log rotated by this version must not be replayed by v0.1.0. A log written by v0.1.0 replays unchanged, and replays to the same state after rotation (`TestV010LogReplaysTheSameAfterRotation`).
- A temporary file left in `segments/` by a crash during a rotation stays until removed by hand; it is ignored by replay.
- Committing the log to Git at checkpoints is recorded in [ADR-0011](0011-event-log-git-commits.md). Compaction (dropping records a checkpoint covers) is recorded in [ADR-0012](0012-event-log-compaction.md).

## Alternatives considered

- **A signed manifest per segment.** Rejected for now: it would sign again what every line already proves, and add a file that can disagree with its segment. The range in the name and the per-record checks find a cut, an extension and a changed line.
- **Rotating on append.** Rejected: it would make the append, which every writer queues for, as slow as the largest rotation.
- **Segments of variable length, rotated by size.** Rejected: a fixed count makes a segment's name say exactly which records it must hold, which is what lets replay find a truncated one.

## Verification

`internal/eventlog/rotate_test.go` and `TestStatusChangeRotatesClosedRanges` in `internal/units/checkpoint_test.go`. Each of the checks above was shown failing on a planted defect: replay without segments, an unchecked overlap, a short or an extended segment, a missing segment, rotating the HEAD record, keeping leftover copies, a headless log with segments, a checkpoint inside a segment, a moved record treated as final, a record with a line break, a temporary file read as a segment, and a units service that never rotates.
