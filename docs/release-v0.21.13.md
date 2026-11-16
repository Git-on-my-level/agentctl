# v0.21.13

agentctl no longer reads the Multica workspace event stream. The fork disabled
event capture on 2026-09-13 (migration `9015`) to stop write deadlocks, and
ruling R24 (2026-11-16) retired the stream instead of restoring it, so its pages
were always empty.

- `await` and the supervisor refresh an issue-bound Multica execution from
  `multica issue get`. That now includes rows recorded before the issue snapshot
  route existed, which previously polled `event list` and never advanced.
- An unavailable issue read leaves the execution unreachable, and a failed
  `issue get` is a retryable error, as for every Multica row; neither falls back
  to events.
- The Multica adapter answers `Events`/`EventsPage` with a typed
  `capability_unavailable` error without launching the CLI, and `MulticaArgv`
  no longer builds `event list` or `event watch`.
- `inbox` advertises the bound-issue refresh route for any issue-bound row
  without a run binding, matching what the runtime now does.

New dispatches and promotions already used `issue get` since v0.10.1; their
behavior is unchanged.
