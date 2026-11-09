package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func seedTerminalForRetention(t *testing.T, journal *Journal, createdAt time.Time, mutation contracts.MutationKey, configure ...func(*model.Execution)) model.Execution {
	t.Helper()
	ctx := context.Background()
	initial := sampleExecution(createdAt)
	initial.CreatedAt = createdAt
	initial.UpdatedAt = createdAt
	initial.Observation.ObservedAt = createdAt
	initial.Capabilities.NegotiatedAt = createdAt
	for _, apply := range configure {
		apply(&initial)
	}
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

// Collected fixtures model history eligible for automatic cleanup. Unread
// safety cases use seedTerminalForRetention directly, regardless of epoch.
func seedCollectedForRetention(t *testing.T, journal *Journal, at time.Time, mutation contracts.MutationKey, configure ...func(*model.Execution)) model.Execution {
	t.Helper()
	value := seedTerminalForRetention(t, journal, at, mutation, configure...)
	if _, _, err := journal.AcknowledgeExecution(context.Background(), value.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestInventoryIsAggregateAndReadOnly(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)
	seedCollectedForRetention(t, journal, now.Add(-48*time.Hour), contracts.MutationKey{})
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
	old := seedCollectedForRetention(t, journal, now.Add(-72*time.Hour), mutation)
	recent := seedCollectedForRetention(t, journal, now.Add(-2*time.Hour), contracts.MutationKey{})
	running, _, err := journal.CreateExecution(ctx, sampleExecution(now), contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 1 || plan.Eligible[0].ExecutionID != old.ID.String() || plan.Records.Executions != 1 || plan.Records.Events != 1 || plan.Records.Outcomes != 1 || plan.Records.IdempotencyKeys != 1 || plan.LogicalBytes == 0 {
		t.Fatalf("unexpected plan: %#v", plan)
	}
	if _, err := journal.GetExecution(ctx, old.ID); err != nil {
		t.Fatalf("planning wrote journal: %v", err)
	}
	applied, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{})
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
	afterPlan, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := journal.ApplyCleanup(ctx, cutoff, afterPlan.PlanDigest, CleanupOptions{})
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
	seedCollectedForRetention(t, journal, now.Add(-72*time.Hour), contracts.MutationKey{})
	cutoff := now.Add(-24 * time.Hour)
	plan, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	additional := seedCollectedForRetention(t, journal, now.Add(-48*time.Hour), contracts.MutationKey{})
	if _, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale reviewed plan was accepted: %v", err)
	}
	if _, err := journal.GetExecution(ctx, additional.ID); err != nil {
		t.Fatalf("failed digest check partially deleted data: %v", err)
	}
}

func TestCleanupProtectsActiveSubscriptionAndOutboxReceipt(t *testing.T) {
	ctx := context.Background()
	journal, _, now := openTestJournal(t)

	activeProtected := seedCollectedForRetention(t, journal, now.Add(-96*time.Hour), contracts.MutationKey{})
	active := durableTestSubscription(durableTestID(ids.TypeSubscription, 990))
	active.Filter.ExecutionIDs = []string{activeProtected.ID.String()}
	// Attention only: an all-kinds subscription to this already-terminal
	// execution would backfill its terminal delivery.
	active.Filter.Kinds = []string{string(model.EventAttention)}
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

	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
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
	if _, err := journal.ApplyCleanup(ctx, now.Add(-24*time.Hour), plan.PlanDigest, CleanupOptions{}); err != nil {
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
	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Eligible) != 0 || len(plan.Protected) != 2 || !containsReason(plan.Protected[0].Reasons, "promotion_reference") || !containsReason(plan.Protected[1].Reasons, "promotion_reference") {
		t.Fatalf("promotion graph was not protected: %#v", plan)
	}
	if _, err := journal.ApplyCleanup(ctx, now.Add(-24*time.Hour), plan.PlanDigest, CleanupOptions{}); err != nil {
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
	parent := seedCollectedForRetention(t, journal, now.Add(-72*time.Hour), contracts.MutationKey{})
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
	plan, err := journal.PlanCleanup(ctx, now.Add(-24*time.Hour), CleanupOptions{})
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

// openRetentionJournalWithEpoch opens a journal whose acknowledgement epoch is
// set to an explicit instant so a test can seed terminals on both sides of it.
func openRetentionJournalWithEpoch(t *testing.T, epoch time.Time) *Journal {
	t.Helper()
	generator := &sequenceGenerator{next: map[ids.Type]uint64{ids.TypeHost: 100, ids.TypeExecution: 200, ids.TypeEvent: 300}}
	path := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	journal, err := Open(path, Options{Generator: generator, Clock: func() time.Time { return epoch }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = journal.Close() })
	return journal
}

func planReasons(plan CleanupPlan, id ids.ExecutionID) []string {
	for _, protected := range plan.Protected {
		if protected.ExecutionID == id.String() {
			return protected.Reasons
		}
	}
	return nil
}

func planIsEligible(plan CleanupPlan, id ids.ExecutionID) bool {
	for _, eligible := range plan.Eligible {
		if eligible.ExecutionID == id.String() {
			return true
		}
	}
	return false
}

func TestCleanupProtectsUnreconciledTerminalResults(t *testing.T) {
	ctx := context.Background()
	epoch := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	journal := openRetentionJournalWithEpoch(t, epoch)
	preEpoch := seedTerminalForRetention(t, journal, epoch.Add(-48*time.Hour), contracts.MutationKey{})
	uncollected := seedTerminalForRetention(t, journal, epoch.Add(time.Hour), contracts.MutationKey{})
	collected := seedTerminalForRetention(t, journal, epoch.Add(2*time.Hour), contracts.MutationKey{})
	if _, _, err := journal.AcknowledgeExecution(ctx, collected.ID, AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	cutoff := epoch.Add(72 * time.Hour)

	plan, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if reasons := planReasons(plan, uncollected.ID); len(reasons) != 1 || reasons[0] != "result_unreconciled" {
		t.Fatalf("uncollected terminal is not protected as unreconciled: %#v", plan.Protected)
	}
	if plan.ProtectedUnreconciled != 2 {
		t.Fatalf("protected_unreconciled=%d want 2: %#v", plan.ProtectedUnreconciled, plan)
	}
	if plan.IncludeUnreconciled {
		t.Fatalf("default plan opted into unreconciled deletion: %#v", plan)
	}
	if reasons := planReasons(plan, preEpoch.ID); len(reasons) != 1 || reasons[0] != "result_unreconciled" {
		t.Fatalf("pre-epoch uncollected terminal is not protected: %#v", plan)
	}
	if !planIsEligible(plan, collected.ID) {
		t.Fatalf("acknowledged terminal was protected: %#v", plan)
	}

	// Applying the protective plan must leave the uncollected result intact.
	if _, err := journal.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetOutcome(ctx, uncollected.ID); err != nil {
		t.Fatalf("uncollected result was deleted: %v", err)
	}
	if _, err := journal.GetOutcome(ctx, preEpoch.ID); err != nil {
		t.Fatalf("pre-epoch uncollected outcome deleted: %v", err)
	}
	// Positive collection evidence unlocks even pre-epoch history.
	if _, _, err := journal.AcknowledgeExecution(ctx, preEpoch.ID, AcknowledgementAwait); err != nil {
		t.Fatal(err)
	}
	collectedPlan, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil || !planIsEligible(collectedPlan, preEpoch.ID) {
		t.Fatalf("collected pre-epoch terminal not eligible: %+v %v", collectedPlan, err)
	}
	if _, err := journal.GetExecution(ctx, collected.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("acknowledged terminal survived: %v", err)
	}
}

func TestCleanupIncludeUnreconciledIsAnExplicitOptOutWithADistinctDigest(t *testing.T) {
	ctx := context.Background()
	epoch := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	journal := openRetentionJournalWithEpoch(t, epoch)
	uncollected := seedTerminalForRetention(t, journal, epoch.Add(time.Hour), contracts.MutationKey{})
	cutoff := epoch.Add(72 * time.Hour)

	protective, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	permissive, err := journal.PlanCleanup(ctx, cutoff, CleanupOptions{IncludeUnreconciled: true})
	if err != nil {
		t.Fatal(err)
	}
	if !permissive.IncludeUnreconciled || permissive.ProtectedUnreconciled != 0 {
		t.Fatalf("opt-out plan still protects uncollected results: %#v", permissive)
	}
	if !planIsEligible(permissive, uncollected.ID) {
		t.Fatalf("opt-out plan did not select the uncollected terminal: %#v", permissive)
	}
	if protective.PlanDigest == permissive.PlanDigest {
		t.Fatalf("opting into unreconciled deletion did not change the plan digest: %s", protective.PlanDigest)
	}

	// A digest reviewed under one policy must not authorize the other.
	if _, err := journal.ApplyCleanup(ctx, cutoff, permissive.PlanDigest, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("permissive digest applied under the protective policy: %v", err)
	}
	if _, err := journal.ApplyCleanup(ctx, cutoff, protective.PlanDigest, CleanupOptions{IncludeUnreconciled: true}); !errors.Is(err, ErrConflict) {
		t.Fatalf("protective digest applied under the permissive policy: %v", err)
	}
	if _, err := journal.ApplyCleanup(ctx, cutoff, permissive.PlanDigest, CleanupOptions{IncludeUnreconciled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := journal.GetExecution(ctx, uncollected.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("explicit opt-out did not delete the uncollected terminal: %v", err)
	}
}

func TestCleanupPlansAndRemovesDiagnosticSibling(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	execution := seedCollectedForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	dir := filepath.Dir(j.path)
	o, err := diagnostic.OpenOutputs(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	path, err := o.Write(execution.ID.String(), []byte("private native output"))
	o.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Attach a pointer without changing any result bytes or journal history.
	err = j.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bOutcomes)
		var outcome model.Outcome
		if err := json.Unmarshal(b.Get([]byte(execution.ID.String())), &outcome); err != nil {
			return err
		}
		outcome.OutputRef = path
		raw, err := json.Marshal(outcome)
		if err != nil {
			return err
		}
		return b.Put([]byte(execution.ID.String()), raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	cutoff := now.Add(-24 * time.Hour)
	plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if plan.DiagnosticFiles != 1 || plan.DiagnosticBytes != int64(len("private native output")) || len(plan.Eligible) != 1 || plan.Eligible[0].DiagnosticFile == nil {
		t.Fatalf("plan=%#v", plan)
	}
	os.WriteFile(path, []byte("replacement output"), 0600)
	if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); !errors.Is(err, ErrConflict) {
		t.Fatalf("changed file accepted: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatal("stale plan deleted file", err)
	}
	plan, err = j.PlanCleanup(ctx, cutoff, CleanupOptions{})
	if err != nil {
		t.Fatal(err)
	}
	applied, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{})
	if err != nil || !applied.Applied || applied.DiagnosticFiles != 1 {
		t.Fatal(applied, err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("diagnostic retained", err)
	}
	inventory, err := j.Inventory(ctx)
	if err != nil || inventory.Executions.Total != 0 {
		t.Fatal(inventory, err)
	}
}
func TestCleanupIncludesLegacyDiagnosticPointer(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	execution := seedCollectedForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	path := filepath.Join(filepath.Dir(j.path), "failed-output-12345.txt")
	if err := os.WriteFile(path, []byte("legacy output"), 0600); err != nil {
		t.Fatal(err)
	}
	err := j.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket(bOutcomes)
		var outcome model.Outcome
		json.Unmarshal(b.Get([]byte(execution.ID.String())), &outcome)
		outcome.OutputRef = path
		raw, _ := json.Marshal(outcome)
		return b.Put([]byte(execution.ID.String()), raw)
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := j.PlanCleanup(ctx, now, CleanupOptions{})
	if err != nil || plan.DiagnosticFiles != 1 {
		t.Fatal(plan, err)
	}
	if _, err := j.ApplyCleanup(ctx, now, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("legacy sibling retained", err)
	}
}

func TestCleanupHandlesEvictedDiagnosticWithoutDroppingRecordOnBusyCache(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	execution := seedCollectedForRetention(t, j, now.Add(-48*time.Hour), contracts.MutationKey{})
	outputs, err := diagnostic.OpenOutputs(filepath.Dir(j.path), true)
	if err != nil {
		t.Fatal(err)
	}
	path, err := outputs.Write(execution.ID.String(), []byte("private diagnostic"))
	outputs.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Eviction may remove a diagnostic while its owning execution remains.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	plan, err := j.PlanCleanup(ctx, now, CleanupOptions{})
	if err != nil || plan.DiagnosticFiles != 0 || len(plan.Eligible) != 1 {
		t.Fatal(plan, err)
	}
	outputs, err = diagnostic.OpenOutputs(filepath.Dir(j.path), true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := j.ApplyCleanup(ctx, now, plan.PlanDigest, CleanupOptions{}); err == nil {
		t.Fatal("cleanup ignored busy cache")
	}
	outputs.Close()
	if _, err := j.GetExecution(ctx, execution.ID); err != nil {
		t.Fatal("busy cleanup dropped record", err)
	}
	if _, err := j.ApplyCleanup(ctx, now, plan.PlanDigest, CleanupOptions{}); err != nil {
		t.Fatal("missing diagnostic blocked cleanup", err)
	}
}

func TestCleanupProtectsANXHistoryAcrossAuthorities(t *testing.T) {
	for _, authority := range []model.Authority{model.AuthorityNative, model.AuthorityMultica} {
		for _, reference := range []string{"label", "link", "legacy_spool"} {
			t.Run(string(authority)+"/"+reference, func(t *testing.T) {
				ctx := context.Background()
				j, _, now := openTestJournal(t)
				row := seedCollectedForRetention(t, j, now.Add(-60*24*time.Hour), contracts.MutationKey{}, func(initial *model.Execution) {
					initial.Authority = authority
					if authority == model.AuthorityMultica {
						initial.Mode = model.ModeMultica
					}
					if reference == "label" {
						initial.Labels = []string{"anx.card.keep-history"}
					}
				})
				options := CleanupOptions{IncludeUnreconciled: true}
				if reference == "link" {
					for range 2 {
						if err := j.ProtectANXRun(ctx, row.ID); err != nil {
							t.Fatal(err)
						}
					}
				} else if reference == "legacy_spool" {
					options.ANXRunLinks = []ids.ExecutionID{row.ID}
				}
				cutoff := now.Add(-30 * 24 * time.Hour)
				plan, err := j.PlanCleanup(ctx, cutoff, options)
				if err != nil {
					t.Fatal(err)
				}
				if len(plan.Eligible) != 0 || len(planReasons(plan, row.ID)) != 1 || planReasons(plan, row.ID)[0] != "anx_run_link" {
					t.Fatalf("ANX history eligible: %+v", plan)
				}
				if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, options); err != nil {
					t.Fatal(err)
				}
				if _, err := j.GetOutcome(ctx, row.ID); err != nil {
					t.Fatal(err)
				}
				events, err := j.ListEvents(ctx, row.ID, contracts.EventQuery{})
				if err != nil || len(events) == 0 {
					t.Fatalf("events=%d err=%v", len(events), err)
				}
				batch, err := j.PlanCleanupBatch(ctx, cutoff, options.ANXRunLinks...)
				if err != nil || batch.Executions() != 0 {
					t.Fatalf("batch=%d err=%v", batch.Executions(), err)
				}
			})
		}
	}
}

func TestCleanupProtectsUnreadWithoutAcknowledgementEpoch(t *testing.T) {
	ctx := context.Background()
	j, _, now := openTestJournal(t)
	row := seedTerminalForRetention(t, j, now.Add(-60*24*time.Hour), contracts.MutationKey{})
	if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bMetadata).Delete(keyAckEpoch) }); err != nil {
		t.Fatal(err)
	}
	plan, err := j.PlanCleanup(ctx, now, CleanupOptions{})
	if err != nil || planIsEligible(plan, row.ID) || plan.ProtectedUnreconciled != 1 {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
}

func TestCleanupProtectsLegacyANXUncertaintyWithoutReclassifyingOnReuse(t *testing.T) {
	for _, authority := range []model.Authority{model.AuthorityNative, model.AuthorityMultica} {
		t.Run(string(authority), func(t *testing.T) {

			ctx := context.Background()
			j, _, now := openTestJournal(t)
			mutation := contracts.MutationKey{Scope: "execution:run", Key: "legacy-root", InputDigest: hash('a')}
			configure := func(value *model.Execution) {
				value.Authority = authority
				if authority == model.AuthorityMultica {
					value.Mode = model.ModeMultica
				}
			}
			legacy := seedTerminalForRetention(t, j, now.Add(-60*24*time.Hour), mutation, configure)
			control := seedCollectedForRetention(t, j, now.Add(-60*24*time.Hour), contracts.MutationKey{}, configure)
			// Model a pre-feature admission whose former custom spool is not discoverable
			// from today's config. Positive collection evidence is already present.
			if err := j.db.Update(func(tx *bbolt.Tx) error { return tx.Bucket(bRetentionTracking).Delete([]byte(legacy.ID.String())) }); err != nil {
				t.Fatal(err)
			}
			if _, reused, err := j.CreateExecution(ctx, sampleExecution(now), mutation); err != nil || !reused {
				t.Fatalf("reuse=%v err=%v", reused, err)
			}
			if _, _, err := j.AcknowledgeExecution(ctx, legacy.ID, AcknowledgementAwait); err != nil {
				t.Fatal(err)
			}
			cutoff := now.Add(-30 * 24 * time.Hour)
			for _, override := range []bool{false, true} {
				options := CleanupOptions{IncludeUnreconciled: override}
				plan, err := j.PlanCleanup(ctx, cutoff, options)
				if err != nil {
					t.Fatal(err)
				}
				reasons := planReasons(plan, legacy.ID)
				if len(reasons) != 1 || reasons[0] != "legacy_anx_history_unknown" || !planIsEligible(plan, control.ID) || planIsEligible(plan, legacy.ID) {
					t.Fatalf("unsafe legacy plan: %+v", plan)
				}
			}
			batch, err := j.PlanCleanupBatch(ctx, cutoff)
			if err != nil || batch.Executions() != 1 {
				t.Fatalf("batch=%d %v", batch.Executions(), err)
			}
			plan, err := j.PlanCleanup(ctx, cutoff, CleanupOptions{})
			if err != nil || plan.Records.RetentionTracking != 1 {
				t.Fatalf("tracking count=%+v %v", plan, err)
			}
			if _, err := j.ApplyCleanup(ctx, cutoff, plan.PlanDigest, CleanupOptions{}); err != nil {
				t.Fatal(err)
			}
			if _, err := j.GetOutcome(ctx, legacy.ID); err != nil {
				t.Fatal(err)
			}
			if err := j.db.View(func(tx *bbolt.Tx) error {
				if tx.Bucket(bRetentionTracking).Get([]byte(control.ID.String())) != nil {
					t.Error("deleted execution left tracking record")
				}
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			events, err := j.ListEvents(ctx, legacy.ID, contracts.EventQuery{})
			if err != nil || len(events) == 0 {
				t.Fatalf("events=%d %v", len(events), err)
			}

		})
	}
}
