# Design: link agentctl runs to ANX cards with durable IDs

Status: draft (2026-10-24). No implementation yet.

## Problem

Agents keep losing other agents' work:

- A card cannot answer "which runs worked on me, what did they conclude, and
  where is the full trail?" Today `card:watch-redesign` in the `anx` workspace
  reports `executions: []` while the local agentctl journal on m5-mbp alone
  holds ten executions labeled `anx.card.watch-redesign` from the last two
  days. The `anx runs ingest` bridge exists but is not wired on this fleet, and
  even when wired it carries only state, labels, adapter, and times.
- Takeovers need hand-written briefs pointing at state files on the origin
  host. If that host is offline or its journal is gone, the trail is gone.
- GLM (ZCode) runs cannot be resumed, so every follow-up starts cold; the next
  agent cannot even read what the previous one did.
- Audits have to reconstruct which agent did what from scattered journals.

## Goals

- A card answers: which runs worked on it, what each concluded, and where the
  full transcript trail is — with enough durable coordinates to fetch the
  transcript even when the origin host is offline or has deleted it.
- The durable run ID resolves in three places: agentctl on the origin host,
  the fleet transcript backup, and ANX's run record.
- ANX stays generic and runs no models. agentctl is one provider of ANX's
  existing public sessions/participation/runs surface; any harness could do
  the same.
- Raw transcripts never enter ANX. ANX stores links plus a size-capped,
  redacted, deterministic outcome.
- Registration is idempotent, at-least-once, and never blocks or fails a run.

## Non-goals

- No transcript storage, search, or summarization inside ANX or agentctl.
- No new issue board, conversation store, or credentials DB (repository rules).
- No automatic acceptance: a registered run outcome is evidence, never proof
  that the card's task was accepted. Task authority stays with the card and
  execution authority with the native CLI / Multica.
- No resume capability for harnesses that lack it (see
  [Follow-up turns](../follow-up-turns.md)); this design only makes prior work
  *findable*, so carry-forward and takeovers start warm.
- No change to the backup's transport (rsync over Tailnet SSH to pve3).

## The existing transcript backup (found)

**Location.** Each fleet Mac runs a daily Hermes cron job
(`session-archive-daily`, job `f4a23bc04b64`, 05:25) that executes
`~/.hermes/session-archive/run-archive.sh`:

1. `collector.py` — read-only incremental collect from harness session stores
   into `~/.hermes/session-archive/` (staging, per-host object trees, and an
   append-only manifest segment `manifests/<host>/<runts>.ndjson.pending`).
2. `rsync -a` the whole local archive root to
   `pve3:/mnt/hdd12t/agent-session-archive/` (Tailnet SSH alias from
   `target.conf`; three retries; receiver mount-guarded to `/dev/sda`).
3. `verify_and_publish.py` — per object: remote `sha256sum` plus `zstd -t`;
   on success the segment is renamed `.pending` → committed on both sides
   (atomic receipt). A monthly `--scrub` audits all committed objects.

**Layout.** Immutable, content-revisioned objects:

```text
<host>/<harness>/<yyyy>/<session-id>/<base>.<sha10>.jsonl.zst
manifests/<host>/<runts>.ndjson          # one row per archived session revision
```

A changed session is archived as a new `<sha10>` revision; old revisions are
retained and each manifest row names the exact object
(`archive_relpath`, `archive_sha256`, `archive_bytes`, `raw_content_sha256`,
`first_ts`, `last_ts`, `label`, `completeness`). Manifests carry no prompt
excerpts; permissions are 0700/0600.

**What it backs up, and how it is keyed.**

| Harness | Source store | Archive session key |
| --- | --- | --- |
| Claude Code | `~/.claude/projects/**/*.jsonl` (recursive; includes subagent transcripts) | Claude session UUID (the `.jsonl` filename) |
| Codex | `~/.codex/sessions/**/rollout-*.jsonl` and `~/.codex/archived_sessions/**/*.jsonl.zst` | rollout filename minus `rollout-` prefix and suffix; the native thread UUID is the trailing 36 characters |
| Hermes | `~/.hermes/state.db` (deterministic sqlite dump of all message rows) | Hermes session id |

