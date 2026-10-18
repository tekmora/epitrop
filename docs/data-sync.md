# Catalog data sync

`epitropctl sync` collects what Epitrop needs to know about models from a handful of sources and writes it to one JSON snapshot file: what each model costs, how much context it takes, which access path reaches it, and how much of a provider's limit is already spent. `epitropctl show` renders a snapshot as a table. The routing that reads these snapshots comes later (milestone 0.5); this page covers the sync.

## Install

Releases are tagged `v*` and carry signed binaries ([Releasing](releasing.md)). Download the archive for your platform with `checksums.txt` and `checksums.txt.sigstore.json`, then verify before unpacking:

```bash
cosign verify-blob \
  --certificate-identity "https://github.com/tekmora/epitrop/.github/workflows/release-binaries.yml@refs/tags/<tag>" \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --bundle checksums.txt.sigstore.json checksums.txt
sha256sum --check --ignore-missing checksums.txt
gh attestation verify epitropctl_<version>_linux_amd64.tar.gz --repo tekmora/epitrop
```

To build from source instead, pin the version:

```bash
go install github.com/tekmora/epitrop/cmd/epitropctl@<tag>
```

`epitropctl version` prints the release version; a source build prints `dev`.

## Running it

```bash
# every source, written to ./epitrop-snapshot.json
epitropctl sync

# a subset, explicit output path
epitropctl sync --sources=codex-local,ollama-local --out=/tmp/snapshot.json

# litellm-gateway needs both flags, or it reports a skip
epitropctl sync --sources=litellm-gateway \
  --litellm-base=https://litellm.example.com \
  --litellm-token-file="$HOME/.config/<gateway>/agent-token"

# render a snapshot; print the snapshot's JSON Schema
epitropctl show --in=/tmp/snapshot.json
epitropctl schema

# print every usage window as known or unknown; a reading older than 10m is unknown
epitropctl headroom --in=/tmp/snapshot.json --max-age=10m

# count the quota limit types and statuses the Claude Code session logs name
epitropctl claude-limits
```

The snapshot is replaced atomically: `sync` writes a temporary file beside `--out`, flushes it and renames it over the old file (`internal/atomicfile`), so a reader or a crash never sees half a snapshot. On Windows the rename is refused while another process holds `--out` open; `sync` then fails with that error and the old snapshot stays whole.

| Flag | Default | Meaning |
| --- | --- | --- |
| `--sources` | all five, in the order of the table below | comma-separated; an empty, duplicate or unknown entry is an error |
| `--out` | `epitrop-snapshot.json` | where the snapshot is written |
| `--litellm-base`, `--litellm-token-file` | none | required together for `litellm-gateway` and `litellm-key-budget`; the token is read from the file and never printed |
| `--ollama` | `http://localhost:11434` | Ollama daemon endpoint |
| `--sessions-dir` | `~/.codex/sessions` | Codex CLI session logs |
| `--claude-projects-dir` | `~/.claude/projects` | Claude Code session logs |
| `--claude-state-dir` | `<user state folder>/epitrop` | folder of the Claude ceiling store |
| `--claude-account` | `default` | account name of the ceiling store file |
| `--claude-ceiling-max-age` | `720h` | age past which a stored ceiling counts as unknown |
| `--openrouter-url`, `--litellm-prices-url` | the public OpenRouter API; the LiteLLM price map at a pinned commit | override only to test against a local fixture |

`sync` prints one `<source> <status> count=<n> [detail]` line per source. Sources run independently: one failing never discards another's records. The status is `ok`, `skip` (the source cannot run here, such as missing flags or no session logs), `fail`, or `degraded` (records were produced but upstream data was lost, such as rejected entries or one of the two public catalogs failing). `sync` exits non-zero when every selected source fails, after printing every line. The whole command times out after 3 minutes (`syncTimeout` in `cmd/epitropctl/sync.go`).

## The snapshot

