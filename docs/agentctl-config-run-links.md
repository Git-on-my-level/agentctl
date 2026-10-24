# Proposed agentctl-config run-link settings

This is documentation for a future reviewed agentctl-config bundle change.
No agentctl-config repository or installed config is modified by this PR.
The agentctl runtime defaults `allowed_orgs` to an empty list, disabling all
PR/issue repository evidence while retaining lifecycle registration.

For this repository, a proposed bundle setting is:

```json
"anx": {
  "run_links": {
    "default": "labeled",
    "workspace_alias": "anx",
    "allowed_orgs": ["Git-on-my-level"]
  }
}
```

Review organization names separately for each workspace before adopting
this proposal. Do not infer an allowed owner from a Git remote. An allowed
owner is necessary, and the run CWD's exact local GitHub origin must also
match the evidence owner and repository. Missing origin or an empty list
drops PR/issue evidence. Typed commit SHAs do not carry repo coordinates.
See [configuration](configuration.md#agent-nexus-run-links) for field bounds.
