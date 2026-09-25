package launchrecipe

import (
	"reflect"
	"testing"
)

func TestOpenClawRecipe(t *testing.T) {
	got, err := Build(Input{Harness: "openclaw", Model: "xai/grok-4.3", Effort: "high"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"openclaw", "agent", "--local", "--agent", "main", "--model", "xai/grok-4.3", "--json", "--thinking", "high", "-m"}
	if !reflect.DeepEqual(got.Argv, want) || got.PromptDelivery != PromptDeliveryArgv {
		t.Fatalf("recipe = %#v", got)
	}
	for _, input := range []Input{
		{Harness: "openclaw", Model: "grok-4.3"},
		{Harness: "openclaw", Model: "xai/grok-4.3", Speed: "fast"},
		{Harness: "openclaw", Model: "xai/grok-4.3", Effort: "ultra"},
	} {
		if _, err := Build(input); err == nil {
			t.Fatalf("accepted unsupported input: %#v", input)
		}
	}
}
