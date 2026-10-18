# Architectural Decision Records (ADRs)

This directory documents key architectural decisions. Records are append-only
like public APIs under HISS-14 (Append-Only ABI & Migration Footers).
An accepted record keeps its number and text; a changed decision is a new
record that supersedes it.

| Number | Date | Title | Status |
| :--- | :--- | :--- | :--- |
| [0000](0000-template.md) | 2026-09-11 | ADR template | Accepted |
| [0004](0004-golusoris-pinned-base.md) | 2026-10-09 | Base the control plane on golusoris, pinned | Accepted |
| [0005](0005-sandboxed-job-runner.md) | 2026-10-10 | Sandbox job execution with a systemd scope, bubblewrap and pasta | Accepted |
| [0006](0006-proxy-socket-egress.md) | 2026-10-11 | Reach declared destinations from a sandbox without a network through a proxy socket | Accepted |
| [0007](0007-job-arguments-unchanged.md) | 2026-10-11 | Start a sandboxed job with its arguments unchanged | Accepted |
| [0008](0008-sandbox-input-roots.md) | 2026-10-15 | Allow sandbox paths only inside declared roots | Accepted |
| [0009](0009-slot-client-home.md) | 2026-10-15 | Give a slot's coding client a home of its own with its login bound read-only | Accepted |
| [0010](0010-event-log-segments.md) | 2026-10-17 | Move closed ranges of the event log into segment files | Accepted |
| [0011](0011-event-log-git-commits.md) | 2026-10-17 | Commit the event log to Git at checkpoints, onto a ref of its own | Accepted |
| [0012](0012-event-log-compaction.md) | 2026-10-17 | Compact the event log onto a signed base once Git holds its segments | Accepted |