Not covered today: ZCode (`~/.zcode/cli/exec/sess_*`,
`~/.zcode/cli/agents/sess_*`, plus `db/db.sqlite`), Cursor, Devin, OMP,
OpenClaw, and generic processes. The host key is `hostname -s`
(e.g. `Davids-MacBook-Pro`), *not* a stable fleet host ID.

**Retention.** Nothing is deleted. Each row carries a `label`
(`user` / `agent` / `unknown`, with evidence) that exists as groundwork for a
future deletion policy; `unknown` is never auto-eligible. Stale `.pending`
segments older than one day are dropped and re-collected on the next run.

**Verified on m5-mbp (manifest segments, `last-verify.json`, cron outputs).
Not verified:** the pve3 remote tree (no ssh from this design pass) and which
other fleet hosts run the job.

## Data model

### Durable run ID

The canonical key is the existing portable pair, not a new mint:

```text
<origin_host_id>/<exec-id>          # e.g. host-goose-addict-spray-hawk-lens-horror/exec-...
agentctl://<origin_host_id>/<exec-id>   # the portable URI form
```

Both halves are typed agentctl word IDs. Word IDs are host-local, so the pair
— already defined by [Identifiers](../identifiers.md) as the globally
unambiguous reference — is the durable ID. ANX stores the two halves as
separate fields (`external_id`, `origin_host_id`) rather than a concatenated
string, so its own upsert identity `(launcher, host_id, external_id)` keeps
working; `host_id` there remains the *ANX-enrolled* host while
`origin_host_id` carries the agentctl journal host.

### Native transcript key

Transcripts are keyed by native session, not execution, because one native
session spans follow-up turns and may pre/post-date any single execution. The
key is the `agentctl identity` v1 correlator, which ANX's sessions API already
types as `native_session_id_kind: provider_session_sha256`:

```text
sha256("agentctl.identity.v1\0" || "native_session\0" || provider || 0x00 || native_session_id)
```

`provider` uses the identity-report canonical provider id (`claude`, `codex`,
`hermes`, `zcode`); the backup index maps its harness names onto the same ids.
The raw native session id never leaves the origin host and the backup.

### Backup index (new, small)

The manifests already contain everything needed except the fingerprint and a
queryable shape. An index builder (run after segment commit, incremental over
new segments) emits one append-only NDJSON per host:

```json
{"schema_version": 1, "host": "Davids-MacBook-Pro", "provider": "codex",
 "native_fingerprint": "sha256:<64 hex>", "session_id": "<raw, stays in backup>",
 "archive_relpath": "Davids-MacBook-Pro/codex/2026/<sid>/<sid>.<sha10>.jsonl.zst",
 "archive_sha256": "sha256:<64 hex>", "archive_bytes": 12345,
 "first_ts": "...", "last_ts": "...", "label": "agent", "completeness": "complete"}
```

at `index/<host>.ndjson` beside `manifests/`, plus a
`session-archive-find <fingerprint>` helper (bounded grep over `index/`,
prints the newest row per fingerprint with object digest). This is the
migration for existing backups: a one-time full build over committed segments
plus incremental appends; no object rewrite, no manifest rewrite. Collector
v2.1 additionally emits `native_fingerprint` (and, when a local agentctl
journal exists, `origin_host_id` + `exec_id` hints) inline in new manifest
rows, so future index builds need no re-derivation.

### ANX run record (extension of the existing runs roster)

The `POST /runs` upsert (idempotent on `(launcher, host_id, external_id)`,
state-regression-protected, replay-detecting) gains these optional fields:

