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

Remove `--plan` only after reviewing the remote side effect. Promotion support
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
