package delegation

import (
	"errors"
	"testing"
)

func grok46(defaulted bool) Entry {
	return Entry{
		Harness: "cursor",
		Family:  "grok",
		Version: "4.6",
		Model:   "cursor-grok-4.6-high",
		Speed:   "regular",
		Effort:  "high",
		Aliases: []string{"grok"},
		Default: defaulted,
	}
}

func TestResolveExactConstraints(t *testing.T) {
	got, err := Resolve(Selector{Harness: "cursor", Family: "grok", Version: "4.6", Settings: &Settings{Speed: "regular"}}, []Entry{grok46(false)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Resolved.Model != "cursor-grok-4.6-high" || !containsAll(got.Defaulted, "effort", "model") || len(got.Defaulted) != 2 {
		t.Fatalf("resolution=%#v", got)
	}
}

func TestResolveNormalizesCaseAndSpaceOnIdentifiers(t *testing.T) {
	got, err := Resolve(Selector{Harness: " Cursor ", Family: "GROK", Version: " 4.6 "}, []Entry{grok46(false)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Resolved.Model != "cursor-grok-4.6-high" {
		t.Fatalf("resolved=%#v", got.Resolved)
	}
}

func TestResolveModelIsExactAndNotCaseFolded(t *testing.T) {
	_, err := Resolve(Selector{Model: "Cursor-Grok-4.6-High"}, []Entry{grok46(false)})
	assertUsage(t, err)
	got, err := Resolve(Selector{Model: " cursor-grok-4.6-high "}, []Entry{grok46(false)})
	if err != nil || got.Resolved.Family != "grok" {
		t.Fatalf("trimmed model: %#v %v", got, err)
	}
}

func TestResolveAliasesIdentifyFamilyButNotVersion(t *testing.T) {
	entry := Entry{Harness: "cursor", Model: "cursor-grok-4.6-high", Speed: "regular", Aliases: []string{"grok", "grok-4.6"}}
	got, err := Resolve(Selector{Family: "grok"}, []Entry{entry})
	if err != nil || got.Resolved.Model != entry.Model {
		t.Fatalf("alias family: %#v %v", got, err)
	}
	_, err = Resolve(Selector{Family: "grok", Version: "4.6"}, []Entry{entry})
	assertUsage(t, err)
	_, err = Resolve(Selector{Family: "grok", Version: "4.6"}, []Entry{grok46(false)})
	if err != nil {
		t.Fatalf("version metadata: %v", err)
	}
}

func TestResolveDoesNotInferVersionFromModelSlug(t *testing.T) {
	entry := Entry{Harness: "cursor", Family: "grok", Model: "cursor-grok-4.6-high", Speed: "regular"}
	_, err := Resolve(Selector{Family: "grok", Version: "4.6"}, []Entry{entry})
	assertUsage(t, err)
}

func TestResolveIntersectsAllConstraints(t *testing.T) {
	codex := Entry{Harness: "codex", Family: "sol", Model: "gpt-5.6-sol", Speed: "regular", Aliases: []string{"sol"}}
	_, err := Resolve(Selector{Harness: "codex", Family: "grok"}, []Entry{grok46(true), codex})
	assertUsage(t, err)
}

func TestResolveFillsOmittedSettingsFromUniqueTuple(t *testing.T) {
	got, err := Resolve(Selector{Family: "grok"}, []Entry{grok46(false)})
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got.Resolved.Speed != "regular" || got.Resolved.Effort != "high" {
		t.Fatalf("resolved=%#v", got.Resolved)
	}
	if !containsAll(got.Defaulted, "harness", "version", "model", "speed", "effort") {
		t.Fatalf("defaulted=%v", got.Defaulted)
	}
}

func TestResolveUsesSingleCompatibleDefault(t *testing.T) {
	older := Entry{Harness: "cursor", Family: "grok", Version: "4.5", Model: "cursor-grok-4.5", Speed: "regular"}
	got, err := Resolve(Selector{Family: "grok"}, []Entry{older, grok46(true)})
	if err != nil || got.Resolved.Version != "4.6" {
		t.Fatalf("default=%#v %v", got, err)
	}
	if !containsAll(got.Defaulted, "version") {
		t.Fatalf("defaulted=%v", got.Defaulted)
	}
}

func TestResolveAmbiguousDefaultsAreNotSilentlySelected(t *testing.T) {
	a := grok46(true)
	b := Entry{Harness: "cursor", Family: "grok", Version: "4.5", Model: "cursor-grok-4.5", Speed: "regular", Default: true}
	_, err := Resolve(Selector{Family: "grok"}, []Entry{b, a})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != KindAmbiguous {
		t.Fatalf("error=%v", err)
	}
	if len(typed.Candidates) != 2 || typed.Candidates[0].Version != "4.5" || typed.Candidates[1].Version != "4.6" {
		t.Fatalf("candidates=%#v", typed.Candidates)
	}
}

func TestResolveIsIndependentOfEntryOrder(t *testing.T) {
	older := Entry{Harness: "cursor", Family: "grok", Version: "4.5", Model: "cursor-grok-4.5", Speed: "regular"}
	sel := Selector{Family: "grok"}
	first, err := Resolve(sel, []Entry{older, grok46(true)})
	if err != nil {
		t.Fatal(err)
	}
	second, err := Resolve(sel, []Entry{grok46(true), older})
	if err != nil {
		t.Fatal(err)
	}
	if first.Resolved.Model != second.Resolved.Model || first.Resolved.Model != "cursor-grok-4.6-high" {
		t.Fatalf("order changed resolution: %#v %#v", first, second)
	}
}

func TestResolveAmbiguousCandidatesAreSorted(t *testing.T) {
	a := Entry{Harness: "cursor", Family: "grok", Version: "4.6", Model: "z-model", Speed: "regular"}
	b := Entry{Harness: "cursor", Family: "grok", Version: "4.5", Model: "a-model", Speed: "regular"}
	_, err := Resolve(Selector{Family: "grok"}, []Entry{a, b})
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != KindAmbiguous {
		t.Fatalf("error=%v", err)
	}
	if typed.Candidates[0].Model != "a-model" || typed.Candidates[1].Model != "z-model" {
		t.Fatalf("candidates=%#v", typed.Candidates)
	}
}

func TestResolveDoesNotMutateInputs(t *testing.T) {
	sel := Selector{Family: "grok", Settings: &Settings{Speed: "regular"}}
	entries := []Entry{grok46(false)}
	got, err := Resolve(sel, entries)
	if err != nil {
		t.Fatal(err)
	}
	sel.Settings.Speed = "changed"
	entries[0].Aliases[0] = "changed"
	if got.Requested.Settings.Speed != "regular" || got.Resolved.Aliases[0] != "grok" {
		t.Fatalf("input mutation leaked: %#v", got)
	}
}

func TestResolveRequiresFamilyOrModel(t *testing.T) {
	_, err := Resolve(Selector{Harness: "cursor"}, []Entry{grok46(true)})
	assertUsage(t, err)
}

func containsAll(got []string, want ...string) bool {
	have := map[string]bool{}
	for _, value := range got {
		have[value] = true
	}
	for _, value := range want {
		if !have[value] {
			return false
		}
	}
	return true
}

func assertUsage(t *testing.T, err error) {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) || typed.Kind != KindUsage {
		t.Fatalf("error=%v, want usage", err)
	}
}
