package store

import (
	"context"
	"errors"
	"runtime"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func TestChildrenLoadSubscriptionsOnlyForUncoveredNonterminal(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	defer j.Close()
	parent, _, err := j.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	child := sampleExecution(now)
	child.ParentExecutionID = &parent.ID
	child.State, child.Liveness, child.TerminalAt = model.StateCompleted, model.LivenessExited, &now
	child, _, err = j.CreateExecution(ctx, child, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	for n := 0; n < 1025; n++ {
		sub := durableTestSubscription(durableTestID(ids.TypeSubscription, uint64(n+1)))
		sub.ExpiresAt = now.Add(time.Hour)
		if _, _, err = j.PutSubscription(ctx, sub); err != nil {
			t.Fatal(err)
		}
	}
	scope := ChildrenScope{Parent: &parent.ID}
	check, err := j.CheckChildCollection(ctx, scope, now)
	if err != nil || !check.Recheck || len(check.Children) != 1 {
		t.Fatalf("terminal queried irrelevant subscriptions: %+v %v", check, err)
	}
	if _, _, err = j.AcknowledgeExecution(ctx, child.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	check, err = j.CheckChildCollection(ctx, scope, now)
	if err != nil || check.Recheck || len(check.Children) != 0 {
		t.Fatalf("collected history queried subscriptions: %+v %v", check, err)
	}
	running := sampleExecution(now)
	running.ParentExecutionID = &parent.ID
	running, _, err = j.CreateExecution(ctx, running, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS == "darwin" || runtime.GOOS == "linux" {
		waiter, err := RegisterWaiter(ctx, j.path, running.ID)
		if err != nil {
			t.Fatal(err)
		}
		defer waiter.Close()
		check, err = j.CheckChildCollection(ctx, scope, now)
		if err != nil || !check.Recheck || len(check.Children) != 0 {
			t.Fatalf("exact await scanned subscriptions or retired obligation: %+v %v", check, err)
		}
		waiter.Close()
	}
	if _, err = j.CheckChildCollection(ctx, scope, now); !errors.Is(err, ErrChildrenBounded) {
		t.Fatalf("uncovered child bypassed subscription bound: %v", err)
	}
	running.State = model.StateAttention
	if _, err = j.UpdateExecution(ctx, running, running.Revision); err != nil {
		t.Fatal(err)
	}
	check, err = j.CheckChildCollection(ctx, scope, now)
	if err != nil || !check.Recheck || len(check.Children) != 1 || check.Children[0].State != model.StateAttention {
		t.Fatalf("attention queried subscriptions: %+v %v", check, err)
	}
}
