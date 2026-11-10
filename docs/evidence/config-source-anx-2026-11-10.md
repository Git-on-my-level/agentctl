# Config bundle Anx inspection, 2026-11-10

Repository inspected: [agentctl-config at commit
950abdc7f4d934d423f7d743baef76ad297da358](https://github.com/Git-on-my-level/agentctl-config/tree/950abdc7f4d934d423f7d743baef76ad297da358),
the default `main` head at inspection time. The GitHub recursive Git tree was
complete (not truncated); JSON contents were read by their pinned blob IDs.

The default `config-bundle.json` and all four published `hosts/*.json` bundles
have no top-level `anx` section. Neither `source/base.json` nor
`source/hosts.json` has one. Therefore no currently published bundle at that
revision requires repair of previously omitted Anx settings.

This inspection covers committed default-branch inputs and bundles only. It
does not assert anything about other refs or host-local live config; no live
host config was read or changed.
