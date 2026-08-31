# Optional integrations

agentctl's standalone execution, journal, result, subscription, and callback
paths do not require the integrations on this page. Each integration remains
authoritative for its own state and must be configured explicitly.

## Multica

Multica can own a durable issue/run/review lifecycle when work needs handoff,
multiple owners, or long-lived review. Direct native execution remains the
default; agentctl never creates a Multica issue from prompt keywords or merely
because Multica is installed.

A complete Multica authority identifies its executable, native profile,
workspace, API origin, and application-link origin:

```bash
agentctl config set-profile \
  --name coordinated \
  --multica-executable /opt/agent-tools/bin/multica \
  --multica-profile example-profile \
  --workspace-id example-workspace \
  --server-url https://coordination.example.com \
  --app-url https://coordination.example.com

agentctl --profile coordinated config doctor
```

Promotion is explicit and plan-capable:

```bash
agentctl --profile coordinated promote exec-... \
  --title "Durable follow-up" \
  --handoff-file handoff.md \
  --plan
```

New routed work can instead be dispatched directly after a live read-only
target check:

```bash
agentctl --profile coordinated dispatch \
  --route "m5 sol" \
  --title "Review the release" \
  --prompt-file task.md \
  --idempotency-key release-review-v1 \
  --plan
```

Remove `--plan` to create or recover the Multica issue and receive a tracked
`exec-*` handle. The route must identify one configured host and concrete model;
the joined Multica runtime must be online and the unarchived agent must be idle
or working. Dispatch passes only the exact resolved assignee ID to Multica,
never a display name, and never falls back to a local native run. The prompt is
sent on Multica stdin and only its digest is journaled.

For replay safety, the remote mutation is a bounded saga: create or recover the
exact assigned issue in `backlog` using Multica's client key, persist its exact
issue binding while the execution is still `starting`, then read that issue and
activate only when it still reports backlog. A replay that observes a later
Multica status skips activation. Multica has no conditional status-update flag,
so a concurrent external status change can still race the activation call and
remains an authority-level concern rather than a false agentctl guarantee.

Agentctl first reserves a local `starting` execution with the exact resolved
assignee and runtime IDs. If the remote response or later local binding is lost,
the same semantic invocation recovers that record before consulting live fleet
state. Once the issue binding exists, replay reads the exact issue rather than
re-running assignment discovery.

Remove `--plan` only after reviewing the remote side effect. Dispatch, promotion,
and durable event observation require a compatible Multica deployment; consult
`capabilities multica` rather than assuming compatibility from the executable
name.

## Network and fleet tools

Tailscale or another network layer may provide authenticated reachability for
operator-managed callback receivers or remote resources. A resource index such
as `tailnetctl`, and a host manager such as `macctl`, may provide aliases or
desired state to an operator workflow.

They are not agentctl dependencies. agentctl does not install them, copy their
credentials, infer network authorization from a host name, or treat network
reachability as permission to mutate a host. The preview does not implement
general remote-host attach or discovery.

## Git knowledge sources

The optional knowledge commands validate and compile explicitly registered
GitHub, Forgejo, or generic Git sources into a verified local bundle. Read-only
context selection never fetches. Authentication remains in native Git or SSH
credential storage, and compiled data is bounded by the source registration's
inclusion and sensitivity policy.

Knowledge repositories remain independently useful and independently
permissioned. They do not need to live beside agentctl or use an
agentctl-specific monorepo layout. See
[Shared context and knowledge](context-and-knowledge.md).

## Managed supervisor

The foreground CLI is sufficient for synchronous execution. An optional local
supervisor adds callback retry and recovery after the parent process exits.
Creating a new launchd or systemd service is an explicit host operation.
`bootstrap update` and the release installer may reconcile only a service whose
manifest proves agentctl already manages it.

On Linux, the explicit systemd-user installer writes the exact plan-derived
unit beneath the user's systemd config root and an owner-only checksum manifest.
It refuses unmanaged, modified, or symlinked targets unless the operator uses
the documented force path after inspection. Failed activation or uninstall
restores the prior unit, manifest, and enabled/active state. It never removes
agentctl config, journal, credentials, or other state. Installing or updating
the agentctl binary alone does not create or restart a systemd service; after a
binary update, rerun `scripts/install-systemd-supervisor.sh` to reconcile an
existing managed unit.
