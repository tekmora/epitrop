# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]


## [0.2.0-rc.1] - 2026-10-18

### Added

- A sandboxed job can reach a list of declared destinations and nothing else: `sandbox.network: proxy` with `sandbox.destinations` (#25)
- Add signed unit checkpoints for fast replay, preemption at slot safe points, and refusal of diverged log copies (#11)
- **BREAKING**: Interval and event job triggers in supervisor, refusing unexecutable schedules or restart policies on triggered jobs at config load (#126)
- Take up interrupted slot runs with epitropctl slot resume, adopting the running or ended job without a second start and recording the result as an uninterrupted run (#127)
- Traces and metrics for slot runs, supervised jobs, side effects and task-graph queue depth, exported over OTLP when an endpoint is set (#34)
- Start a slot run as a systemd user service of its own with epitropctl slot run --detach, carrying the template's memory_max and memory_high, so the run outlives the session that started it
- units frontier, assign and release: list the graph nodes a slot may take, claim one and open its unit in one record, and give it back with a reason
- units handover: the open units of one repository with their pending decisions and unread notes, written as a slot's resume note on request
- `epitropctl watch` accepts `--github-base`, `--huggingface-base`, and `--ntfy-base` flags to point API requests at local stand-ins (#145)
- A sandboxed job can declare paths inside its workspace read-only, and cannot rename them away (#128)
- End a hung job whose heartbeat, the last write to its log, is older than heartbeat.max_age_seconds, recording job.stale and leaving the restart to its policy (#21)
- Sync reads the calling LiteLLM key's budget window, and epitropctl headroom reports a stale or missing usage reading as unknown, never as zero (#24)
- Cutover 1 plan: the fleet's background jobs declared as Epitrop jobs in `docs/cutover-1.example.yaml`, with the parallel run and switch-over criteria in `docs/cutover-1.md` (#36)
- `docs/config.example.yaml`: running the release watch's supervisor as a systemd user unit that loads its token with `LoadCredentialEncrypted=`, with the token's minimal rights (#23)
- Release watch: every pass ends with one summary line (sources polled and failed; releases new, filed and deferred; whether the rate-limit throttle stopped it), so a pass without new releases still shows in the job log (#271)
- Catalog: the usage window carries `burn_percent_per_hour` and `active_agents` (optional, at least zero; schema and validation refuse anything else); codex-local measures both over the last 15 minutes of session logs, and litellm-key-budget names both as absent. The schema stays at version 1, so a reader that validates against the v0.1.0 schema refuses snapshots carrying the new fields until it takes the new schema (#274)
- **BREAKING**: Sync: the claude-local source reads Claude Code session logs and writes the five_hour and seven_day usage windows; used_percent and the burn rate are derived from a ceiling observed at a rejected request and stay absent with a reason until one is seen, and epitropctl claude-limits lists the limit types the logs name; a snapshot may now hold 25,000 records, which a reader from an earlier release refuses (#276)

### Changed

- **BREAKING**: Loading a configuration refuses a job or watch whose `router_alias` is not listed in `router.aliases`
- **BREAKING**: Allow a sandboxed job's workspace and inputs only inside the directories sandbox_input_roots declares, and refuse login locations below HOME unless a root names exactly one
- Cutover 1 parallel run: shadow jobs for the landing daemon, auto-rebase, the update intake and the monitor run each script's dry mode with a heartbeat bound (#36)
- `docs/cutover-1.md`: the shadow supervisor's host unit tags its journal lines with its own name, so its exit reason can be found by `journalctl -t` (#263)
- **BREAKING**: The /events HTTP stream serves signed event-log records in sequence order, replacing unit and job diff summaries; each event carries the record sequence number as its id and the event type as its event name, and clients can resume from any record with Last-Event-ID (#278)

### Fixed

- A sandboxed job gets its arguments as its config wrote them: `systemd-run` no longer replaces `${NAME}` and `$$` in them, and the value of an allowed variable no longer lands on the argv (#25)
- A run-record, task-graph or effect-resolution write that loses a race to another writer is now refused instead of reported as written (#108)
- A sandboxed job with egress network no longer logs pasta's "Couldn't get any nameserver address" line (#137)
- A release watch run without GITHUB_TOKEN now names the missing token when a sink search is refused, and logs once when rate_limit_floor is at or above the GitHub rate limit (#138)
- `epitropctl watch` refuses a release watch seen-set file whose version is not 1, instead of reading it and rewriting it as version 1 (#148)
- The signed event log and the epitropctl commands that write it work on Windows, and Windows test suites run every package (#152)
- Stopping a job whose shim is exiting no longer fails with "job lock is not held"; the job counts as stopped (#227)
- A sandboxed job start is no longer refused when the kernel still shows the shim's program for the job right after the start. (#228)
- `epitropctl jobs supervise` no longer ends after 30 seconds with a deadline error; it runs until SIGTERM or an interrupt and then exits 0 (#36)

### Migration

- Snapshots (#276, #277): upgrade every program that reads snapshots before a writer selects sources whose caps sum past 20,000 records. Code that only compares against `catalog.MaxSnapshotRecords` needs no change.
- Event stream (#278, #279): `/events` no longer sends the unit and job summary events. Read event names as record types (`unit.recorded`, `job.started`, ...), parse `data` as the record, and read the current state back with the tools after a 410.


## [0.1.0] - 2026-10-11

### Changed

- No change since 0.1.0-rc.3: the first release is the code of that candidate, and the changelog sections of the release candidates list what it holds. Known gap: a sandboxed job with the network mode egress reaches the whole internet, and no mode limits a job to declared destinations. A job started by slot run that needs its model gateway therefore has network access to every address. Run no real work unit in a slot until a later release closes this (#25)


## [0.1.0-rc.3] - 2026-10-11

### Fixed

- List the snapshot schema in checksums.txt, so a release signs and attests it with the archives, and take the release notes from the changelog section of the tag (#29)
- Stopping a job whose process ends while its status is read now reports the job as gone and not as an error (#21)


## [0.1.0-rc.2] - 2026-10-11

### Added

- Base the control plane on the golusoris framework at a pinned version (#30)
- Load configuration from one file checked against a published JSON Schema (#31)
- Record state in an append-only event log with signed records and replay (#32)
- Add the acyclic task graph with staged, bounded changes (#5)
- Supervise declared jobs with a per-job shim, restart and a validated stop path (#21)
- Run supervised jobs in a sandbox with cgroup limits, a restricted filesystem view and a chosen network mode (#25)
- Watch upstream releases and file one issue per release (#23)
- Keep a run record of work units and answer which units to resume (#22)
- Record a refused job start as a job.refused event (#94)
- Stop a job at its own deadline with max_runtime_seconds (#95)
- Give a sandboxed job an optional soft memory limit with memory_high (#98)
- Keep an evidence pointer on the unit record (#99)
- Add the catalog data-sync guide (#61)
- Keep the task graph in the event log: graph apply commits a staged change as one record, graph show rebuilds the graph from the log (#5)
- Run each outbound side effect at most once: a ledger in the event log keyed by task and action type, with effects list and effects resolve (#7)
- Run one work unit in a slot: slot run prepares a fresh clone, starts a sandboxed job from a one-job template, checks the result and the branch the run left, and records the unit with an evidence pointer (#25)

### Changed

- Require govulncheck and gosec on every pull request (#57)
- **BREAKING**: Rename Tribunus to Epitrop: the module is github.com/tekmora/epitrop, the command is epitropctl, the default event log directory is .epitrop and job scopes are named epitrop-job-<id>.scope

### Fixed

- Refuse a sandboxed job found outside its systemd scope (#87)
- Collect the shims a supervisor starts (#90)
- Keep the last replay error when the wait for a job start ends (#91)
- Let the supervisor tests compile on macOS and Windows (#93)
- Let schemas-repin move a pinned upstream document (#56)
- Apply event_log.lock_timeout_seconds in the supervisor and the job shim, which waited the default 5 s whatever the config said (#83)

## [0.1.0-rc.1] - 2026-10-09

Not published: the release run for this tag was cancelled, so no artefact exists for it. 0.1.0-rc.2 supersedes it.

### Added

- Run the adopted verification gates on every pull request and main branch push (#1)
- Adopt the Praetor governance harness and local verification gates (#26)
- Publish tribunusctl binaries, the snapshot schema, CycloneDX SBOMs, signed checksums and build provenance for every release tag (#29)
- Configure Renovate and exclude Praetor-managed assets from direct dependency updates (#18)
- **BREAKING**: Version the catalog snapshot with schema_version 1 and publish its JSON Schema (#29)
- Check catalog sources against pinned upstream schemas before accepting snapshot changes (#28)

### Fixed

- Refuse silent fallback in catalog source collection and surface degraded source runs explicitly (#27)
- Publish release-candidate tags as prereleases, never as the latest release (#29)
- Align the v2 architecture, roadmap and onboarding documents with the implemented catalog code (#3)

