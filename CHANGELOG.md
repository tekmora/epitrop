# Changelog

All notable changes to this project will be documented in this file.
The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]


## [0.3.0-rc.1] - 2026-11-22

### Security

- `tools/spectral/package-lock.json` moves fast-uri to 3.1.9, and the new `make lock-advisories` gate (osv-scanner, run at pre-push and in CI) fails on any published advisory in that lockfile (a dated exemption for the picomatch advisory GHSA-fc5q-39xc-vjpf ended when that advisory was withdrawn, see Fixed, so this release carries none), and `make govulncheck-osvscanner` scans the osv-scanner module itself for reachable Go vulnerabilities
- `make gosec` scans test files and files behind the repository’s own build tags (derived from its `//go:build` lines), requires a rule and a reason on every `#nosec`, refuses a file no scan pass reaches, and the findings this exposed are fixed or annotated (#379)
- Errors from the shared HTTP fetch helper print request URLs with the password redacted, and a URL that fails to parse is no longer echoed in the error (#380)
- The schema refresh tool refuses manifest URLs and redirect targets that carry user info, and its errors print URLs with the password redacted (#380)

### Added

- Rank the task-graph frontier by four terms read from the event log (elapsed time, progress, cost, confidence), with node ID as the final tie-break. (#10)
- A job can set restart.window_seconds, so restart.max_restarts bounds the restarts in that window instead of every restart of its life (#285)
- epitrop-server classifies each failed job exit with the dead-letter classes and appends one `job.dead_lettered` record (#9)
- A slot home bind can name its token's expiry with the new `expires_rfc3339`, the dotted path of an RFC 3339 time, and a missing or unparsable expiry is refused before the start; the Antigravity CLI login it was added for is refused in this release (see Changed)
- `epitropctl jobs supervise` reports no job alive once per episode while recoverable and always writes a terminal exit line: unrecorded, completed (exit 0), or stopped once jobs and stopped always jobs are excluded; other failed once runs count as down without recovery, running jobs and armed cron/event jobs are alive, recoverable windowed budgets keep supervision running with a restart due time, and terminal failures exit 3 (`RestartPreventExitStatus=3` prevents retries); a job becoming alive ends the episode, and `jobs stop` keeps on-failure always jobs stopped (#36)
- Publish the documentation as a site on GitHub Pages (https://tekmora.github.io/epitrop/) and lay out the README to the ecosystem standard, with a test that keeps its release badge on the newest released version.
- The Upstream schemas workflow also runs weekly on main, so a change in a live upstream document shows as its own red run instead of failing an unrelated pull request
- A system test restarts the supervisor while a job, the release watch and open units are active and checks each returns to its state (#399)
- Each supervised job bounds its active log size and keeps a fixed number of rotated log files on whole-line boundaries through jobs[].log_rotation (#404)
- catalog/testdata/contract/ pins the snapshot contract with valid, degraded, stale, and incompatible-version fixtures a consumer replays against its decoder (#29)

### Changed

- A slot run refuses a job with sandbox.network egress: it reaches its declared gateway (proxy) or nothing, also when a run is resumed (#139)
- The supervisor takes its job lock through gofrs/flock, the same library as the event log lock; a job started while the lock is held gets no descriptor of the lock file, and closing a released lock twice is no error
- The Claude ceiling store and the job record a job shim writes are replaced atomically through one shared helper: the new file is written and synced beside the old one, renamed into place and the folder synced; a reader sees the old file or the new one, never a partial file; a failure before the rename leaves the previous file in place; the helper tries to remove the staged file; if that removal fails as well the returned error names both failures and the staged file stays behind; a failed folder sync after the rename returns an error with the new file already in place and no staged file left
- `epitropctl sync` reads the clock once and stamps that time on the snapshot and on every record of every source; each source's `Fetch` takes the time as a parameter
- Metric instruments come from the golusoris catalog; counters and gauges now carry floating-point values
- The schema refresh tool refuses a manifest URL that does not use https with a host and refuses any redirect off https, and the docs record the provenance rule for unsigned upstream schema documents
- A slot home refuses the consumer Antigravity CLI login (`.gemini/antigravity-cli/antigravity-oauth-token`), in the loader and in the shim, until business authentication lands (#367)
- The Praetor audit excuses the managed API compatibility job's 60-minute timeout until 2026-12-01, since that workflow is locked to the engine and cannot take its 30-minute bound yet (#436)

### Fixed

- `epitropctl slot run --detach` hands the service the caller's values of the job's `sandbox.env_allow` names, so a detached job sees the same environment as an attached one (#139)
- The release watch reads the marker search 10 issues to a page under a 4 MiB page cap, so a sink with long issue bodies no longer fails every pass (#284)
- Sandboxed jobs resolve localhost: every sandbox gets a read-only /etc/hosts that names only localhost, never the host's file
- A connection refused as busy is drained under a deadline before closing, so closing a socket with unread data sends no reset (#314)
- A request the egress proxy refuses for its form, its destination or an upstream failure is drained under a deadline before the connection closes, so the client reads the answer instead of a reset (#318)
- Built with Go 1.27.4, which carries the fix for GO-2026-6809 in crypto/x509 certificate verification that Go 1.27.3 first shipped
- A cron job's due time is its minute slot, also with gronx v1.20.7
- `make gosec`, and the CI steps that now call it, scan the packages the `go` command builds: nested worktrees under `.claude/` and `.standards/` and other dot folders are no longer scanned, and a self-test proves a planted finding in the tree still fails the scan
- A job restarts again once its oldest restart is older than the default one-hour window; every schedule records a supervisor-stop end as `job.interrupted`, an `always` job resumes without spending budget unless its policy is `never` or it was stopped on purpose since, and a job whose exit was recorded before the stop still spends budget (#36)
- The picomatch advisory GHSA-fc5q-39xc-vjpf was withdrawn, so its exemption is gone from osv-scanner.toml and .standards.yaml and the lockfile gate excuses no advisory; the osv-scanner self-test now plants braces 3.0.2 (GHSA-grv7-fg5c-xmjg) and names a withdrawn plant as such
- A non-200 reply with a body larger than the byte cap reports its HTTP status instead of a body-too-large error, so the release watch and the admission probe name the status (#380)
- `config.Load` refuses case-only and hard-link aliases of another job's `log_path` or any job's rotated `log_path.1` to `log_path.101`, so jobs cannot share or overwrite a rotated log file (#442)


## [0.2.0] - 2026-10-18

This release ships the code of `v0.2.0-rc.1` unchanged.

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

