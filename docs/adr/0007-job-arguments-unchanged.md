# ADR-0007: Start a sandboxed job with its arguments unchanged

- **Status**: Accepted
- **Date**: 2026-10-11, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

[ADR-0005](0005-sandboxed-job-runner.md) starts a sandboxed job through `systemd-run --user --scope`. Its item 2 says the shim execs one argv without a shell, and that no `env_allow` value appears on that argv.

With `--scope`, `systemd-run` itself rewrites the command it is given, from its own environment, before it execs it. The switch that controls this is `--expand-environment=`. It exists since systemd 254. For `--scope` it was off by default until systemd 258 turned it on; both facts are in the release notes of those versions. The shim passed no such switch, so what a job received depended on the host's systemd version.

Measured on systemd 262 with the variable `NAME` set and `OTHER` unset:

| Argument in the job's command | What the job received |
| :--- | :--- |
| `${NAME}`, alone or inside a longer argument | the value of `NAME` |
| `${OTHER}` | an empty argument |
| an argument that is exactly `$NAME` | the value of `NAME` |
| an argument that is exactly `$OTHER` | nothing: the argument is dropped and the ones after it move up |
| `$$` | `$` |
| `$NAME` inside a longer argument | unchanged |

The environment `systemd-run` reads is the one the shim builds for the job: `PATH`, `HOME`, `TMPDIR`, each `env_allow` name that is set, and the two bus variables. Two things follow:

- A command that names an allowed variable gets its value written into the argv, where any local user reads it in `/proc/<pid>/cmdline`. Item 2 of ADR-0005 rules that out.
- A script passed as one argument, for example to `bash -c`, reaches the interpreter with its `${...}` references already replaced and its `$$` halved.

## Decision

1. **The shim passes `--expand-environment=no` on every scope start.** The argv of ADR-0005 item 2 begins:

   ```text
   systemd-run --user --scope --quiet --collect --expand-environment=no
       --unit=epitrop-job-<id>.scope
   ```

   Nothing between the shim and the job rewrites an argument. A job that wants a variable's value reads it from its environment.

2. **A `systemd-run` without the switch stops the job start.** This extends the list in ADR-0005 item 4. The preflight runs `systemd-run --expand-environment=no --version`, with a 5 s deadline. The call starts no unit: it parses the switch and prints the version, and a `systemd-run` that does not know the switch exits non-zero at it. The error names the tool, the switch and systemd 254, and carries the tool's own message.

3. **No start without the switch on an older systemd.** The release notes say a scope on a systemd before 258 does not expand by default, so leaving the switch out there would give the same result. That path is not offered: it would be an argv this repository has never run, on a systemd it has no host for, and the result would rest on release notes alone.

4. **Tests prove it in both directions.**

   | Test | What it shows |
   | :--- | :--- |
   | `TestSandboxedJobArgumentsArriveUnexpanded` | against the real user manager and the real sandbox: five arguments of the forms above arrive unchanged. Without the switch the test fails with the allowed variable's value on the argv and one argument missing. |
   | `TestSandboxProductionArgvKeepsSystemdPrefix` | the switch is in the argv, before the unit name. |
   | `TestSandboxPreflightRefusesScopeRunnerWithoutNoExpand` | a stand-in that refuses the switch as an unknown option stops the preflight; a missing `systemd-run` is reported as missing, not as lacking the switch. |

## What this record decides

1. A job's argv is the command of its config, byte for byte.
2. systemd 254 is the oldest systemd a sandboxed job starts on.

## References

Decisions made before this record, by the maintainer. The wording is paraphrased.

- `epitrop-open-list-20261011`, 2026-10-11, how records change: an accepted record keeps its text, an addition is a follow-up record, and a new record ships as Proposed with its code (paraphrased).
- `fleet-systemd-run-expansion`, 2026-10-11, the finding: every `systemd-run` call passes `--expand-environment=no` (paraphrased).

## Consequences

- **Positive**: What a job runs is what its config says, on every supported systemd. An allowed variable's value stays in the environment and off the argv, as ADR-0005 item 2 states. A `$` in a command needs no escaping.
- **Negative**: A host with a systemd older than 254 cannot start a sandboxed job; the preflight says why. A job that relied on the expansion, by writing `${NAME}` in its command to get a value, now gets the text; it reads the variable from its environment instead. The behaviour of a systemd older than 254 at the probe is inferred from how `systemd-run` treats any unknown option, not measured on such a host.
