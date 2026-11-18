# MCP server

```bash
epitrop-server --config=file
```

`epitrop-server` serves the run record over the Model Context Protocol on stdin and stdout, for a client that starts it as a subprocess. By default its tools only read, and no tool appends to the event log or signals a job:

- `jobs_status` lists the configured jobs as `epitropctl jobs status` does, including `stale` and `last_beat` for a job that declares `heartbeat.max_age_seconds`. On a platform without the supervisor (anything but Linux) the tool returns the reason as a tool error.
- `units_list` lists the work units of the run record, sorted by id, optionally narrowed to one status.
- `unit_status` shows one unit as `epitropctl units show` does, with the count of unread notes in place of the notes. Reading does not mark a note read.
- `runs_list` lists the slot runs, one per unit whose evidence pointer names a run's start file (`started`) or report (`ended`), sorted by unit id, optionally narrowed to one phase.
- `run_status` shows one unit's slot run: the start record while it runs, the runner's report once it has ended. A report whose digest does not match the unit's evidence pointer is refused.
- `watches_list` lists the release watch routes of `release_watch.routes` with their sources and sink kinds.
- `watch_status` shows one route: per source, how many releases the seen-set holds and the one seen last, and its sinks. An ntfy sink shows only its kind, since its topic is all a reader needs to read the notifications.

With `server.transport: http` in the config, the server serves the same tools over streamable HTTP on `server.listen` (default `127.0.0.1:8899`) and prints the address it serves. The server has no authentication, so the config refuses a listen address whose host is not loopback, and the server refuses a request whose `Host` header names another host. Beside the tools at `/mcp` it serves:

- `/livez`, `/readyz` and `/startupz`. `/readyz` answers 503 until the event log has replayed, and again whenever a replay fails; the server replays every two seconds.
- `/events`, a server-sent event stream with one event per signed event-log record (`id` = record sequence number decimal, `event` = record type, `data` = record JSON). A client can resume the stream with a `Last-Event-ID` header; an empty header streams live events from the current tail position, an unstarted tail or missing events source answers 503, a malformed header or a cursor ahead of the tail answers 400, and a cursor below the base of a compacted log answers 410. A stalled client is dropped after a 10-second write deadline, and a client whose queue fills holds back the tail for other subscribers until dropped.

With `server.allow_mutations: true` the server also serves three tools that change state:

- `jobs_start` starts one configured job and leaves a running job alone; `jobs_stop` stops one with its stop signal, the grace period and then a kill. The supervisor starts the job shim as `epitrop-server` itself.
- `unit_set` records a unit's status, under the same transition rules as `epitropctl units record`.

Each call names a `key` for the request. The effect ledger runs a key with the same arguments at most once: a repeat answers `skipped` and changes nothing, and the same key with other arguments is a new request. A call that failed leaves its key open, and a repeat is refused until the claim is resolved with `epitropctl effects resolve`.

The tool names and config keys are fixed; the HTTP paths stay provisional until the first release that ships the server.
