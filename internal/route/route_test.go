package route

import "testing"

func TestAdapterForModelFamily(t *testing.T) {
	cases := map[string]string{
		"":       "",
		"auto":   "",
		"glm":    "omp",
		"gpt":    "codex",
		"grok":   "cursor",
		"claude": "claude",
	}
	for in, want := range cases {
		got, err := AdapterForModelFamily(in)
		if err != nil {
			t.Fatalf("%q: %v", in, err)
		}
		if got != want {
			t.Fatalf("%q: got %q want %q", in, got, want)
		}
	}
}

func TestUnknownModelFamilyFailsClosed(t *testing.T) {
	if _, err := AdapterForModelFamily("mystery"); err == nil {
		t.Fatal("expected unknown model family error")
	}
}
