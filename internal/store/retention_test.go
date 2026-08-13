package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

func seedTerminalForRetention(t *testing.T, journal *Journal, createdAt time.Time, mutation contracts.MutationKey) model.Execution {
	t.Helper()
	ctx := context.Background()
	initial := sampleExecution(createdAt)
	initial.CreatedAt = createdAt
	initial.UpdatedAt = createdAt
	initial.Observation.ObservedAt = createdAt
	initial.Capabilities.NegotiatedAt = createdAt
	execution, _, err := journal.CreateExecution(ctx, initial, mutation)
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := createdAt.Add(time.Minute)
	next := execution
	next.State = model.StateCompleted
	next.Liveness = model.LivenessExited
	next.TerminalAt = &terminalAt
	next.UpdatedAt = terminalAt
	next.Observation.ObservedAt = terminalAt
	text := "retained result for " + execution.ID.String()
	sum := sha256.Sum256([]byte(text))
	outcome := model.Outcome{
		SchemaVersion: model.SchemaVersion, ExecutionID: execution.ID, Revision: 1,
		State: model.StateCompleted, Availability: model.OutcomeStored, RecordedAt: terminalAt,
		Source: execution.Adapter, ResultRef: "agentctl://" + execution.OriginHostID.String() + "/" + execution.ID.String(),
		Content: &model.OutcomeContent{MediaType: "text/plain", Text: text, Preview: text, Bytes: len(text), SHA256: "sha256:" + hex.EncodeToString(sum[:])},
	}
	key, projection := semanticEvent(t, execution.Adapter, map[string]any{"retention": execution.ID.String()})
	state := model.StateCompleted
	event := model.Event{ExecutionID: execution.ID, Authority: execution.Authority, Adapter: execution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &state, ObservedAt: terminalAt, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"result_available": true}}
	stored, _, _, _, err := journal.CommitTerminalOutcome(ctx, next, execution.Revision, outcome, event, projection)
	if err != nil {
		t.Fatal(err)
	}
	return stored
}