```json
{
  "launcher": "agentctl",
  "external_id": "exec-calm-pen-slide-adjust-steak-antique",
  "origin_host_id": "host-goose-addict-spray-hawk-lens-horror",
  "request_key": "watch-redesign-lane-a-1",
  "adapter": "claude-code", "model": "claude-opus-5-5",
  "state": "completed", "liveness": "exited",
  "labels": ["anx.card.watch-redesign"],
  "started_at": "2026-10-24T06:24:22Z", "ended_at": "2026-10-24T06:49:51Z",
  "card_ref": "card:watch-redesign",
  "outcome": {
    "availability": "stored",
    "failure": null,
    "summary": "Redacted final answer, at most 2048 UTF-8 bytes (see Redaction)",
    "links": [{"kind": "pr", "ref": "https://github.com/.../pull/104"},
              {"kind": "commit", "ref": "sha256-pinned git sha"}],
    "head_after": "<40-hex git HEAD in the launch cwd at terminal, or null>"
  },
  "native_session": {"provider": "claude", "kind": "provider_session_sha256",
                      "fingerprint": "sha256:<64 hex>"},
  "transcript": {"archive_host": "pve3", "status": "indexed",
                 "fingerprint": "sha256:<64 hex>", "harness": "claude-code",
                 "archive_sha256": "sha256:<64 hex>", "last_ts": "..."},
  "supersedes_external_id": null
}
```

Bounds: `summary` ≤ 2048 bytes; `links` ≤ 8 entries, each ≤ 512 bytes;
unknown fields rejected (existing behavior). `native_session` and
`transcript` are principal-scoped: they are omitted from card-shared
projections and task reads (which already never share native session
identifiers) and shown only in the runs roster to callers allowed to see runs.
`transcript.status` is `pending` (run live, backup runs daily),
`indexed` (index row verified), `not_archived` (harness the collector does
not cover), or `unknown`. Cards get a composed read-only view — `anx work
context <card>` gains a `runs` section and `anx runs list` gains
`--card`/`--state`/`--origin-host` filters — both assemble existing data; the
only core change is the new run fields. Card `executions` entries are written
from registered terminal runs as `{authority: "agentctl", run_id:
"agentctl://<origin_host_id>/<exec-id>"}` (the existing `authority` +
`run_id` shape, ≤ 200 items).

Sessions and participation stay as defined: the registering helper maintains
one ANX session per native session (`anx sessions register`, monotonic
`sequence`, `native_session_id_kind: provider_session_sha256`) and refreshes
`anx work participants register <card>` with `activity` mapped from run
state (`running` → `active`, terminal → `idle`, superseded → `left`). A
session heartbeat never refreshes participation and vice versa; unchanged.

### Where each fact lives

| Fact | Owner |
| --- | --- |
| Execution envelope, result body, native session binding | agentctl journal (origin host) |
| Transcript bytes, revisions, manifest, index | the pve3 backup (and local mirror) |
| Run identity, outcome summary, links, transcript locator, card link | ANX run record + card `executions` |
| Card state, acceptance, evidence | ANX card (existing authorities) |

agentctl remains a supervision layer: it adds no ANX credential handling and
stores nothing new beyond what the journal already records.

## Card resolution

Precedence, deterministic, first match wins:

1. **Explicit override:** `--anx-card card:<slug>` on `delegate`/`run`
   (sugar for adding the label; the label remains the recorded contract).
2. **Execution labels:** every `anx.card.<slug>` label on the execution.
   Multiple labels register the run against each card. A slug that does not
   resolve (`not_found`) parks that registration with reason
   `card_unresolved` in the local spool and surfaces it in
   `anx runs status`; it never fails or blocks the run.
3. **Directory/session bindings:** `anx integrations map <directory>
   --card-id card:<ref>` and `anx integrations bind --session-id ...`,
   resolved by the ANX-side helper against the launch cwd (longest-directory
   rule), only when no label or override exists.
4. **None:** no registration, unless the operator enabled
   `run_links.register_unlabeled` (default off).

Labels stay the operator-facing, typed, LLM-friendly contract; directory
bindings are fallback convenience. Nothing here authorizes mutation: card
links are metadata.

## Flows

### Start

