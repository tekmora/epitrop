# ADR-0006: Reach declared destinations from a sandbox without a network through a proxy socket

- **Status**: Accepted
- **Date**: 2026-10-11, accepted 2026-10-18
- **Authors**: Epitrop maintainers

## Context

[ADR-0005](0005-sandboxed-job-runner.md) gives a sandboxed job one of two network modes. `none` has no network. `egress` reaches the whole internet: the record lists the missing per-destination allowlist as a negative consequence, and the release notes of 0.1.0 state it as a known gap. A headless agent slot needs one thing from the network, its model gateway. Run with `egress`, a harness that follows an instruction planted in its input can send the workspace anywhere. No real unit runs in a slot before that is closed (see References).

A filter inside the sandbox's network namespace would be a rule the job's side of the boundary carries. The rule has to sit where the job cannot change it, and a job that needs one destination should not get a network interface at all.

## Decision

1. **A third network mode, `network: proxy`, with a list of destinations.** `sandbox.destinations` holds 1 to 16 values of the form `host:port`. Each is written in its one spelling: a name in lower case without a trailing dot, an address as Go's `netip` prints it, an IPv6 address in brackets. A value in another spelling, a duplicate, a port outside 1 to 65535 and a list on a job whose network is not `proxy` are refused at config load, and again by the shim before it starts anything. `job.started` records the applied mode as `enforce/proxy`.
2. **The sandbox has no network.** The argv is the one of `network: none` in ADR-0005: no `--share-net`, no pasta, no resolver file. Five things are added:

   ```text
   bwrap ... --ro-bind <socket> /run/epitrop/proxy.sock --ro-bind <epitrop executable> /run/epitrop/relay
       --setenv HTTPS_PROXY http://127.0.0.1:3128 --setenv https_proxy http://127.0.0.1:3128
       --setenv HTTP_PROXY http://127.0.0.1:3128 --setenv http_proxy http://127.0.0.1:3128
       --setenv GOMAXPROCS 2
       -- /run/epitrop/relay __egress-relay --socket /run/epitrop/proxy.sock --listen 127.0.0.1:3128
       -- env --default-signal=TERM --unset=GOMAXPROCS <command>...
   ```

   | Part | Why |
   | --- | --- |
   | the socket, bound read-only | the only way out of the namespace. A Unix socket crosses a network namespace because it is a file; connecting to it needs no write access to the mount. |
   | the relay | the job's programs speak to a proxy over TCP, not over a Unix socket. The relay listens on the sandbox's own loopback and copies each connection to the socket. It checks nothing and holds no rule. |
   | the Epitrop executable as the relay | one program, already on the host in the version the shim runs; no second binary to build, ship and keep in step. It is bound read-only, so the path rule of ADR-0005 (a job must not be able to write the executable) holds. |
   | the four proxy variables | the common spellings. A job cannot set them itself: `env_allow` refuses `HTTP_PROXY`, `HTTPS_PROXY`, `ALL_PROXY` and `NO_PROXY` for this mode. |
   | `GOMAXPROCS` for the relay, removed again for the command | the relay's threads count against the job's task limit (item 5). The Go runtime sizes itself from the host's processors before the relay's first line runs, so only the environment bounds it. The command starts without the variable, and `env_allow` refuses it for this mode. |

3. **The proxy runs in the shim, outside the sandbox.** For a job with `network: proxy` the shim starts the proxy before the job and stops it after the job has ended. It listens on a Unix socket in a directory only the user can enter and serves one request per connection, `CONNECT host:port`. It opens the tunnel only when `host:port`, brought to its one spelling, equals a declared destination. A proxy that cannot start refuses the job (`job.refused`); there is no start without it.

   | Request | Answer |
   | --- | --- |
   | `CONNECT` to a declared destination that is reached | `200`, then the tunnel |
   | a target that is not declared, or not a `host:port` | `403` |
   | any method but `CONNECT` | `405` |
   | a request line of another form, or a request that ends before its end | `400` |
   | more than 64 header lines or 8 KiB before the tunnel | `431` |
   | a declared destination that is not reached, or a name whose addresses are all left out (item 4) | `502` |
   | a 65th tunnel at the same time | `503` |

   Each refusal writes one line to the job's log, up to 128 lines, and the shim adds the number of refused requests when the job ends. A line stays one line of at most 1024 characters, whatever the request and the error hold.
4. **A name does not lead to the host.** The proxy resolves a declared name outside the sandbox and leaves out every loopback, link-local, multicast and unspecified address among the answers; it tries at most 8 addresses. A job that controls a name's DNS answer can therefore not point a declared name at a service on the host the control plane runs on. An address is reached as written, so a model gateway on the host's loopback is declared as `127.0.0.1:<port>`, by whoever writes the config.
5. **The relay does not change how a job stops or what it reports.** The relay starts with SIGTERM ignored and gives the job the default action back, as bwrap and pasta do in ADR-0005, so a graceful stop reaches the job. The relay's exit code is the job's; 128 plus the signal's number when a signal ended the job; 125 when the relay itself failed. A tunnel the other side ended writes nothing to the job's output; a relay fault writes one line. The relay counts against the job's task limit, so `tasks_max` for this mode must be at least 32: the 16 any job needs and 16 for the relay (item 7).
6. **RT-03 fixtures prove it in both directions.** They extend the network row of ADR-0005's table and are replayed by `TestRT03Fixtures` against the real sandbox:

   | Boundary | Refused (violation) | Allowed (compliant) |
   | --- | --- | --- |
   | network, `proxy` | through the proxy to a destination that is not declared; a direct connection to the listener's address, past the proxy; a direct connection to host 127.0.0.1; through the proxy to a declared name that resolves to loopback | through the proxy to the declared destination |

   Every rule of this record was removed once, in a copy, to show a test failing.
