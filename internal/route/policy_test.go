package route

import "testing"

func preferredTable() []ModelRecord {
	return []ModelRecord{
		{Adapter: "cursor", Model: "cursor-grok-4.6-high", Aliases: []string{"grok", "grok-4.6"}},
		{Adapter: "codex", Model: "gpt-5.6-sol", Aliases: []string{"sol"}},
	}
}

func TestNewCatalogOmitsBuiltinsWhenPreferredExists(t *testing.T) {
	catalog := NewCatalog("m4-air", map[string]string{"m5": "m5-mbp"}, preferredTable(), "multica")
	if !HasConcretePreferences(catalog) {
		t.Fatal("expected concrete preference table")
	}
	got := Match("grok", catalog)
	if len(got.Models) != 1 || got.Models[0].Adapter != "cursor" || got.Models[0].Model != "cursor-grok-4.6-high" {
		t.Fatalf("in-table grok = %#v", got.Models)
	}
	off := Match("composer", catalog)
	if len(off.Models) != 0 {
		t.Fatalf("off-policy composer looked like a route: %#v", off.Models)
	}
	if len(off.Unmatched) == 0 || off.Unmatched[0] != "composer" {
		t.Fatalf("off-policy composer unmatched = %#v", off.Unmatched)
	}
}

func TestNewCatalogKeepsBuiltinsWithoutPreferred(t *testing.T) {
	catalog := NewCatalog("", nil, nil, "")
	got := Match("glm", catalog)
	if len(got.Models) != 1 || got.Models[0].Adapter != "omp" || got.Models[0].Model != "" {
		t.Fatalf("builtin glm = %#v", got.Models)
	}
}

func TestInPreferredTableExactAndAlias(t *testing.T) {
	table := preferredTable()
	if !InPreferredTable(table, "cursor", "cursor-grok-4.6-high") || !InPreferredTable(table, "CURSOR", "grok") {
		t.Fatal("expected grok aliases to match the reviewed cursor model")
	}
	if !InPreferredTable(table, "codex", "gpt-5.6-sol") || !InPreferredTable(table, "codex", "sol") {
		t.Fatal("expected sol aliases to match the reviewed codex model")
	}
	if InPreferredTable(table, "cursor", "composer-2.5") || InPreferredTable(table, "codex", "cursor-grok-4.6-high") {
		t.Fatal("off-policy adapter+model pair matched")
	}
	if InPreferredTable(table, "cursor", "") || InPreferredTable(BuiltinModelCatalog(), "cursor", "grok") {
		t.Fatal("empty model or builtin family records should not count as policy")
	}
}

func TestNativeArgvModel(t *testing.T) {
	if got := NativeArgvModel("cursor", []string{"cursor-agent", "--print", "--model", "composer-2.5", "--trust"}); got != "composer-2.5" {
		t.Fatalf("cursor --model = %q", got)
	}
	if got := NativeArgvModel("codex", []string{"codex", "exec", "--json", "-m", "gpt-5.6-sol"}); got != "gpt-5.6-sol" {
		t.Fatalf("codex -m = %q", got)
	}
	if got := NativeArgvModel("cursor", []string{"cursor-agent", "-m", "composer-2.5"}); got != "" {
		t.Fatalf("cursor -m should not be treated as a model: %q", got)
	}
	if got := NativeArgvModel("codex", []string{"codex", "exec", "--json", "--", "--model", "ignored"}); got != "" {
		t.Fatalf("model after -- should be ignored: %q", got)
	}
}
