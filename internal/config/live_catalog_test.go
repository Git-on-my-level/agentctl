package config

import "testing"

func TestLiveCatalogEntriesFlagConflictingAliasExecutables(t *testing.T) {
	profile := Profile{
		Adapters:         map[string]Adapter{"cursor": {Executable: "/a/cursor"}, "cursor-agent": {Executable: "/b/cursor"}},
		AgentPreferences: &AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{{Agent: "cursor", Model: "composer-2.5"}, {Agent: "codex", Model: "gpt-6-luna"}}},
	}
	entries := LiveCatalogEntries(profile)
	if len(entries) != 2 || !entries[0].Ambiguous || entries[1].Ambiguous || entries[1].Executable != "codex" {
		t.Fatalf("entries = %+v", entries)
	}
}