7. **Every limit has a reason.** A number marked *choice* is not derived: it is small, bounds a loop or a buffer, and is the first thing to measure again when a real harness runs in this mode.

   | Limit | Value | Reason |
   | --- | --- | --- |
   | destinations of a job | 16 | A slot job needs one, its gateway. The list is what a review reads, and every request is compared against it. *Choice.* |
   | name, label, port | 253, 63, 1 to 65535 | What a DNS name can be (`hostname(7)`), and the range of a TCP port. |
   | tunnels at a time | 64 | An open tunnel holds two connections and the goroutines that copy between them in the shim, and the same in the relay. One more is answered `503` at once and can come again. *Choice.* |
   | connections in a job's life | 2^25 | The bound of the accept loop. It is above one connection a second for the longest runtime a job may have, 365 days (`TestProxyConnectionBoundCoversEveryJobRuntime`). At the bound the listener is closed: a further connection is refused at once, and the job's log gets one line. |
   | request before the tunnel | 64 header lines, 8 KiB | The proxy uses the request line only. 8 KiB is the read buffer of one connection, so 64 unfinished requests hold 512 KiB in the shim. |
   | time for the request | 10 s | The client is on the same host. One that sends nothing holds one of the 64 places no longer. |
   | time to reach a destination | 10 s | For the lookup and every address together; then `502`. *Choice.* |
   | addresses tried for a name | 8 | Tried in turn within those 10 s. *Choice.* |
   | life of a tunnel | the job's deadline | The shim always runs the proxy under the job's deadline, the longest runtime when the job sets none. 24 hours apply only to a caller without a deadline. |
   | lines in the job's log | 128, each at most 1024 characters, the target cut at 128 | The shim's lines stand among the job's own output. A job that asks for many targets, or for a long one, does not bury them; the number of refused requests at the end is complete. |
   | faults kept | 8 | Errors that belong to no request, such as a failed accept. The first ones say what went wrong; further ones are dropped. |
   | wait for the proxy to stop | 10 s | The job and its sandbox are gone by then, so only tunnels that are closing remain. |
   | listener in the sandbox | `127.0.0.1:3128` | The sandbox's own loopback in its own network namespace, so the same for every job. The port is a *choice*. |
   | processors and tasks of the relay | `GOMAXPROCS=2`, 16 tasks | Measured with go1.27.2, 64 tunnels carrying data: 5 to 7 threads on 1 to 32 processors with the variable; without it the number grows with the processors, 6 to 11 on 16 and 13 to 14 on 32. 16 is the 7 measured and room for threads the runtime adds while others wait in a system call (`TestRelayThreadCount`). The scope's task limit enforces the sum of relay and job. |
   | socket path | 100 bytes | A Unix socket address holds 108 (`unix(7)`). |
   | exit code when the relay failed | 125 | As `env(1)` and `timeout(1)` report their own failure. |
   | life of the relay | 366 days | Above the longest runtime of a job, so the supervisor's deadline always comes first (`TestRelayLifeIsAboveEveryJobRuntime`). |

## What this record decides

1. A mode beside `none` and `egress`, not a filter added to `egress` (item 1). `egress` stays as it is.
2. The rule sits in a process outside the sandbox, and the sandbox gets a socket and no interface (items 2 and 3).
3. `CONNECT` only. The proxy sees a host and a port and passes bytes; it does not end TLS and filters no path (item 3).
4. Destinations are exact `host:port` values in one spelling: no wildcard, no port range, no address range (item 1).
5. The address rule for names, and that an address literal is the way to declare a destination on loopback (item 4).
6. The Epitrop executable is the relay (item 2).
7. The fixed bounds and the reason of each (item 7). The ones marked as a choice are open to a measurement.

## References

Decisions made before this record, by the maintainer. The wording is paraphrased.

- `tribunus-slot-egress`, 2026-10-11, network of a slot job: the first slot run on a throwaway unit may use `egress`, recorded as a known gap; a sandbox without a network and with a gateway socket is built as its own unit before any real unit runs in a slot (paraphrased).
- `tribunus-slot-harness-auth`, 2026-10-11, what a harness gets: the gateway is a network address, and credentials reach a job only as read-only files or allowed variables (paraphrased).
- `epitrop-0-1-0`, 2026-10-11, the release: 0.1.0 states the open egress of slot jobs plainly, and this unit ships after it (paraphrased).

## Consequences

- **Positive**: A job reaches what its config names and nothing else, and has no network interface a mistake could widen. The destinations of a job are in its config, where a review reads them. The network part of ADR-0005's "no per-destination allowlist yet" is closed for jobs that use this mode.
- **Negative**: Only programs that use a proxy from the environment work: a program that resolves a name itself before it connects finds no resolver, and UDP has no way out. Which harness works in this mode is measured with the slot unit that follows, not here. The proxy sees no content, so a declared destination that accepts uploads is a way out for data; the list is the control. Plain HTTP through the proxy is refused (`405`); a destination has to be reached with `CONNECT`, in practice HTTPS. Names are resolved with the host's resolver at every tunnel, with no pinning between tunnels. `rt03.violation` events still wait for the broker: a refusal is a line in the job's log and a count. Linux only, as ADR-0005.