A snapshot is `catalog.Snapshot` with `schema_version: 1`. Its JSON Schema is `catalog/snapshot.schema.json`, printed by `epitropctl schema` and shipped with every release. `epitropctl show` refuses a snapshot without a supported `schema_version` and asks for a new `sync`. A snapshot is exactly what one sync produced: nothing is merged across runs or sources.

Each record (`catalog.Record`) carries a model id, provider, access path (`api`, `subscription_cli`, `gateway` or `local`), context window, price per million tokens in and out, capabilities, rate limits and a usage window. Only the model id, access path and provenance are required. Provenance is `{source, fetched_at, kind}`: `measured` means read live from a real system, and `declared` means copied from a published catalog. One sync reads the clock once: the snapshot's `generated_at` and the `fetched_at` of every record it writes are that one reading, so the records of one snapshot never disagree about when it was taken (`TestSyncStampsOneInjectedTime`). A value a source does not report stays empty, and `Record.Absent` names it with a reason; nothing is guessed. A record that fails `Record.Validate` is rejected and counted in its source's line, and the rest of the snapshot is still written.

A usage window holds `used_percent`, `window_minutes` and `resets_at`, and two fields a window governor paces by: `burn_percent_per_hour`, how fast `used_percent` rose over the 15 minutes before the read (not the average since the window opened), and `active_agents`, the sessions that drew on the window in those 15 minutes. Both are optional; a burn rate is a finite number of at least zero and an agent count an integer of at least zero, and the schema and `Record.Validate` refuse anything else. A measured zero is written as `0`; a field the source cannot measure is left out and named in `Absent` (`usage_window.burn_percent_per_hour`, `usage_window.active_agents`). Both fields were added in `schema_version` 1 without a version change, so a reader that validates against the v0.1.0 schema refuses a snapshot that carries them until it takes the new schema.

## Sources

| Source | How | Kind | Bounds | Failure and edge cases |
| --- | --- | --- | --- | --- |
| `codex-local` | Reads `rate_limits.primary` from the newest `~/.codex/sessions/**/*.jsonl`, read-only. The burn rate and active agents come from every session file written in the last 15 minutes: the samples of the same window (`limit_id` and `resets_at`) with a `timestamp` in [now − 15 min, now] | measured | 20,000 files walked, 128 MiB per file, 500,000 lines, 4 MiB per line; at most 64 session files read for the burn rate; one record | no logs or no rate limits: skip; an empty `limit_id`: fail. Burn rate absent with fewer than two samples at distinct times, or when `used_percent` fell; both fields absent past 64 recent files; a recent file that cannot be read, or a sample line without a valid `timestamp`: both fields absent, status degraded |
| `claude-local` | Reads the assistant lines of `~/.claude/projects/**/*.jsonl` written in the last 7 days, read-only; only the `usage` token counts and `quotaLimits` (`rateLimitType`, `resetsAt`) are decoded, never message content or credentials. The logs carry no usage percentage, so `used_percent` is derived: the cost-equivalent of the usage inside the window (first-party API rates in `internal/sources/claudelocal/prices.go`) divided by the ceiling, the smallest cost-equivalent a window held at a rejected request. The ceiling is kept in `<user state folder>/epitrop/claude-ceilings/<account>.json`. The burn rate uses the usage of the last 15 minutes; active agents count the sessions and subagents with a usage line in those 15 minutes | measured | 50,000 files walked, 10,000 files read, 256 MiB per file, 1,000,000 lines, 8 MiB per line; 1,000,000 usage entries and 10,000 rejections; one record for `five_hour` and `seven_day` each plus one per other window a rejection names, at most 64 | no logs: skip; a corrupt ceiling store, or rejections naming more than 64 windows: fail. Without a ceiling (no rejection seen, or its rejection older than `--claude-ceiling-max-age`, default 30 days) `used_percent` and the burn rate are absent with that reason, `resets_at` comes from the newest rejection or is absent, and active agents stay measured. A model without rates in the window makes `used_percent` absent; an unknown window type makes the whole usage window absent |
| `litellm-gateway` | `GET <base>/v1/models` with a bearer token | measured | 15 s, 8 MiB, 5,000 models | an empty model list: fail; an entry without an id is rejected and counted; a list where no entry has an id: fail. Prices and context are absent, because `/model/info` is not readable with an agent token |
| `litellm-key-budget` | `GET <base>/key/info` with the same bearer token and no `key` parameter, so the gateway answers for the calling key and the token never enters a URL | measured | 15 s, 1 MiB; one record, `litellm-key[/<alias>]` | a key whose `status` is not `active`, or that is `blocked`: fail; a response without `info` or `spend`: fail. The used share is `spend / max_budget × 100`; without a positive `max_budget` it is absent. The window length is absent for a calendar-month (`mo`) or sub-minute `budget_duration`, and the reset time is absent without a time zone. The burn rate and active agents are always absent: key info reports the spend, not its rate, and does not name the callers of the key |
| `ollama-local` | `GET /api/tags` for installed models, `GET /api/ps` for loaded ones | measured | 5 s, 8 MiB, 5,000 models | no installed models: skip (a real local state); `/api/ps` unavailable: records kept, loaded state absent, status degraded |
| `public-catalog` | OpenRouter's public models API and LiteLLM's price map | declared | 20 s and 16 MiB per sub-fetch, 4,999 records each | an empty list or map: that sub-fetch fails; one sub-fetch failing: degraded; a malformed entry is counted (`malformed=N`, `rejected=N`); an OpenRouter `-1` price is absent as `variable price`, never negative |

