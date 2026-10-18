# Cutover 1: watchers and supervision

Cutover 1 ([#36](https://github.com/tekmora/epitrop/issues/36)) moves the fleet's background jobs from host service units and session-bound shell loops to the Epitrop supervisor. The scripts themselves stay as they are: Epitrop starts, watches and restarts them. Replacing what the landing scripts do is cutover 2 ([#38](https://github.com/tekmora/epitrop/issues/38)).

This page is the plan. [`cutover-1.example.yaml`](cutover-1.example.yaml) declares the jobs with placeholder paths, and `TestExampleConfigValidates` in `internal/config/config_test.go` loads it and `TestCutoverOneExampleIsASupervisorConfig` in `internal/supervisor/cutover_linux_test.go` checks it as the supervisor does. The operator's copy names the real scripts and lives in the private configuration, outside this repository.

## The jobs

| Job | Today | Under Epitrop | Can run beside the live script |
| :--- | :--- | :--- | :--- |
| `landing-daemon` | host service unit | `schedule: always`, `restart: on-failure` | as `landing-daemon-shadow`, in its dry mode |
| `auto-rebase` | host service unit | `schedule: always`, `restart: on-failure` | as `auto-rebase-shadow`, in its dry mode |
| `update-intake` | host service unit | `schedule: always`, `restart: on-failure` | as `update-intake-shadow`, in its dry mode |
| `fleet-monitor` | a loop a session starts, ends with the session | `schedule: always`, `restart: always` | as `fleet-monitor-shadow`, in its dry mode |
| `scratch-cleanup` | started by the monitor when space runs low | `schedule: "*/15 * * * *"`, `max_runtime_seconds: 600` | no: it deletes |

The queue restore loop, which waits for one pull request to merge and then relands the units queued behind it, is not declared. It takes the pull request and the queue as arguments for each wait, and a declared job's arguments are fixed ([ADR-0007](adr/0007-job-arguments-unchanged.md)). It moves with the landing queue in cutover 2.

Each script now prints one beat line on every pass, idle or not, so a heartbeat bound (`heartbeat.max_age_seconds`, #21) can tell a hung script from an idle one. The shadow jobs declare a bound of a few pass intervals: 300 seconds for the landing daemon and the monitor, 600 for auto-rebase, and 2700 for the update intake, whose pass waits ten minutes and may take thirty. The live jobs declare none until the shadow run has measured how long their passes take.

## Why the parallel run uses dry modes

Each job acts on shared state: the landing queue, the forge, the scratch directories, the monitor's record of reported issues. A second copy of a live script beside the running one acts twice, or takes work from the running copy. The parallel run therefore runs each script's dry mode under Epitrop, a mode that prints what the script would do and changes nothing, and compares that output with what the live script did.

Each script has a dry mode (`DRYRUN=1`) that writes nothing, and the shadow jobs run it. The dry modes are work in the scripts' own repository; Epitrop needs no change for them.

## Phases

1. **Declared (this change).** The jobs exist as configuration. `epitropctl jobs status --config <file>` loads the operator's copy and lists every job as `dead`, never started. No supervisor runs it.
2. **Parallel run.** A supervisor runs the shadow jobs only, outside any session, with the live scripts unchanged under their host units. Starting that supervisor is an operator decision, because it is a long-running process on the host.
3. **Switch-over.** One script at a time: its host unit stops, and its job moves from the shadow configuration to the live one. Each switch-over is an operator decision that cites a week of matching evidence. Undoing it means starting the host unit again and removing the job.

## Matching evidence

For each script, over seven consecutive days of the parallel run:

- **Decisions match.** Every action in the live script's log (a landing, a rebase, a requeue, a skip) has a dry-mode line for the same unit in the same pass or the next one, and the dry mode names no action the live script did not take. The comparison is per unit name, and a pass is the script's own interval.
- **Supervision holds.** The supervisor's event log shows every shadow job started on its schedule, each exit recorded, and no `job.stale` or restart that the host unit's journal does not also show for the live script. `epitropctl jobs history --config=<shadow config> --since=<week start>` counts these records per job: starts (adoptions included), exits, exits without code 0, restarts, stale beats, refusals and stops, with the times of the first and last counted record. A compacted log counts only the records after its base.
- **Sessions do not matter.** At least one operator session ends during the week, and `jobs_status` through MCP from a new session shows every shadow job with the same state as `epitropctl jobs status`.
- **A planted event fires.** One planted queue entry is seen by the dry mode after a supervisor restart.

A full disk under the event log makes the supervisor exit with an error naming the job, the event type and the event-log directory. Once disk space is freed, restarting the supervisor automatically repairs any half-written append past HEAD and re-adopts the running jobs without duplicate starts.

The supervisor's host unit sets `SyslogIdentifier=` to the unit's own name ([systemd.exec(5)](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html#SyslogIdentifier=)). The supervisor runs inside `bwrap`, and without the setting its output, including the error line it exits with, is tagged `bwrap`. After the 2026-10-17 full-disk exit that line carried no unit, so `journalctl --user -u <unit>` showed the exit status but not the reason ([#263](https://github.com/tekmora/epitrop/issues/263)). With the setting, `journalctl --user -t <unit name>` lists every line the supervisor wrote, the last one included.

A mismatch restarts the week for that script once its cause is fixed.

## Retired scripts

None yet. Each switch-over adds a row here: the script, the job that replaces it, the decision, and the evidence.
