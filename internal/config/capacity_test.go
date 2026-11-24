package config

import (
	"strings"
	"testing"
)

func TestCapacityRejectsInvalidDrainSettings(t *testing.T) {
	negative, overOne := -.01, 1.01
	for _, test := range []struct {
		name     string
		capacity Capacity
		want     string
	}{
		{"profile policy", Capacity{Policy: "priority"}, "capacity.policy"},
		{"pool policy", Capacity{Pools: map[string]CapacityPool{"work": {Policy: "balance"}}}, `capacity pool "work" policy`},
		{"negative epsilon", Capacity{Epsilon: &negative}, "capacity.epsilon"},
		{"epsilon above one", Capacity{Epsilon: &overOne}, "capacity.epsilon"},
		{"negative expected cost", Capacity{ExpectedCost: &negative}, "capacity.expected_cost"},
		{"expected cost above one", Capacity{ExpectedCost: &overOne}, "capacity.expected_cost"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := test.capacity.Validate(); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("Validate() = %v, want an error naming %s", err, test.want)
			}
		})
	}
}