The two public catalogs use incompatible model-id schemes (for example `openai/gpt-4` and `gpt-4`). The sync does not reconcile them; each record names its sub-feed in its provenance (`public-catalog:openrouter` or `public-catalog:litellm-prices`).

Every parser is checked against a schema fragment taken from the upstream's published document at a pinned revision (`internal/sources/upstream-schemas.json`; `make schemas-check`). Renovate moves the pins together in one pull request a week (`renovate.json`, group `upstream schema pins`). That pull request needs one `make schemas-repin` commit, and it merges once each pinned commit is at least three days old. Renovate cannot hold these back by age, because git refs carry no release timestamp. A rename or retype upstream fails a parser test instead of dropping a field.

One source is not implemented, on purpose: a Gemini quota source (no readable local store). It is not scraped from an undocumented endpoint.

## Headroom: stale is unknown, never zero

`epitropctl headroom --max-age=<duration> [--in=file]` reads a snapshot through `catalog.ReadHeadroom` and prints one JSON reading per record that has a usage window. A reading is `known` and carries `used_percent` only when all of these hold; otherwise it is `unknown`, carries no `used_percent` at all, and its `reason` names the first rule it broke:

- the source reported a used share, and it is a finite number of at least zero (above 100 is a real overrun and stays known);
- it was fetched no later than now, and at most `--max-age` ago (exactly `--max-age` is still known);
- its window has not reset since (`resets_at` later than now).

`--max-age` has no default: each consumer states how old a reading it accepts. A consumer reads `unknown` as "no data", never as zero usage, so a dead or stale source cannot open a quota that may already be spent. Wiring the readings into routing and dispatch is a later step; this command and `catalog.ReadHeadroom` are the read side.

## Verification notes

The sources were first verified live on 2026-09-18, before their parsers were written:

- a real Codex session log showed the `rate_limits.primary` shape the parser reads;
- a LiteLLM gateway returned HTTP 200 for `/v1/models` and 403 for `/model/info` with an agent token;
- a local Ollama daemon answered both endpoints;
- both public catalog URLs returned real bodies.

The pinned schema fragments record the revision and full-document digest each parser was last checked against (2026-10-09; `litellm-key-info` 2026-10-17). The `litellm-key-budget` parser follows the `/key/info` docstring at the pinned LiteLLM revision and the fixture in `internal/sources/litellmgateway/testdata/fixtures/key-info-response.json`; it has not yet been run against a live gateway.

## Out of scope

Routing, scheduling or repeating a sync, scraping undocumented endpoints, and persistence beyond the snapshot file are not part of the sync. A snapshot is a point-in-time file that an operator or a later routing layer reads.