`delegate` journals the execution (existing). When a card resolves and
`run_links` is enabled, the ANX-side helper (see Opt-in) registers the run:
state `running`, harness, host, durable ID halves, `request_key` (the agentctl
delegate request key — the stable key for retries), and — after the native
session id is recorded — `anx sessions register` for the native session.
Registration happens off the critical path: launch never waits on ANX.

### Finish

At terminal the helper reads the local journal (`agentctl status`/`result`,
exactly as `anx runs ingest` does today) and upserts the terminal record:
state, times, failure projection, redacted summary, links, `head_after`,
`native_session.fingerprint`, `supersedes_external_id`, and — if the backup
index is reachable — `transcript`. Because the backup runs daily, the
transcript locator is normally confirmed the next day by a reconcile pass
that re-checks the index for fingerprints still `pending`; `status` stays
honest (`pending`, not guessed) until then.

### Follow-up turn

`continue` creates a new execution that supersedes the old one on the same
native session. The helper registers it as a new run record with
`supersedes_external_id` set; the native session fingerprint is identical, so
ANX's session record and the backup index already converge the lineage. A
carry-forward round registers the same way with
`native_session: null` (fresh session) and the prior run's ID in
`supersedes_external_id` — the trail shows a warm start without claiming
conversation continuity.

### Takeover

An agent picking up a card: `anx work context <card>` (runs section) or
`anx runs list --card card:<slug>` → newest non-superseded run → read its
outcome summary and links; if it needs the full trail, see Fetch. The
`agentctl://` run ID tells it where the authoritative journal lives; when the
origin host is reachable, `agentctl status|result` there gives the envelope
and stored answer directly.

### Fetch

Order of attempts, each logged:

1. **Origin host via agentctl** (when reachable): `agentctl result <exec-id>`
   for the stored answer; the native session binding points at the local
   transcript for the harness's own tools.
2. **Backup:** resolve `native_session.fingerprint` through
   `session-archive-find` on the archive host (or over the rsynced `index/`),
   fetch `archive_relpath`, verify `sha256`, decompress. No origin-host
   access required.

Every fetch appends one line (principal, fingerprint, digest, relpath, time)
to `fetches/<host>.ndjson` on the archive host. Fetches never mutate ANX and
never mark work collected; acknowledgement semantics stay with `result`.

## Redaction

The summary is derived deterministically from the stored result body (which
already excludes prompts, reasoning, and tool chatter). No model runs. Rules,
applied in order, then truncated to 2048 UTF-8 bytes with a trailing `…[truncated]`
marker:

- Drop any line matching credential shapes: `AKIA…`, `sk-…`, `ghp_…`,
  `xox[bp]-…`, `-----BEGIN … PRIVATE KEY-----`, `Bearer …`, Google
  `ya29.…`/AIza keys, high-entropy assignments to names containing
  `token|key|secret|password|credential`.
- Drop `ENV=VALUE` assignments wholesale (values never negotiated).
- Replace absolute local paths (`/Users/…`, `/Volumes/…`, `~/…`, `/home/…`)
  and Tailnet hostnames/IPs with `<path>` / `<host>`.
- Strip URL query strings (they may carry tokens); keep scheme, host, path.
- If any rule match is ambiguous (partial overlap with a credential shape),
  omit the summary entirely with
  `summary_omitted_reason: "redaction_failed"` — never ship a maybe-leak.

Links pass the same filter plus scheme/host allowlists (`https` git hosts,
`pr:`/`commit:` refs). `head_after` is a bare 40-hex sha. These rules ship as
a published fixture-tested function in the ANX CLI, not prose.

## Access

Transcripts are operator-private. Reading them rides the *existing* access
model, never card visibility:

- Origin-host fetch requires an owner shell on that host (journal is
  owner-only; agentctl never publishes a remote transcript endpoint).
- Backup fetch requires the operator's pve3 account over Tailnet SSH — the
  same grant the daily rsync uses. No new credential store, no presigned URLs,
  no ANX-issued capability. "Can see the card" grants exactly the compact
  outcome, never bytes.
