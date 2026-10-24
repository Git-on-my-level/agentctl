# Proposed agentctl-config run-link settings

This is documentation for a future reviewed agentctl-config bundle change.
No agentctl-config repository or installed config is modified by this PR.
Under David’s 2026-10-24 ruling, the runtime default `allowed_orgs: []`
allows any owner. Repository/user information and local paths may be disclosed;
credentials, prompt, transcript and answer text remain excluded.

For a shared workspace that opts into an owner restriction, a proposed setting is:

```json
"anx": {
  "run_links": {
    "default": "labeled",
    "workspace_alias": "anx",
    "allowed_orgs": ["Git-on-my-level"]
  }
}
```

A non-empty list restricts origin repository metadata and typed PR/issue
evidence to the listed owners. PR/issue evidence must also match the run CWD's
local origin owner/repository on any host. Missing origin drops repository
metadata and PR/issue evidence. An empty list permits every origin owner;
it does not disable evidence. URL userinfo is stripped before parsing and
all outbound fields retain credential checks. Typed commit SHAs carry no
repo coordinates. See [configuration](configuration.md#agent-nexus-run-links).
