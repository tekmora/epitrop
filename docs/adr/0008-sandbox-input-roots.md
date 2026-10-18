# ADR-0008: Allow sandbox paths only inside declared roots

- **Status**: Accepted
- **Date**: 2026-10-15, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

[ADR-0005](0005-sandboxed-job-runner.md) item 3 lists what a job's `workspace` and `inputs` may not be: the event log, the signing key, the config file, the running executable, `/`, `$HOME` and any directory containing `$HOME`. Every other path passes. That is a list of unsafe cases, and it misses most of them:

- A path below `$HOME` that does not contain it passes, so an input of `~/.ssh`, `~/.config/gh` or a coding client's login folder hands the job that login.
- A path outside `$HOME` passes, so `/run/user/<uid>` (the session bus and agent sockets), another user's home or a service's state below `/var/lib` can be bound into the sandbox.

A headless agent slot ([architecture N23](../architecture-v2.md)) runs a coding client as a sandboxed job. The client needs its own login folder as an input and must not see any other login on the host. Under item 3 nothing stops a template from binding the parent of that folder.

## Decision

1. **Every sandbox path lies in a declared root.** The top-level `sandbox_input_roots` lists at most 32 directories. Each resolves through symlinks at load and must exist. A root that is `/` or contains `$HOME` is refused. A job's `workspace` and each of its `inputs` resolve through symlinks and `..` first, and must then lie in one root; a path that only shares a name prefix with a root does not. Lying outside `$HOME` passes nothing on its own. With no root, no sandboxed job loads.

2. **Login locations stay shut as a second barrier.** A path that equals, contains or lies inside one of these locations below `$HOME` is refused, even when a root contains it:

   `.ssh`, `.gnupg`, `.netrc`, `.git-credentials`, `.config/git/credentials`, `.config/gh`, `.docker`, `.aws`, `.azure`, `.config/gcloud`, `.kube`, `.password-store`, `.pki`, `.local/share/keyrings`, `.claude`, `.claude.json`, `.codex`, `.gemini`, `.config/opencode`, `.local/share/opencode`.

   A root that names one location exactly opens that location alone: a root of `~/.claude` lets a job take `~/.claude` or a file in it, and still refuses `~/.codex`, `~/.ssh` and a symlink in `~/.claude` that leads to either.

3. **The checks of ADR-0005 item 3 stay.** The protected control-plane paths, `/` and `$HOME` are refused before the roots are consulted, and the shim checks again at job start.

4. **Tests prove it in both directions.**

   | Test | What it shows |
   | :--- | :--- |
   | `TestLoadSandboxPathsLieInADeclaredRoot` | a path outside every root, in `/run/user/<uid>`, beside a root under a shared name prefix or reached through `..` is refused; no roots refuse every job; a root of `/`, of `$HOME` or containing `$HOME` is refused. |
   | `TestLoadSandboxInputsRefuseLoginLocations` | below a broad root every planted login is refused and a folder without one is taken; a root naming one client folder opens that folder alone. |
   | `TestLoadSandboxInputsFollowSymlinksToLogins` | a symlink in a root or in an opened client folder that leads to another login is refused under the login's name. |
   | `TestSandboxedJobReadsOnlyItsOwnClientLogin` | against the real sandbox: a job given its own client folder reads the login in it and none of ten others planted beside it. |

   Ten mutants of the checks and of the input binds each turn one of these tests red.

## What this record decides

1. `sandbox_input_roots` is required for any sandboxed job, and an empty list allows none.
2. The login locations above are refused below any root except one that names them exactly.

## References

Decisions made before this record, by the maintainer. The wording is paraphrased.

- `epitrop-n23-input-allowlist`, 2026-10-15: every input and the workspace must resolve inside a declared root; lying outside `$HOME` is no pass; the login list stays as a second barrier, and a root naming one client folder exactly opens only that folder; no roots means no inputs; the change ships as a breaking configuration change (paraphrased).

## Consequences

- **Positive**: What a job can read is what the configuration names, not everything item 3 failed to list. A slot gets its own client login and nothing else on the host.
- **Negative**: Every configuration with a sandboxed job must add `sandbox_input_roots`, slot templates included; one without it no longer loads. A login location not on the list is protected only by the roots. The list is checked by name below `$HOME`; a client that keeps its login elsewhere is not covered by item 2.
