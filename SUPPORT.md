# Support

agentctl is a community-supported public preview. There is no paid support,
availability guarantee, or response-time SLA.

## Where to ask

- Use GitHub issues for reproducible bugs and scoped feature requests.
- Use repository discussions, when enabled, for usage questions and design
  exploration.
- Use [Security](SECURITY.md) for vulnerabilities; do not disclose them in a
  public issue.

Include the agentctl version, operating system and architecture, adapter name,
the relevant `agentctl doctor` or `capabilities` projection, and a minimal
reproduction. Redact usernames, filesystem paths, executable digests, host IDs,
authority endpoints, native session IDs, prompts, results, and credentials as
appropriate. Never upload an entire journal or harness session database.

## Support boundary

The latest tagged preview and current `main` are the supported agentctl code
lines. Built-in adapter parsers are maintained against documented structured
output, but native vendors own their CLIs and may change behavior independently.
Capability probes and `doctor` are the authoritative readiness check for an
installed version.

Multica, Tailscale, `tailnetctl`, `macctl`, private knowledge repositories, and
third-party callback receivers are optional integrations. Their installation,
accounts, network availability, and support remain with their respective
projects or operators.

For the current platform and capability matrix, see the
[README](README.md#support-matrix) and [Adapters](docs/adapters.md).
