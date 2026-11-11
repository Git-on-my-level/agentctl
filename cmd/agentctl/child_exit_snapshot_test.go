package main

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"go.etcd.io/bbolt"
)

func TestChildExitInboxUsesHydratedSnapshotAfterLegacyRewrite(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	ctx := context.Background()
	j, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	parent := identityFixture(t, j, "codex", "legacy-inbox")
	child := addExitChild(t, j, parent, false)
	child.State = model.StateAttention
	child, err = j.UpdateExecution(ctx, child, child.Revision)
	if err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	c := common{configPath: f.config, journalPath: f.journal}
	parent, err = a.finalizeResult(ctx, c, j, parent, adapter.Result{Success: true, State: adapter.StateCompleted, Content: "parent answer"}, time.Now().UTC(), false)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err = j.AcknowledgeExecution(ctx, parent.ID, store.AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	historical, err := j.GetOutcome(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	j.Close()
	// Deliberately uncertified raw write models a pre-field, pre-certificate
	// supervisor. Production writers must still all use Journal.update.
	db, err := bbolt.Open(f.journal, 0600, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte("executions"))
		for _, id := range []string{parent.ID.String(), child.ID.String()} {
			var e map[string]any
			if err := json.Unmarshal(bucket.Get([]byte(id)), &e); err != nil {
				return err
			}
			delete(e, "parent_execution_id")
			delete(e, "child_exit_recheck")
			delete(e, "child_exit_guard")
			delete(e, "uncollected_children")
			raw, err := json.Marshal(e)
			if err != nil {
				return err
			}
			if err = bucket.Put([]byte(id), raw); err != nil {
				return err
			}
		}
		return nil
	})
	db.Close()
	if err != nil {
		t.Fatal(err)
	}
	if problem := a.inbox(ctx, output.Renderer{Mode: output.JSON, Writer: &out}, c, nil); problem != nil {
		t.Fatal(problem)
	}
	var envelope struct {
		Result struct {
			Executions []inboxExecution `json:"executions"`
		} `json:"result"`
	}
	if err = json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, item := range envelope.Result.Executions {
		if item.ID == parent.ID {
			found = true
			if len(item.UncollectedChildren) != 1 || item.UncollectedChildren[0].ID != child.ID || item.UncollectedChildren[0].State != model.StateAttention {
				t.Fatalf("hydrated obligation lost: %+v", item)
			}
		}
	}
	if !found {
		t.Fatal("parent retired from raw projections instead of hydrated snapshot")
	}
	j, err = store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer j.Close()
	after, err := j.GetOutcome(ctx, parent.ID)
	if err != nil {
		t.Fatal(err)
	}
	beforeRaw, _ := json.Marshal(historical)
	afterRaw, _ := json.Marshal(after)
	if !bytes.Equal(beforeRaw, afterRaw) {
		t.Fatal("inbox rewrote immutable outcome")
	}
}
