package store

import (
	"errors"
	"testing"
)

func TestChildExpiryJSONMembers(t *testing.T) {
	for _, test := range []struct {
		name string
		raw  string
		bad  bool
	}{
		{"compact", `{"object_type":"expired","object_id":"child","input_digest":"digest"}`, false},
		{"full", `{"scope":"parent","key":"child","object_type":"expired","object_id":"child","input_digest":"digest"}`, false},
		{"scope-duplicate", `{"scope":null,"scope":"parent"}`, true},
		{"key-duplicate", `{"key":null,"key":"child"}`, true},
		{"digest-duplicate", `{"input_digest":null,"input_digest":"digest"}`, true},
		{"type-duplicate", `{"object_type":"execution","object_type":"expired"}`, true},
		{"id-duplicate", `{"object_id":null,"object_id":"child"}`, true},
		{"case-alias", `{"Scope":null,"scope":"parent"}`, true},
		{"reverse-alias", `{"scope":null,"Scope":"parent"}`, true},
		{"escaped-name", `{"\u0073cope":null,"scope":"parent"}`, true},
		{"unicode-alias", `{"ſcope":null,"scope":"parent"}`, true},
		{"nested-duplicate", `{"extension":{"x":null,"x":1}}`, true},
		{"array-object-duplicate", `{"extension":[{"x":null,"x":1}]}`, true},
		{"separate-objects", `{"a":{"x":1},"b":{"x":2}}`, false},
		{"separate-array-objects", `{"extension":[{"x":1},{"x":2}]}`, false},
		{"unknown-case-distinct", `{"x":1,"X":2}`, false},
		{"large-extension-number", `{"extension":1e1000000}`, false},
		{"null", `null`, true},
		{"array-root", `[]`, true},
		{"trailing-value", `{"x":1}{"x":2}`, true},
		{"incomplete", `{"x":`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := validateChildExpiryJSON([]byte(test.raw))
			if test.bad && !errors.Is(err, ErrCorrupt) {
				t.Fatalf("ambiguous/invalid expiry JSON accepted: %v", err)
			}
			if !test.bad && err != nil {
				t.Fatal(err)
			}
		})
	}
}
