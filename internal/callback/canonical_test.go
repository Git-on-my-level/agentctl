package callback

import (
	"encoding/json"
	"math"
	"testing"
)

func TestCanonicalJSONDeterministicMapAndNegativeZero(t *testing.T) {
	got, err := CanonicalJSON(map[string]any{"z": []any{true, "x"}, "a": -0.0, "n": json.Number("1.0")})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"a":0,"n":1,"z":[true,"x"]}` {
		t.Fatalf("canonical JSON = %s", got)
	}
	if _, err := CanonicalJSON(math.Inf(1)); err == nil {
		t.Fatal("expected non-finite number rejection")
	}
}

func TestSemanticDedupeFixture(t *testing.T) {
	producer, err := NewProducer("codex", 1)
	if err != nil {
		t.Fatal(err)
	}
	event, err := producer.Produce(map[string]any{"authority_scope": "run-1", "kind": "terminal", "source_id": "turn-9"})
	if err != nil {
		t.Fatal(err)
	}
	if string(event.Projection) != `{"authority_scope":"run-1","kind":"terminal","source_id":"turn-9"}` {
		t.Fatalf("projection = %s", event.Projection)
	}
	const want = "sha256:9fdaa568b5e1ae22bf875e4357a3fa72d00331a39adb8d0e78ed6e85b66ba91b"
	if event.DedupeKey != want {
		t.Fatalf("key = %s, want %s", event.DedupeKey, want)
	}
}
