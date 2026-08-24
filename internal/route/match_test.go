package route

import (
	"encoding/json"
	"strings"
	"testing"
)

func testCatalog() Catalog {
	return Catalog{
		ThisHost: "m4-air",
		Hosts: []HostRecord{
			{ID: "m4-air", Aliases: []string{"air"}},
			{ID: "m1-mac-studio", Aliases: []string{"studio", "m1"}},
			{ID: "m4-mac-mini", Aliases: []string{"mini"}},
			{ID: "m5-mbp", Aliases: []string{"m5"}},
		},
		Models: append(BuiltinModelCatalog(),
			ModelRecord{Adapter: "omp", Model: "glm-5.3", Speed: "regular", Aliases: []string{"omp", "glm", "glm-5.3"}},
			ModelRecord{Adapter: "cursor", Model: "cursor-grok-4.6-high", Speed: "regular", Aliases: []string{"grok", "grok-4.6"}},
			ModelRecord{Adapter: "codex", Model: "gpt-5.6-sol", Speed: "regular", Aliases: []string{"sol"}},
		),
		PlacementKind: "multica",
	}
}

func TestMatchStudioOmpIsObvious(t *testing.T) {
	got := Match("studio omp", testCatalog())
	if len(got.Hosts) != 1 || got.Hosts[0].ID != "m1-mac-studio" {
		t.Fatalf("hosts = %#v", got.Hosts)
	}
	if len(got.Models) < 1 || got.Models[0].Adapter != "omp" || got.Models[0].Model != "glm-5.3" {
		t.Fatalf("models = %#v", got.Models)
	}
	if got.Placement.Mode != "remote" || got.Placement.Kind != "multica" || got.Placement.Host != "m1-mac-studio" {
		t.Fatalf("placement = %#v", got.Placement)
	}
}

func TestMatchLocalGlmHasNoHost(t *testing.T) {
	got := Match("glm", testCatalog())
	if len(got.Hosts) != 0 {
		t.Fatalf("hosts = %#v", got.Hosts)
	}
	if got.Placement.Mode != "no_host" {
		t.Fatalf("placement = %#v", got.Placement)
	}
	if len(got.Models) < 1 || got.Models[0].Adapter != "omp" {
		t.Fatalf("models = %#v", got.Models)
	}
}

func TestMatchKeepsMultipleModels(t *testing.T) {
	got := Match("studio grok or sol", testCatalog())
	if len(got.Hosts) != 1 || got.Hosts[0].ID != "m1-mac-studio" {
		t.Fatalf("hosts = %#v", got.Hosts)
	}
	adapters := map[string]bool{}
	for _, model := range got.Models {
		adapters[model.Adapter+"/"+model.Model] = true
	}
	if !adapters["cursor/cursor-grok-4.6-high"] || !adapters["codex/gpt-5.6-sol"] {
		t.Fatalf("expected grok and sol, got %#v", got.Models)
	}
	if contains(got.Unmatched, "or") {
		t.Fatalf("glue 'or' should not appear in unmatched: %#v", got.Unmatched)
	}
}

func TestMatchDoesNotFailClosedOnProse(t *testing.T) {
	got := Match("please land a pr", testCatalog())
	if len(got.Hosts) != 0 || len(got.Models) != 0 {
		t.Fatalf("unexpected hits: %#v", got)
	}
	if len(got.Unmatched) == 0 {
		t.Fatal("expected unmatched prose tokens")
	}
}

func TestMatchHostnameSubstringWithoutAlias(t *testing.T) {
	catalog := Catalog{
		ThisHost: "box",
		Hosts:    []HostRecord{{ID: "m1-mac-studio"}},
		Models:   BuiltinModelCatalog(),
	}
	got := Match("studio", catalog)
	if len(got.Hosts) != 1 || got.Hosts[0].ID != "m1-mac-studio" {
		t.Fatalf("hosts = %#v", got.Hosts)
	}
}

func TestMatchThisHostIsLocal(t *testing.T) {
	got := Match("air grok", testCatalog())
	if got.Placement.Mode != "local" || got.Placement.Host != "m4-air" {
		t.Fatalf("placement = %#v", got.Placement)
	}
}

func TestMatchRemoteWithoutPlacementAsks(t *testing.T) {
	catalog := testCatalog()
	catalog.PlacementKind = ""
	got := Match("studio glm", catalog)
	if got.Placement.Mode != "need_placement" {
		t.Fatalf("placement = %#v", got.Placement)
	}
}

func TestMatchBuiltinFamilyWithoutPreferred(t *testing.T) {
	got := Match("glm", Catalog{Models: BuiltinModelCatalog()})
	if len(got.Models) != 1 || got.Models[0].Adapter != "omp" || got.Models[0].Model != "" {
		t.Fatalf("models = %#v", got.Models)
	}
}

func TestMatchMacHitsSeveralHosts(t *testing.T) {
	got := Match("mac", testCatalog())
	if len(got.Hosts) < 2 {
		t.Fatalf("expected several mac hosts, got %#v", got.Hosts)
	}
	if got.Placement.Mode != "ambiguous_host" {
		t.Fatalf("placement = %#v", got.Placement)
	}
}

func TestMatchEmptyQueryIsEmpty(t *testing.T) {
	got := Match("", testCatalog())
	if len(got.Tokens) != 0 || got.Placement.Mode != "no_host" {
		t.Fatalf("got %#v", got)
	}
}

func TestMatchJSONOmitsMatchAnnotations(t *testing.T) {
	raw, err := json.Marshal(Match("studio omp", testCatalog()))
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	for _, waste := range []string{`"kind":"exact"`, `"hit":"studio"`, `"hit":"omp"`, `"score":`, `"tokens":`, `"query":`, `"reason":`, `"speed":"regular"`} {
		if strings.Contains(s, waste) {
			t.Fatalf("wasted annotation %s in %s", waste, s)
		}
	}
	if !strings.Contains(s, `"id":"m1-mac-studio"`) || !strings.Contains(s, `"adapter":"omp"`) {
		t.Fatalf("missing useful fields: %s", s)
	}

	raw, err = json.Marshal(Match("glm", testCatalog()))
	if err != nil {
		t.Fatal(err)
	}
	s = string(raw)
	if strings.Contains(s, `"hosts"`) {
		t.Fatalf("empty hosts should be omitted: %s", s)
	}
}

func contains(in []string, want string) bool {
	for _, v := range in {
		if v == want {
			return true
		}
	}
	return false
}
