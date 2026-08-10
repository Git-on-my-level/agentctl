package ids

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestVersionOneFixtures(t *testing.T) {
	fixtures := []struct {
		typ     Type
		payload uint64
		want    string
	}{
		{TypeExecution, 0, "exec-abandon-abandon-abandon-abandon-abandon-aisle"},
		{TypeExecution, payloadMask, "exec-zoo-zoo-zoo-zoo-zoo-wish"},
		{TypeEvent, 0x0123456789abcdef & payloadMask, "event-banana-pencil-owner-cube-social-join"},
		{TypeHost, 0x0fedcba987654321, "host-yard-total-plastic-uncover-drip-angry"},
	}
	for _, fixture := range fixtures {
		id, err := FromPayload(fixture.typ, fixture.payload)
		if err != nil {
			t.Fatal(err)
		}
		if id.String() != fixture.want {
			t.Fatalf("fixture = %s, want %s", id, fixture.want)
		}
		parsed, err := Parse(fixture.want)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Type() != fixture.typ || parsed.Payload() != fixture.payload {
			t.Fatalf("round trip = %#v", parsed)
		}
	}
}

func TestEveryRegisteredTypeRoundTrips(t *testing.T) {
	for _, typ := range Types() {
		id, err := FromPayload(typ, 42)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ParseAs(typ, id.String()); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
}

func TestWrongTypeFixtureIsRejected(t *testing.T) {
	event, err := FromPayload(TypeEvent, 0)
	if err != nil {
		t.Fatal(err)
	}
	wrong := string(TypeExecution) + strings.TrimPrefix(event.String(), string(TypeEvent))
	if _, err := Parse(wrong); !errors.Is(err, ErrInvalid) {
		t.Fatalf("wrong type checksum accepted: %s", wrong)
	}
}

func TestRejectsOneBitOneWordAndCaseErrors(t *testing.T) {
	valid, _ := FromPayload(TypeExecution, 0)
	cases := []string{strings.Replace(valid.String(), "aisle", "able", 1), strings.Replace(valid.String(), "abandon", "ability", 1), strings.ToUpper(valid.String())}
	for _, value := range cases {
		if _, err := Parse(value); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Parse(%q) error=%v", value, err)
		}
	}
}

func TestTypedJSONRejectsWrongType(t *testing.T) {
	value, _ := FromPayload(TypeEvent, 7)
	var execution ExecutionID
	if err := json.Unmarshal([]byte(`"`+value.String()+`"`), &execution); !errors.Is(err, ErrInvalid) {
		t.Fatalf("error = %v", err)
	}
}

func TestWordListContract(t *testing.T) {
	if got := WordListDigest(); got != "sha256:2f5eed53a4727b4bf8880d8f3f199efc90e58503646d9ff8eff3a2ed3b24dbda" {
		t.Fatalf("digest=%s", got)
	}
	if len(wordList) != 2048 || len(wordIndex) != 2048 {
		t.Fatalf("word list size = %d/%d", len(wordList), len(wordIndex))
	}
	for _, word := range wordList {
		if !wordPattern.MatchString(word) {
			t.Fatalf("invalid word %q", word)
		}
	}
}

func TestPortableURIFixtures(t *testing.T) {
	host, _ := FromPayload(TypeHost, 1)
	source, _ := FromPayload(TypeSource, 2)
	value := "codex://" + host.String() + "/" + source.String()
	parsed, err := ParseURI(value)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.String() != value || parsed.Scheme != "codex" {
		t.Fatalf("parsed=%#v", parsed)
	}
	bad := strings.Replace(value, "codex://", "multica://", 1)
	if _, err := ParseURI(bad); !errors.Is(err, ErrInvalid) {
		t.Fatalf("invalid URI err=%v", err)
	}
}
