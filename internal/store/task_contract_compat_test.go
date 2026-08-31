package store

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"go.etcd.io/bbolt"
)

func installLegacyTaskContractFixture(t *testing.T, journal *Journal, rawContract string) ids.ExecutionID {
	t.Helper()
	created, _, err := journal.CreateExecution(context.Background(), sampleExecution(journal.clock().UTC()), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(created)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	document["task_contract"] = json.RawMessage(rawContract)
	encoded, err = json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.db.Update(func(tx *bbolt.Tx) error {
		return tx.Bucket(bExecutions).Put([]byte(created.ID.String()), encoded)
	}); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func TestExecutionV1ReadsLegacyTaskContractRows(t *testing.T) {
	journal, _, _ := openTestJournal(t)
	cases := []struct {
		name string
		raw  string
	}{
		{name: "empty", raw: `{}`},
		{name: "extension_and_old_values", raw: `{
  "objective_summary": "legacy bounded objective",
  "side_effect_boundary": "human reviewed write boundary",
  "expected_artifact_kinds": [],
  "future_extension": {"schema_version": 2}
}`},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			id := installLegacyTaskContractFixture(t, journal, test.raw)
			read, err := journal.GetExecution(context.Background(), id)
			if err != nil {
				t.Fatalf("GetExecution rejected v1 row: %v", err)
			}
			if read.TaskContract == nil {
				t.Fatal("legacy task contract was dropped")
			}
			listed, err := journal.ListExecutions(context.Background(), false)
			if err != nil {
				t.Fatalf("ListExecutions rejected v1 row: %v", err)
			}
			found := false
			for _, execution := range listed {
				found = found || execution.ID == id
			}
			if !found {
				t.Fatalf("legacy execution %s was not listed", id)
			}
		})
	}
}
