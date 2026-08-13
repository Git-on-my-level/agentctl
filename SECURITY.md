# Security policy

## Supported versions

Security fixes are provided for the latest tagged preview release and the
current `main` branch. Pre-1.0 interfaces may change when a safe fix requires
it. Older preview releases should be upgraded before a report is evaluated.

## Reporting a vulnerability

Use GitHub's private vulnerability-reporting form on this repository's
**Security** tab. Include the affected version or commit, impact, reproduction
steps, and any proposed mitigation. Please do not include credentials, private
agent output, journal files, or native session databases unless a maintainer
requests a narrowly redacted artifact through that private report.

If the private form is not visible, open a public issue containing only a
request for a private security contact. Do not describe the vulnerability in
that issue. The project does not currently advertise a monitored security email
address.

Maintainers will acknowledge a report in the private GitHub thread, validate
the affected surface, and coordinate disclosure after a fix is available. No
specific response-time SLA is offered during the public preview.

## Scope

Security-relevant surfaces include:

- unsafe state, config, callback, or skill-install paths;
- disclosure of credentials, prompts, final results, or native session data;
- callback forgery, replay, SSRF, or destination rebinding;
- command or argument injection across adapter boundaries;
- journal integrity and authorization-boundary bypasses; and
- release artifact or update-chain compromise.

The local threat boundary does not defend data from another unrestricted
process running as the same operating-system user. See
[State, security, privacy, and retention](docs/state-security-and-privacy.md)
for the documented model.
