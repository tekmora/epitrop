# ADR-0011: Commit the event log to Git at checkpoints, onto a ref of its own

- **Status**: Accepted
- **Date**: 2026-10-17, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

The event log ([architecture, Event log](../architecture-v2.md)) is a directory of plain files in a Git working tree, written only by the control plane. Nothing commits it: its history lives only on the disk it was written to. Issue #58 asks for batched commits at checkpoint boundaries, made by the control plane under its own identity, with Git and hooks kept off the append path. Compaction, the next step of #58, will move segments out of `events/` only once a commit holds them, so it needs these commits first.

The commits must not disturb the operator. The log usually lies in a repository the operator works in. A commit made with `git commit` would run the operator's commit hooks, take whatever the operator has staged, fail on a conflicted index and move the checked-out branch.

## Decision

1. **The log is committed at every checkpoint, when enabled.** With `event_log.git.enabled`, the units service calls `Writer.RecordInGit` (`internal/eventlog/gitrecord.go`) after it writes a checkpoint and rotates the log at a status change. The keys are `enabled` (default `false`), `ref` (default `refs/epitrop/events`), `author_name` and `author_email` (both required when enabled) and `timeout_seconds` (default 30, from 1 to 300), which bounds the Git commands of one commit together. A failed commit is recorded on the trace span and tried again at the next checkpoint. Appends never wait for Git.

2. **The commit holds the log byte for byte.** Under the log lock, `RecordInGit` stores every regular file in `events/`, `events/segments/` and `checkpoints/<name>/` as a blob with `git hash-object --no-filters`, so neither a clean filter nor line-ending conversion changes a record. Names that start with a dot (the lock file, the temporary files of an interrupted write) stay out, and so does every other file in the log directory. The tree's paths are relative to the log directory.

3. **The commit goes to a ref nothing checks out, built with Git's plumbing.** `RecordInGit` fills a private index in a temporary directory, writes the tree, and makes the commit with `git commit-tree` on top of the commit the ref names. When the ref already holds that tree it makes no commit. It moves the ref with `git update-ref` and the old value it built on, so a ref another writer moved meanwhile is refused, not overwritten. The operator's index, working tree and branch are never touched, and no commit hook runs. Git runs with the caller's `GIT_*` variables and the global and system configuration dropped (`internal/gitexec`, shared with the slot runner), and the repository's own configuration still applies; a `reference-transaction` hook the repository declares runs on the ref update, as it does for every ref update.

4. **Git does not sign the commit.** Every record and checkpoint it holds carries its own Ed25519 receipt, so the content is proven without a commit signature, and no further key is needed. The author and committer are the configured identity; the dates come from the log's clock.

## Consequences

- With `enabled` set, the log's history is kept in the repository's object store on `refs/epitrop/events`. Pushing that ref is a separate step that this record does not cover.
- A log directory outside any Git repository fails the commit with the path; the log itself is unaffected.
- A crash during a commit leaves the log and the ref as they were, with at most objects no ref names, which `git gc` removes.
- Compaction can now require a segment to be held by a commit before it leaves `events/` (#58).

## Alternatives considered

- **Committing onto the checked-out branch with `git commit --only`.** Rejected: it runs the operator's commit hooks, races what the operator has staged, fails on a conflicted index and mixes the log's commits into the operator's history.
- **Signing the commits with a key of their own.** Rejected for now: it adds a key the operator has to manage and a way for the commit to fail, and signs again what every record already proves.
- **`git add` with a private index.** Rejected: it applies clean filters and line-ending conversion, so the committed bytes could differ from the log's.

## Verification

`internal/eventlog/gitrecord_test.go`, `TestStatusChangeRecordsTheLogInGit` in `internal/units/checkpoint_test.go`, `TestRunUnitsSetRecordsTheLogInGitWhenEnabled` in `cmd/epitropctl/units_test.go`, and the `event_log.git` cases in `internal/config/config_test.go`. Each check was shown failing on a planted defect: a clean filter applied, dot files committed, checkpoints left out, the operator's index used, a ref update without its old value, a commit made for an unchanged log, a commit without its parent, the private index left behind, no log lock, no validation, a units service that never commits, a command that does not pass the setting on, an author left optional, and an open ref pattern.