func TestInventoryIsAggregateAndReadOnly(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	seedTerminalForRetention(t, journal, now.Add(-48*time.Hour), contracts.MutationKey{})
	infoBefore, err := InventoryFileState(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := journal.Inventory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	infoAfter, err := InventoryFileState(journal.path)
	if err != nil {
		t.Fatal(err)
	}
	if infoBefore != infoAfter {
		t.Fatalf("inventory changed journal file: before=%#v after=%#v", infoBefore, infoAfter)
	}
	if inventory.Executions.Total != 1 || inventory.Executions.Terminal != 1 || inventory.Executions.Events != 1 || inventory.Executions.Outcomes != 1 || inventory.Executions.ResultBytes == 0 {
		t.Fatalf("unexpected inventory: %#v", inventory)
	}
	if inventory.FileBytes == 0 || inventory.LogicalBytes == 0 || len(inventory.Buckets) == 0 {
		t.Fatalf("missing usage: %#v", inventory)
	}
}

type inventoryFileState struct {
	Size    int64
	ModTime int64
}

func InventoryFileState(path string) (inventoryFileState, error) {
	info, err := os.Stat(path)
	if err != nil {
		return inventoryFileState{}, err
	}
	return inventoryFileState{Size: info.Size(), ModTime: info.ModTime().UnixNano()}, nil
}

func TestCleanupPlanApplyIsExactAtomicAndIdempotent(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	mutation := contracts.MutationKey{Scope: "execution:run", Key: "retention-old", InputDigest: hash('b')}
	old := seedTerminalForRetention(t, journal, now.Add(-72*time.Hour), mutation)
	recent := seedTerminalForRetention(t, journal, now.Add(-2*time.Hour), contracts.MutationKey{})
	running, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := journal.PlanCleanup(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Eligible[0].ExecutionID != old.ID.String() || plan.Records.Executions != 1 || plan.Records.Events != 1 || plan.Records.Outcomes != 1 || plan.Records.IdempotencyKeys != 1 || plan.LogicalBytes == 0 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if _, err := journal.GetExecution(ctx, old.ID); err != nil {
		t.Fatalf("planning wrote journal: %v", err)
	}
	applied, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest)
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Applied || applied.PlanDigest != plan.PlanDigest || applied.Records.Total() != plan.Records.Total() {
		t.Fatalf("applied plan differs: plan=%#v applied=%#v", plan, applied)
	}
	if _, err := journal.GetExecution(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old execution remains: %v", err)
	}
	if _, err := journal.GetOutcome(ctx, old.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old outcome remains: %v", err)
	}
	if _, err := journal.GetExecution(ctx, recent.ID); err != nil {
		t.Fatalf("recent execution removed: %v", err)
	}
	if _, err := journal.GetExecution(ctx, running.ID); err != nil {
		t.Fatalf("running execution removed: %v", err)
	}
	afterPlan, err := journal.PlanCleanup(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	again, err := journal.ApplyCleanup(ctx, cutoff, afterPlan.PlanDigest)
	if err != nil || !again.Applied || len(again.Eligible) != 0 || again.Records.Total() != 0 {
		t.Fatalf("repeat apply is not an idempotent no-op: %#v err=%v", again, err)
	}
	// The stale idempotency record was removed with its execution graph.
	created, reused, err := journal.CreateExecution(ctx, sampleExecution(now), mutation)
	if err != nil || reused || created.ID == old.ID {
		t.Fatalf("idempotency index was not cleaned: created=%#v reused=%v err=%v", created, reused, err)
	}
}

func TestCleanupApplyRejectsStaleReviewedPlan(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	seedTerminalForRetention(t, journal, now.Add(-72*time.Hour), contracts.MutationKey{})
	cutoff := now.Add(-24 * time.Hour)
	plan, err := journal.PlanCleanup(ctx, cutoff)
	if err != nil {
		t.Fatal(err)
	}
	additional := seedTerminalForRetention(t, journal, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reviewed plan was accepted: %v", err)
	}
	if _, err := journal.GetExecution(ctx, additional.ID); err != nil {
		t.Fatalf("failed digest check partially deleted data: %v", err)
	}
}

func TestCleanupProtectsActiveSubscriptionAndOutboxReceipt(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)

	activeProtected := seedTerminalForRetention(t, journal, now.Add(-96*time.Hour), contracts.MutationKey{})
	active := durableTestSubscription(durableTestID(ids.TypeSubscription, 990))
	active.Filter.ExecutionIDs = []string{activeProtected.ID.String()}
	if _, _, err := journal.PutSubscription(ctx, active); err != nil {
		t.Fatal(err)
	}

	// Seed an active terminal subscription before completion so fan-out leaves a
	// durable delivery. Acknowledging it stops the subscription, but both the
	// outbox record and receipt continue to protect the execution.
	createdAt := now.Add(-72 * time.Hour)
	initial := sampleExecution(createdAt)
	initial.CreatedAt, initial.UpdatedAt, initial.Observation.ObservedAt, initial.Capabilities.NegotiatedAt = createdAt, createdAt, createdAt, createdAt
	outboxExecution, _, err := journal.CreateExecution(ctx, initial, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	callbackSub := durableTestSubscription(durableTestID(ids.TypeSubscription, 991))
	callbackSub.Filter.ExecutionIDs = []string{outboxExecution.ID.String()}
	callbackSub.Filter.Kinds = []string{string(model.EventTerminal)}
	callbackSub.AutoExpireOnTerminal = true
	if _, _, err := journal.PutSubscription(ctx, callbackSub); err != nil {
		t.Fatal(err)
	}
	terminalAt := createdAt.Add(time.Minute)
	next := outboxExecution
	next.State, next.Liveness, next.TerminalAt, next.UpdatedAt, next.Observation.ObservedAt = model.StateCompleted, model.LivenessExited, &terminalAt, terminalAt, terminalAt
	text := "callback result"
	sum := sha256.Sum256([]byte(text))
	outcome := model.Outcome{SchemaVersion: 1, ExecutionID: outboxExecution.ID, Revision: 1, State: model.StateCompleted, Availability: model.OutcomeStored, RecordedAt: terminalAt, Source: outboxExecution.Adapter, ResultRef: "agentctl://" + outboxExecution.OriginHostID.String() + "/" + outboxExecution.ID.String(), Content: &model.OutcomeContent{MediaType: "text/plain", Text: text, Preview: text, Bytes: len(text), SHA256: "sha256:" + hex.EncodeToString(sum[:])}}
	key, projection := semanticEvent(t, outboxExecution.Adapter, map[string]any{"terminal": "callback"})
	state := model.StateCompleted
	event := model.Event{ExecutionID: outboxExecution.ID, Authority: outboxExecution.Authority, Adapter: outboxExecution.Adapter, Ordering: model.OrderingObservation, Kind: model.EventTerminal, State: &state, ObservedAt: terminalAt, DedupeKey: key, DedupeVersion: 1, Payload: map[string]any{"result_available": true}}
	if _, _, _, _, err := journal.CommitTerminalOutcome(ctx, next, outboxExecution.Revision, outcome, event, projection); err != nil {
		t.Fatal(err)
	}
	pending, err := journal.ListPendingDeliveries(ctx)
	if err != nil || len(pending) != 1 {
		t.Fatalf("pending=%#v err=%v", pending, err)
	}
	if err := journal.Ack(ctx, pending[0].ID); err != nil {
		t.Fatal(err)
	}

	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 0 || len(plan.Protected) != 2 {
		t.Fatalf("protected records were eligible: %#v", plan)
	}
	reasons := map[string][]string{}
	for _, item := range plan.Protected {
		reasons[item.ExecutionID] = item.Reasons
	}
	if !containsReason(reasons[activeProtected.ID.String()], "active_subscription") {
		t.Fatalf("active subscription reason missing: %#v", reasons)
	}
	if !containsReason(reasons[outboxExecution.ID.String()], "outbox_delivery") || !containsReason(reasons[outboxExecution.ID.String()], "delivery_receipt") {
		t.Fatalf("delivery protection reasons missing: %#v", reasons)
	}
	if _, err := journal.ApplyCleanup(ctx, now.Add(-24*time.Hour), plan.PlanDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetExecution(ctx, activeProtected.ID); err != nil {
		t.Fatalf("active reference deleted: %v", err)
	}
	if _, err := journal.GetExecution(ctx, outboxExecution.ID); err != nil {
		t.Fatalf("delivery reference deleted: %v", err)
	}
}

func TestCleanupProtectsPromotionGraph(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	oneID, _ := ids.FromPayload(ids.TypeExecution, 1200)
	twoID, _ := ids.FromPayload(ids.TypeExecution, 1201)
	promotionKey := hash('c')
	create := func(id ids.ID, other ids.ID, role string) model.Execution {
		createdAt := now.Add(-72 * time.Hour)
		value := sampleExecution(createdAt)
		value.ID = ids.ExecutionID(id.String())
		value.CreatedAt, value.UpdatedAt, value.Observation.ObservedAt, value.Capabilities.NegotiatedAt = createdAt, createdAt, createdAt, createdAt
		value.Promotion = &model.PromotionLink{Role: role, CounterpartExecutionID: ids.ExecutionID(other.String()), PromotionKey: promotionKey, State: model.PromotionActive}
		created, _, err := journal.CreateExecution(ctx, value, contracts.MutationKey{})
		if err != nil {
			t.Fatal(err)
		}
		terminalAt := createdAt.Add(time.Minute)
		created.State, created.Liveness, created.TerminalAt, created.UpdatedAt, created.Observation.ObservedAt = model.StateCompleted, model.LivenessExited, &terminalAt, terminalAt, terminalAt
		created, err = journal.UpdateExecution(ctx, created, created.Revision)
		if err != nil {
			t.Fatal(err)
		}
		return created
	}
	one := create(oneID, twoID, "source")
	two := create(twoID, oneID, "target")
	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 0 || len(plan.Protected) != 2 || !containsReason(plan.Protected[0].Reasons, "promotion_reference") || !containsReason(plan.Protected[1].Reasons, "promotion_reference") {
		t.Fatalf("promotion graph was not protected: %#v", plan)
	}
	if _, err := journal.ApplyCleanup(ctx, now.Add(-24*time.Hour), plan.PlanDigest); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetExecution(ctx, one.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetExecution(ctx, two.ID); err != nil {
		t.Fatal(err)
	}
}

func TestCleanupPropagatesReferenceProtectionAcrossEligibleGraph(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	parent := seedTerminalForRetention(t, journal, now.Add(-72*time.Hour), contracts.MutationKey{})
	createdAt := now.Add(-71 * time.Hour)
	childValue := sampleExecution(createdAt)
	childValue.CreatedAt, childValue.UpdatedAt, childValue.Observation.ObservedAt, childValue.Capabilities.NegotiatedAt = createdAt, createdAt, createdAt, createdAt
	childValue.ParentExecutionID = &parent.ID
	child, _, err := journal.CreateExecution(ctx, childValue, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	terminalAt := createdAt.Add(time.Minute)
	child.State, child.Liveness, child.TerminalAt, child.UpdatedAt, child.Observation.ObservedAt = model.StateCompleted, model.LivenessExited, &terminalAt, terminalAt, terminalAt
	if _, err := journal.UpdateExecution(ctx, child, child.Revision); err != nil {
		t.Fatal(err)
	}
	active := durableTestSubscription(durableTestID(ids.TypeSubscription, 1300))
	active.Filter.ExecutionIDs = []string{parent.ID.String()}
	if _, _, err := journal.PutSubscription(ctx, active); err != nil {
		t.Fatal(err)
	}
	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 0 || len(plan.Protected) != 2 {
		t.Fatalf("linked eligible sibling was not protected: %#v", plan)
	}
	reasons := map[string][]string{}
	for _, item := range plan.Protected {
		reasons[item.ExecutionID] = item.Reasons
	}
	if !containsReason(reasons[child.ID.String()], "execution_graph_reference") {
		t.Fatalf("graph protection was not propagated: %#v", reasons)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, reason := range reasons {
		if reason == want {
			return true
		}
	}
	return false
}
