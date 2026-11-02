package main

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestRecentCapacityMatchesDelegateReceipt(t *testing.T) {
	f := newPoolFixture(t, "priority")
	f.writeFacts(t, map[string]float64{"account-a": .1, "account-b": .2})
	code, out := f.invoke("capacity fixture", "--wait")
	if code != 0 {
		t.Fatalf("delegate code=%d %s", code, out)
	}
	var receipt struct {
		Result struct {
			Capacity *model.DelegationCapacity `json:"capacity"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &receipt); err != nil {
		t.Fatal(err)
	}
	doc, out, code := readRecentExport(t, f.journal)
	if code != 0 || len(doc.Result.Executions) != 1 {
		t.Fatalf("recent code=%d %s", code, out)
	}
	if receipt.Result.Capacity == nil || !reflect.DeepEqual(doc.Result.Executions[0].Capacity, receipt.Result.Capacity) {
		t.Fatalf("capacity differs from receipt: %s", out)
	}
	assertResultMatchesSchemaShape(t, out, "recent-result.schema.json")
}