- ANX-side: run records are visible to workspace members through the runs
  roster; `native_session` and `transcript` fields are principal-scoped and
  omitted from card/task projections shared beyond the principal.
- Every backup fetch is logged (above); agentctl reads remain covered by the
  existing owner-only journal.

## Idempotency and failure

- **Stable keys:** the registration attempt is keyed by the agentctl delegate
  request key plus execution ID; ANX's run upsert dedupes on
  `(launcher, host_id, external_id)` and rejects state regression. Replays
  are safe and detected (`replayed: true`).
- **At-least-once:** registrations may duplicate; consumers dedupe. Terminal
  upserts refresh — never regress — state.
- **ANX unreachable:** the run is unaffected. Registrations spool in the
  helper's bounded local outbox (the same spool `anx integrations` already
  uses: retry with backoff, dead-letter with reason) and flush on the next
  attempt or bridge tick. Unflushed entries older than a configurable bound
  surface in `anx runs status` — they never block launches, continues, or
  results.
- **Backup unreachable:** `transcript.status` stays `pending`; the outcome
  still registers.
- **Origin host lost before flush:** the fingerprint is missing from ANX. The
  daily backup still captured the transcript; the optional journal-export
  step (Rollout, step 6) makes the backup self-sufficient for
  exec→session resolution, and a later reconcile backfills ANX.
- **Unknown card / label typos:** parked as `card_unresolved`, surfaced, never
  fatal.
- **Multica mode:** run links register Multica-authority executions
  (lifecycle state, card link) but `outcome.availability` is
  `unavailable_at_source` (the adapter has no result-content route) and
  `transcript.status` is `not_archived` unless a native worker session is
  bound and covered by the collector. Continuation stays Multica-owned.

## Opt-in and configuration

- **Default:** enabled for executions that carry an `anx.card.*` label —
  zero-config, matches today's documented convention. Unlabeled runs are not
  registered.
- **`agentctl-config`** (reviewed bundle) gains a small advisory section:

```json
"anx": {"run_links": {"default": "labeled", "workspace_alias": "anx",
                       "summary_max_bytes": 2048}}
```

  agentctl itself reads no ANX credentials and makes no ANX calls; the
  section documents fleet intent for the helper and the skill.
- **Enable surface:** `anx integrations enable run.links` (same family and
  spool machinery as `context.inject`), which creates the agentctl
  subscription (kinds `started` + `terminal`, `command` destination →
  `anx runs ingest`) and the reconcile tick. `anx integrations status
  run.links` reports pending spool, dead letters, and unresolved cards.
- **Interaction with `context.inject`:** independent. `context.inject`
  injects card context at session start; `run_links` reports outcomes at
  finish. Both resolve cards with the same precedence, so a directory mapping
  set up for one is honored by the other; there is no ordering dependency.

## Rollout (each step is one agent PR)

1. **Backup collector v2.1** (fleet scripts, not this repo): add ZCode
   (`~/.zcode/cli/exec|agents/sess_*`) with the same stable-capture rules;
   emit `native_fingerprint` (and `origin_host_id`/`exec_id` hints from the
   local agentctl journal) in manifest rows.
2. **Backup index + find helper:** incremental builder after segment commit,
   one-time backfill over existing committed segments (no rewrites),
   `session-archive-find`, fetch log.
3. **ANX core + CLI run fields:** `origin_host_id`, `request_key`, `outcome`,
   `native_session`, `transcript`, `supersedes_external_id` on `POST /runs`
   (nullable columns; no migration), roster filters, principal-scoped
   projection, `work context` runs section, card `executions` writer.
4. **`anx runs ingest` v2:** handle `started` events; build the outcome
   (summary redaction with fixtures, links from the result's evidence block —
   the same `---evidence---` convention the PM runner already parses — plus
   `head_after`); sessions/participation registration; transcript reconcile
   against the index; `run.links` integration + spool.
5. **agentctl (this repo):** docs (this file linked from
   optional-integrations), portable skill update teaching the label
   convention, durable ID, and fetch flow; optional `--anx-card` sugar;
   expose `request_key` and `delegation.resolved.model` in `status --json`
   for non-terminal reads.
