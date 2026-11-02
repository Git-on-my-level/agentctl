package adapter

import (
	"errors"
	"testing"
)

func TestProcPIDNamespaceRejectsInheritedAndUnavailableViews(t *testing.T) {
	for _, scenario := range []struct {
		name       string
		self, view uint64
		fail       string
		valid      bool
	}{
		{"matching", 7, 7, "", true},
		{"inherited_ancestor", 7, 8, "", false},
		{"unknown", 0, 0, "", false},
		{"self_hidden", 7, 7, "/proc/self/ns/pid", false},
		{"init_hidden", 7, 7, "/proc/1/ns/pid", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			got, err := ProcPIDNamespace(func(path string) (uint64, error) {
				if path == scenario.fail {
					return 0, errors.New("unavailable")
				}
				switch path {
				case "/proc/self/ns/pid":
					return scenario.self, nil
				case "/proc/1/ns/pid":
					return scenario.view, nil
				default:
					t.Fatalf("unexpected path %s", path)
					return 0, nil
				}
			})
			if scenario.valid {
				if err != nil || got != scenario.self {
					t.Fatalf("valid view: inode=%d err=%v", got, err)
				}
			} else if err == nil || got != 0 {
				t.Fatalf("unsafe view accepted: inode=%d err=%v", got, err)
			}
		})
	}
}
