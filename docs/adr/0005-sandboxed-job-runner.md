# ADR-0005: Sandbox job execution with a systemd scope, bubblewrap and pasta

- **Status**: Accepted
- **Date**: 2026-10-10, accepted 2026-10-11
- **Authors**: Epitrop maintainers

## Context

[RT-03](../architecture-v2.md#rt-03-control-plane-isolation) says task execution may affect the control plane only through messages the control plane validates. A task never gets the control plane's process, credentials, writable paths or network reach. The job supervisor (#21) starts every job through a per-job shim. The shim is the one place where a job's process is created, so it is where isolation must apply.

The first target is the Linux workstation that runs the supervisor as a per-host systemd user unit. Isolation has to work without root. It must not freeze the interactive desktop under a runaway build. It must fail closed: a job that asked for a sandbox never runs without one. A Firecracker tier follows later through a separate sandbox binary; this record covers the process sandbox only.

## Decision

1. **Sandbox by default.** Every job runs sandboxed unless its config sets `sandbox.mode: off` with a non-empty `reason`. The shim records the mode it applied in the `job.started` event (`enforce/none`, `enforce/egress` or `off: <reason>`). Status shows that recorded mode for a running job, never the current config, which may have changed since the start; a stopped job shows the configured mode its next start will use.
2. **One argv, built by the shim.** For `mode: enforce`, the shim execs (argv only, no shell):

   ```text
   systemd-run --user --scope --quiet --collect --unit=epitrop-job-<id>.scope
       -p MemoryMax=<memory_max> [-p MemoryHigh=<memory_high>]
       -p CPUWeight=<cpu_weight> -p TasksMax=<tasks_max> --
   env --ignore-signal=TERM
   [egress only] pasta --config-net --address 192.0.2.2 --netmask 24 --gateway 192.0.2.1
       --map-host-loopback none -t none -u none -T none -U none -D none --quiet --
   bwrap --die-with-parent --unshare-all [egress only: --share-net]
       --ro-bind /usr /usr --symlink usr/bin /bin --symlink usr/lib /lib --symlink usr/lib64 /lib64
       --ro-bind /etc/ssl /etc/ssl --ro-bind /etc/ca-certificates /etc/ca-certificates
       [egress only] --ro-bind /run/systemd/resolve/resolv.conf /etc/resolv.conf
       --proc /proc --dev /dev --tmpfs /tmp --tmpfs /var/tmp
       --bind <workspace> <workspace>
       [--bind <read_only ancestor> <read_only ancestor>]... [--ro-bind <read_only> <read_only>]...
       [--ro-bind <input> <input>]...
       --ro-bind <resolved command path> <resolved command path>
       --chdir <workspace> --setenv PATH /usr/bin:/bin --setenv HOME <workspace>
       --setenv TMPDIR /tmp --unsetenv XDG_RUNTIME_DIR --unsetenv DBUS_SESSION_BUS_ADDRESS
       --unsetenv INVOCATION_ID
       -- env --default-signal=TERM <command>...
   ```

   The shim starts this argv with an environment it builds itself: `PATH`, `HOME`, `TMPDIR`, each `env_allow` name that is set, and the two variables `systemd-run --user` needs to reach the user manager. No value from `env_allow` appears on the argv, where any local user could read it in `/proc/<pid>/cmdline`.

   | Part | Why |
   | --- | --- |
   | `systemd-run --user --scope` | a cgroup per job with memory, CPU and task limits, so a runaway job cannot starve the desktop. `memory_max` is the hard limit, at which the kernel kills; the optional `memory_high` is a soft limit below it, above which the kernel throttles the job and reclaims its memory first. With `--scope` it execs the command in place, so the shim's process group and parent-death signal carry into the job. The shim names the scope itself, one unit per start, and confirms the job's cgroup is that unit before it records the start (item 4). |
   | `--die-with-parent` | the sandboxed command dies when bwrap or the shim dies, which keeps the supervisor's no-orphan guarantee. |
   | no `--new-session` | the job stays in the process group the shim created, so a graceful SIGTERM from stop reaches it. TIOCSTI injection, which `--new-session` guards against, must be disabled in the kernel (`/proc/sys/dev/tty/legacy_tiocsti` = 0); the shim refuses to start otherwise. |
   | `--unshare-all` | new user, pid, ipc, uts, cgroup and network namespaces: the job cannot see or signal the shim or the supervisor. |
   | `env --ignore-signal=TERM` … `env --default-signal=TERM` | stop sends SIGTERM to the whole process group, which includes bwrap and pasta. Without this, both die on it and `--die-with-parent` SIGKILLs the job before its own TERM handling runs; in a test before this decision, 0 of 5 graceful stops reached the job. bwrap and pasta now start with TERM ignored, and the job gets TERM back at its default just before it starts (5 of 5). SIGKILL, used for escalation and on shim death, cannot be ignored. |
   | pasta with every forwarding flag `none` and `--map-host-loopback none` | egress without the host's loopback. pasta's defaults (`-t/-u/-T/-U auto`, gateway mapped to host loopback) forward host-listening ports into the namespace. In a test before this decision, a local model server on 127.0.0.1 answered from inside the sandbox. pasta is started under the name `pasta`, never its resolved symlink target: pasta and passt are one binary that picks its mode from its name. `-D none` turns off the DNS addresses pasta would hand on: the job never uses them, since it reads the resolver file the sandbox binds in. Without the flag, pasta logs `Couldn't get any nameserver address` into the job log on a host whose only resolver is on loopback. |
   | `--address 192.0.2.2 --netmask 24 --gateway 192.0.2.1` | a namespace address from TEST-NET-1 (RFC 5737). By default pasta copies the host's own address into the namespace, so the job cannot reach services on the host's LAN address: those packets never leave the namespace. |
   | `/run/systemd/resolve/resolv.conf` | the upstream resolvers. The host's `/etc/resolv.conf` names the systemd-resolved stub on loopback, which the sandbox must not reach. |
   | shim-built environment plus `--unsetenv` | the control plane's environment, which holds its tokens, never reaches the job. Only the names in `env_allow` are copied. bwrap removes the user-manager variables before the job starts, so the job sees exactly `PATH`, `HOME`, `TMPDIR`, `PWD` and its allowlisted names. The names the sandbox sets or removes are refused in `env_allow`. |

3. **Paths are checked at config load and again at start.** `workspace` and each `input` resolve through symlinks. A path that equals, contains or lies inside the event log directory, the signing key, the config file or the running Epitrop executable is refused, as is `/`, `$HOME` or any directory containing `$HOME` (also reached through a symlink). The executable is protected because the supervisor re-executes it as each job's shim: a job that could write it would run its own code outside the sandbox at the next start. Config load fails closed when `$HOME` cannot be found. At job start the shim checks the workspace and inputs against the event log and the signing key again, and refuses a path whose symlink resolution changed since load.

   `read_only` names paths inside the workspace, relative to it, that the job may read and not write: a working copy's hooks, for one (#128). Each must exist, be a clean relative path and resolve through its symlinks to a place strictly below the workspace; read-only paths may not overlap each other or the home's `.slot` directory, and the list holds at most 32. A missing path is refused, at config load and again at every start, rather than skipped: a path the job could create in the writable workspace would not be read-only. The start resolves the list again, so a path swapped for a symlink out of the workspace after load is refused (`TestBuildSandboxCommandRefusesAReadOnlyPathSwappedOut`). bwrap binds each path read-only over the writable workspace. A read-only bind alone does not hold: the job can rename its parent directory and create a writable directory of the same name. Each directory between the workspace and a read-only path is therefore first bound onto itself, writable as before; a mount point cannot be renamed. `TestSandboxedJobCannotChangeItsReadOnlyPaths` writes, adds, removes, renames the path and renames its parent, against real bwrap.
4. **Fail closed.** A missing `bwrap` or `systemd-run`, an `env` outside `/usr` or without `--ignore-signal` and `--default-signal` (GNU coreutils 8.31 or later), a missing `pasta` or resolver file for egress, or TIOCSTI not disabled stops the job start with an error that names the missing piece. There is no fallback to an unsandboxed run. Other platforms report `not-supported` for `mode: enforce`.

   The same holds after the start. `systemd-run` exiting 0 does not show where the job runs, so the shim reads the job's cgroup from `/proc/<pid>/cgroup` and appends `job.started` only when it ends in the job's own scope unit. A unit name of its own is what makes the check exact: the cgroup the shim itself runs in may be a scope too. `systemd-run` execs the job only after the user manager placed it, so a process that is no longer `systemd-run`, or has exited, outside its scope is refused at once; while it is still `systemd-run` the shim waits up to 10 s. It also waits while `/proc/<pid>/exe` still names the shim's own program: right after the start returns, the kernel can show the parent's program for a moment before the new one, measured in about 3 of 1,000 starts under load, and a job refused in that moment was one that ran correctly (#228, `TestConfirmJobScope`). Whether the process has exited is asked of the kernel's wait interface, not read from `/proc`: `/proc` shows a zombie also for the moment in which a process with several threads execs from a thread other than its first, and a job taken for exited in that moment would be refused although it runs. A refused job's whole process group is killed. The reason is written to the job's log and recorded as a `job.refused` event, so a caller can act on a refusal without reading a log, and `jobs start` returns it. The same event records every other reason the shim did not start a job: a sandbox that cannot be built, a command that cannot start. `TestSandboxedJobOutsideItsScopeIsRefused` plants a scope command that reports success and starts the job without creating a scope; `TestSandboxedJobCgroupMemoryMax` is the allowed case, against the real user manager.
5. **RT-03 fixtures prove it in both directions.** Each case under `internal/supervisor/testdata/rt03/` uses the fixture format of the architecture document. Every boundary has a refused and an allowed case:

   | Boundary | Refused (violation) | Allowed (compliant) |
   | --- | --- | --- |
   | paths | write into the event log, overwrite the config file, write `$HOME` | write in the workspace |
   | credentials | read a secret variable that is not allowlisted; read the signing key | read an allowlisted variable |
   | process | signal the shim or the supervisor | signal the job's own child |
   | network | any connection with `network: none`; with `egress`, reach a listener on host 127.0.0.1 through the namespace's own loopback or through the gateway address | with `egress`, connect to a listener on the host's non-loopback address |

   The checker fails when a violation is allowed or a compliant action is refused. The loopback listener binds below the kernel's ephemeral port range, where host services listen: pasta's automatic forwarding skips ephemeral ports, so a listener on port 0 would never show a forwarding leak. Every rule above was removed once, in a copy, to show a test failing (rule 13); each network rule is caught by its fixture, not only by the argv test.

## What this record decides

Some choices behind the runner were made by the maintainer before this record was written; the record builds on them and they are listed under References. The others were made while building the runner and are decided here:

1. Sandbox by default. `mode: off` needs a written reason, and the mode is recorded with every start (item 1).
2. One argv, built by the shim, without a shell. The shim builds the environment, and the value of an allowed variable never appears on the argv (item 2).
3. Egress through pasta with every forwarding off, a namespace address from TEST-NET-1 and no mapping of the host's loopback. There is no destination filter yet (item 2).
4. No `--new-session`. The kernel's TIOCSTI switch has to be off instead, and a start fails when it is not (items 2 and 4).
5. The two `env` wrappers around the sandbox, so that a stop signal reaches the job and not only its sandbox (item 2).
6. The path rules: what a workspace or an input may not be or contain, checked at config load and again at start (item 3).
7. The list of what stops a start: which missing tool or setting, and no fallback to an unsandboxed run (item 4).
8. A scope unit of its own per start, `epitrop-job-<id>.scope`, as what makes the scope check exact (items 2 and 4).
9. The RT-03 fixture set as the proof, with a refused and an allowed case per boundary (item 5).
10. Read-only paths inside the workspace: must exist, checked at load and at start, with their ancestor directories bound onto themselves against a rename (item 3).

## References

Decisions made before this record, by the maintainer. The wording is paraphrased.

- `structure-06`, 2026-10-10, sandbox tiers: the process sandbox is built now; a virtual-machine tier follows later through a separate sandbox binary (paraphrased).
- `structure-16`, 2026-10-10, where the supervisor runs: as a per-host systemd user unit (paraphrased).
- `sandbox-scope-ci`, 2026-10-10, a job outside its scope: the shim fails closed unless the started job sits in its own scope, and the tests need a real scope (paraphrased).
- `tribunus-open-questions-20261010`, 2026-10-10, three rulings on the scope check: the waits of 10 s and 15 s stay as margins with their measured basis in the code and the measured latency in the CI log; a refused start is recorded as an event; a missing scope fails the tests in CI (paraphrased).
- The slot plan of 2026-10-10, soft memory limit: a sandboxed job may carry `memory_high` beside `memory_max` (paraphrased).

## Consequences

- **Positive**: Jobs, including the release watch (#23), run without the control plane's credentials, paths or loopback. The flags and fixtures are written so other repositories, such as Praetor's agent sandbox, can reuse them. The loopback finding becomes a test anyone can run.
- **Negative**: Linux only. Egress is all-or-nothing: there is no per-destination allowlist yet. Short-lived minted credentials and `rt03.violation` events need the broker and follow later. CI runners need `bubblewrap` and `passt` installed and unprivileged user namespaces allowed; anything the CI runner cannot prove (the systemd user scope) is proven on the workstation and said so in the test: the tests that need a scope first start a process the way the shim does. Outside CI they skip, with the reason, unless it ends up in its scope. Under CI a missing scope fails them: the hosted runner provides one, and a run without it would have lost the sandbox tests silently. CI runs the tests once, verbosely, and the scope probe logs the runner's systemd version and how long a job took to enter its scope.