6. **Hardening (optional):** archive a daily redacted agentctl journal export
   (exec → labels, request key, fingerprint, state) into the backup tree so
   exec→transcript resolution survives the loss of every origin host.

Steps 1–2 and 3–4 are independent; 5 can land any time after 3. Nothing here
requires changing the backup's transport or retention.

## Worked example: Lane A, 2026-10-24, origin host offline

Lane A (`watch-backend-a`, a GLM lane) ran on a fleet Mac other than m5-mbp,
hit its 360-minute `deadline_at`, and could not be continued (ZCode has no
native resume; carry-forward was available but would have started cold). Its
exec never existed in m5-mbp's journal — verified: `agentctl recent --label
watch-backend-a` returns zero rows here. A takeover agent on m5-mbp needs its
exec, outcome, and transcript from `card:watch-redesign` alone.

**Today (dead end).** `anx context watch-redesign` (workspace `anx`) shows
`executions: []` and `anx runs list --card card:watch-redesign` returns zero
runs — the ten sibling runs on m5-mbp were never registered, and Lane A's
host is unreachable anyway. The takeover agent writes a cold brief from
scratch or waits for a human.

**With this design.** The takeover agent runs:

```bash
anx runs list --card card:watch-redesign --state failed
# → run exec-<lane-a-id>, origin_host_id host-<studio>, adapter zcode,
#   model zai/glm-5.3, state failed, ended_at 12:5xZ,
#   outcome.summary "…backend watcher landed except the reconnect test…",
#   links [pr …, commit …], transcript.status indexed
#   (after rollout step 1; before it, not_archived for zcode — honest)
anx runs get run:<handle>        # full record incl. native_session.fingerprint
```

The summary plus links answer "what did it conclude". For the full trail, the
origin host is offline, so step 1 of Fetch is skipped:

```bash
ssh pve3-tail session-archive-find sha256:<fingerprint>
# → archive_relpath <host>/zcode/2026/<sid>/<sid>.<sha10>.jsonl.zst  sha256:…
ssh pve3-tail "cat /mnt/hdd12t/agent-session-archive/<relpath>" > t.jsonl.zst
shasum -a 256 t.jsonl.zst   # matches archive_sha256; logged by the helper
zstd -dc t.jsonl.zst | less
```

The takeover brief now cites `agentctl://host-<studio>/exec-<lane-a-id>`, the
outcome, and the transcript revision digest — durable across hosts, verifiable,
and reconstructible even after the origin host deletes its local copy. A
carry-forward round registers a new run superseding Lane A, visible on the
same card.

## Decisions (David, 2026-10-24)

1. **Host keying:** the fingerprint index keys rows by agentctl `origin_host_id` in addition to `hostname -s`, because hostnames can collide across Macs. Confirm which hosts run `session-archive-daily` by reading each host's Hermes cron config through hostctl, not ssh.
2. **Roster visibility:** workspace members see the redacted outcome summary and PR/commit links. `native_session` and the transcript locator are visible only to the owning principal and human operators.
3. **Collector order:** add ZCode first (it covers the GLM lanes), then Cursor. Defer Devin. Skip OMP permanently (retired 2026-09-25).
4. **Retention:** archive forever for now. Revisit in about 3 months with measured size. If a bound is adopted later, `label=agent` revisions become deletable only after it (for example 180 days).

## Implementation steps (one PR each)

1. ZCode session collector for `session-archive-daily`. This touches the Hermes and fleet hosts, so it is owned by the Hermes owner, with David's approval.
2. Fingerprint index plus a `session-archive-find` helper over the existing manifests, keyed by `origin_host_id` and hostname.
3. agentctl run registration: outcome schema, redaction, an idempotent upsert keyed by the delegate request key, spooling while ANX is unreachable, and default-on for runs that carry an `anx.card.*` label.
4. ANX: a runs view on `work context` and `runs list --card`, with the visibility rules from decision 2.
