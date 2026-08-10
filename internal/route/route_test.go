package route

import (
	"reflect"
	"testing"
)

func TestExplainDefaultsToDirectAndKeepsAdapterIndependent(t *testing.T) {
	got, err := Explain(Request{ModelFamily: "glm"})
	if err != nil {
		t.Fatal(err)
	}
	want := Decision{
		Lifecycle: LifecycleDirect,
		Adapter:   "omp",
		Reasons:   []string{"direct_by_default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("decision = %#v, want %#v", got, want)
	}
}

func TestExplainDurableSignalsSelectMultica(t *testing.T) {
	got, err := Explain(Request{
		ModelFamily:      "gpt",
		NeedsPR:          true,
		CrossHostHandoff: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lifecycle != LifecycleMultica || got.Adapter != "codex" {
		t.Fatalf("unexpected decision: %#v", got)
	}
	wantReasons := []string{"pr_lifecycle", "cross_host_handoff"}
	if !reflect.DeepEqual(got.Reasons, wantReasons) {
		t.Fatalf("reasons = %#v, want %#v", got.Reasons, wantReasons)
	}
}

func TestExplicitLifecycleWinsWithoutChangingExecutor(t *testing.T) {
	got, err := Explain(Request{
		ExplicitLifecycle: LifecycleDirect,
		ModelFamily:       "cursor-grok",
		NeedsPR:           true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if got.Lifecycle != LifecycleDirect || got.Adapter != "cursor" || !got.Explicit {
		t.Fatalf("unexpected decision: %#v", got)
	}
}

func TestUnknownModelFamilyFailsClosed(t *testing.T) {
	if _, err := Explain(Request{ModelFamily: "mystery"}); err == nil {
		t.Fatal("expected unknown model family error")
	}
}
