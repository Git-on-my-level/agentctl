package store

// Retention is deliberately an explicit maintenance operation. The journal
// never deletes records on open or on a timer. Planning is read-only; applying
// recomputes the same protected execution graph inside one write transaction.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/subscription"
)

const RetentionSchemaVersion = 1

type BucketUsage struct {
	Name          string `json:"name"`
	Records       int    `json:"records"`
	NestedBuckets int    `json:"nested_buckets"`
	KeyBytes      int64  `json:"key_bytes"`
	ValueBytes    int64  `json:"value_bytes"`
}

type InventoryExecutionSummary struct {
	Total       int   `json:"total"`
	Terminal    int   `json:"terminal"`
	NonTerminal int   `json:"nonterminal"`
	Events      int   `json:"events"`
	Outcomes    int   `json:"outcomes"`
	ResultBytes int64 `json:"result_bytes"`
}

type RetentionInventory struct {
	SchemaVersion int                       `json:"schema_version"`
	GeneratedAt   time.Time                 `json:"generated_at"`
	JournalPath   string                    `json:"journal_path"`
	FileBytes     int64                     `json:"file_bytes"`
	LogicalBytes  int64                     `json:"logical_bytes"`
	Buckets       []BucketUsage             `json:"buckets"`
	Executions    InventoryExecutionSummary `json:"executions"`
}

type CleanupRecordCounts struct {
	Executions       int `json:"executions"`
	Events           int `json:"events"`
	EventIndexes     int `json:"event_indexes"`
	EventDedupe      int `json:"event_dedupe"`
	TerminalIndexes  int `json:"terminal_indexes"`
	Outcomes         int `json:"outcomes"`
	IdempotencyKeys  int `json:"idempotency_keys"`
	Acknowledgements int `json:"acknowledgements,omitempty"`
}

func (c CleanupRecordCounts) Total() int {
	return c.Executions + c.Events + c.EventIndexes + c.EventDedupe + c.TerminalIndexes + c.Outcomes + c.IdempotencyKeys + c.Acknowledgements
}

type CleanupExecution struct {
	ExecutionID  string              `json:"execution_id"`
	TerminalAt   time.Time           `json:"terminal_at"`
	Records      CleanupRecordCounts `json:"records"`
	LogicalBytes int64               `json:"logical_bytes"`
}

type ProtectedExecution struct {
	ExecutionID string    `json:"execution_id"`
	TerminalAt  time.Time `json:"terminal_at"`
	Reasons     []string  `json:"reasons"`
}

type CleanupPlan struct {
	SchemaVersion int                  `json:"schema_version"`
	Before        time.Time            `json:"before"`
	GeneratedAt   time.Time            `json:"generated_at"`
	Applied       bool                 `json:"applied"`
	PlanDigest    string               `json:"plan_digest"`
	Eligible      []CleanupExecution   `json:"eligible"`
	Protected     []ProtectedExecution `json:"protected"`
	Records       CleanupRecordCounts  `json:"records"`
	LogicalBytes  int64                `json:"logical_bytes"`
}

type cleanupEvent struct {
	id        string
	dedupeKey string
}

type cleanupTarget struct {
	CleanupExecution
	events          []cleanupEvent
	idempotencyKeys [][]byte
}

// Inventory returns aggregate logical usage without modifying the journal.
func (j *Journal) Inventory(ctx context.Context) (RetentionInventory, error) {
	if err := ctx.Err(); err != nil {
		return RetentionInventory{}, err
	}
	result := RetentionInventory{SchemaVersion: RetentionSchemaVersion, GeneratedAt: j.clock().UTC(), JournalPath: j.path, Buckets: []BucketUsage{}}
	if info, err := os.Stat(j.path); err == nil {
		result.FileBytes = info.Size()
	} else {
		return RetentionInventory{}, fmt.Errorf("stat journal: %w", err)
	}
	err := j.db.View(func(tx *bbolt.Tx) error {
		if err := tx.ForEach(func(name []byte, bucket *bbolt.Bucket) error {
			usage := BucketUsage{Name: string(name)}
			measureBucket(bucket, &usage)
			result.LogicalBytes += usage.KeyBytes + usage.ValueBytes
			result.Buckets = append(result.Buckets, usage)
			return nil
		}); err != nil {
			return err
		}
		executions := tx.Bucket(bExecutions)
		if executions != nil {
			if err := executions.ForEach(func(_, raw []byte) error {
				var value model.Execution
				if err := json.Unmarshal(raw, &value); err != nil {
					return corrupt(err)
				}
				if err := value.Validate(); err != nil {
					return corrupt(err)
				}
				result.Executions.Total++
				if value.State.Terminal() {
					result.Executions.Terminal++
				} else {
					result.Executions.NonTerminal++
				}
				return nil
			}); err != nil {
				return err
			}
		}
		if events := tx.Bucket(bEvents); events != nil {
			result.Executions.Events = events.Stats().KeyN
		}
		if outcomes := tx.Bucket(bOutcomes); outcomes != nil {
			if err := outcomes.ForEach(func(_, raw []byte) error {
				var value model.Outcome
				if err := json.Unmarshal(raw, &value); err != nil {
					return corrupt(err)
				}
				if err := value.Validate(); err != nil {
					return corrupt(err)
				}
				result.Executions.Outcomes++
				if value.Content != nil {
					result.Executions.ResultBytes += int64(len(value.Content.Text))
				}
				return nil
			}); err != nil {
				return err
			}
		}
		return nil
	})
	sort.Slice(result.Buckets, func(i, k int) bool { return result.Buckets[i].Name < result.Buckets[k].Name })
	return result, err
}

func measureBucket(bucket *bbolt.Bucket, usage *BucketUsage) {
	_ = bucket.ForEach(func(key, value []byte) error {
		usage.KeyBytes += int64(len(key))
		if value == nil {
			usage.NestedBuckets++
			if child := bucket.Bucket(key); child != nil {
				measureBucket(child, usage)
			}
			return nil
		}
		usage.Records++
		usage.ValueBytes += int64(len(value))
		return nil
	})
}

// PlanCleanup selects terminal execution graphs whose terminal timestamp is
// strictly before the cutoff. It never writes, including when opened writable.
func (j *Journal) PlanCleanup(ctx context.Context, before time.Time) (CleanupPlan, error) {
	if err := ctx.Err(); err != nil {
		return CleanupPlan{}, err
	}
	if before.IsZero() {
		return CleanupPlan{}, fmt.Errorf("cleanup cutoff is required")
	}
	var plan CleanupPlan
	err := j.db.View(func(tx *bbolt.Tx) error {
		var err error
		plan, _, err = j.cleanupPlanTx(tx, before.UTC())
		return err
	})
	return plan, err
}

// ApplyCleanup atomically recomputes and deletes the eligible graph. Repeating
// it is safe: already-deleted records simply produce an empty applied plan.
func (j *Journal) ApplyCleanup(ctx context.Context, before time.Time, expectedPlanDigest string) (CleanupPlan, error) {
	if j.readOnly {
		return CleanupPlan{}, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return CleanupPlan{}, err
	}
	if before.IsZero() {
		return CleanupPlan{}, fmt.Errorf("cleanup cutoff is required")
	}
	if expectedPlanDigest == "" {
		return CleanupPlan{}, fmt.Errorf("%w: cleanup plan digest is required", ErrConflict)
	}
	var plan CleanupPlan
	err := j.db.Update(func(tx *bbolt.Tx) error {
		var targets []cleanupTarget
		var err error
		plan, targets, err = j.cleanupPlanTx(tx, before.UTC())
		if err != nil {
			return err
		}
		if plan.PlanDigest != expectedPlanDigest {
			return fmt.Errorf("%w: cleanup plan changed; reviewed %s, current %s", ErrConflict, expectedPlanDigest, plan.PlanDigest)
		}
		for _, target := range targets {
			for _, event := range target.events {
				if err := tx.Bucket(bEvents).Delete([]byte(event.id)); err != nil {
					return err
				}
				if err := tx.Bucket(bDedupe).Delete([]byte(event.dedupeKey)); err != nil {
					return err
				}
			}
			if nested := tx.Bucket(bEventsByExecution).Bucket([]byte(target.ExecutionID)); nested != nil {
				if err := tx.Bucket(bEventsByExecution).DeleteBucket([]byte(target.ExecutionID)); err != nil {
					return err
				}
			}
			for _, key := range target.idempotencyKeys {
				if err := tx.Bucket(bIdempotency).Delete(key); err != nil {
					return err
				}
			}
			for _, bucket := range [][]byte{bTerminal, bOutcomes, bExecutions} {
				if err := tx.Bucket(bucket).Delete([]byte(target.ExecutionID)); err != nil {
					return err
				}
			}
			if acks := tx.Bucket(bAcknowledgements); acks != nil {
				if err := acks.Delete([]byte(target.ExecutionID)); err != nil {
					return err
				}
			}
		}
		plan.Applied = true
		return nil
	})
	return plan, err
}

func (j *Journal) cleanupPlanTx(tx *bbolt.Tx, before time.Time) (CleanupPlan, []cleanupTarget, error) {
	now := j.clock().UTC()
	plan := CleanupPlan{SchemaVersion: RetentionSchemaVersion, Before: before, GeneratedAt: now, Eligible: []CleanupExecution{}, Protected: []ProtectedExecution{}}
	executions := map[string]model.Execution{}
	base := map[string]bool{}
	if err := tx.Bucket(bExecutions).ForEach(func(key, raw []byte) error {
		var value model.Execution
		if err := json.Unmarshal(raw, &value); err != nil {
			return corrupt(err)
		}
		if err := value.Validate(); err != nil {
			return corrupt(err)
		}
		id := string(key)
		executions[id] = value
		base[id] = value.State.Terminal() && value.TerminalAt != nil && value.TerminalAt.Before(before)
		return nil
	}); err != nil {
		return CleanupPlan{}, nil, err
	}

	protected := map[string]map[string]bool{}
	protect := func(id, reason string) {
		if !base[id] {
			return
		}
		if protected[id] == nil {
			protected[id] = map[string]bool{}
		}
		protected[id][reason] = true
	}

	// Never partially remove an execution lineage. Promotion links are retained
	// unconditionally because they represent an authority handoff/recovery log.
	adjacency := map[string]map[string]bool{}
	link := func(a, b string) {
		if a == "" || b == "" || executions[b].ID.IsZero() {
			return
		}
		if adjacency[a] == nil {
			adjacency[a] = map[string]bool{}
		}
		if adjacency[b] == nil {
			adjacency[b] = map[string]bool{}
		}
		adjacency[a][b], adjacency[b][a] = true, true
	}
	for id, value := range executions {
		if value.ParentExecutionID != nil {
			link(id, value.ParentExecutionID.String())
		}
		for _, other := range value.Supersedes {
			link(id, other.String())
		}
		if value.SupersededBy != nil {
			link(id, value.SupersededBy.String())
		}
		if value.Promotion != nil {
			link(id, value.Promotion.CounterpartExecutionID.String())
			protect(id, "promotion_reference")
			protect(value.Promotion.CounterpartExecutionID.String(), "promotion_reference")
		}
	}
	visited := map[string]bool{}
	for id := range executions {
		if visited[id] {
			continue
		}
		queue := []string{id}
		component := []string{}
		allEligible := true
		for len(queue) > 0 {
			current := queue[0]
			queue = queue[1:]
			if visited[current] {
				continue
			}
			visited[current] = true
			component = append(component, current)
			if !base[current] {
				allEligible = false
			}
			for other := range adjacency[current] {
				if !visited[other] {
					queue = append(queue, other)
				}
			}
		}
		if !allEligible {
			for _, member := range component {
				protect(member, "execution_graph_partially_retained")
			}
		}
	}

	// Active exact subscriptions, including their cursor/coordinator references,
	// keep the complete execution history needed to interpret future delivery.
	if bucket := tx.Bucket(bSubscriptions); bucket != nil {
		if err := bucket.ForEach(func(_, raw []byte) error {
			var record SubscriptionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			value := record.Subscription
			active := value.State == subscription.StateActive && (value.ExpiresAt.IsZero() || now.Before(value.ExpiresAt))
			if !active {
				return nil
			}
			for _, id := range value.Filter.ExecutionIDs {
				protect(id, "active_subscription")
			}
			protect(value.CoordinatorExecutionID, "active_subscription")
			if value.Cursor.EventID != "" {
				if eventID := tx.Bucket(bEvents).Get([]byte(value.Cursor.EventID)); eventID != nil {
					var stored storedEvent
					if err := json.Unmarshal(eventID, &stored); err != nil {
						return corrupt(err)
					}
					protect(stored.Event.ExecutionID.String(), "active_subscription_cursor")
				}
			}
			return nil
		}); err != nil {
			return CleanupPlan{}, nil, err
		}
	}

	// Any retained delivery (pending, acknowledged, paused, or dead-lettered)
	// keeps the event/execution it names. Receipts must have an outbox record;
	// an orphan is corruption rather than permission to erase uncertain data.
	deliveryExecutions := map[string]string{}
	protectDeliveryBucket := func(bucketName []byte) error {
		bucket := tx.Bucket(bucketName)
		if bucket == nil {
			return nil
		}
		return bucket.ForEach(func(key, raw []byte) error {
			item, err := decodeDelivery(raw)
			if err != nil {
				return err
			}
			executionID := item.ExecutionID
			if executionID == "" {
				eventRaw := tx.Bucket(bEvents).Get([]byte(item.EventID))
				if eventRaw == nil {
					return fmt.Errorf("%w: delivery event missing", ErrCorrupt)
				}
				var stored storedEvent
				if err := json.Unmarshal(eventRaw, &stored); err != nil {
					return corrupt(err)
				}
				executionID = stored.Event.ExecutionID.String()
			}
			deliveryExecutions[string(key)] = executionID
			protect(executionID, "outbox_delivery")
			return nil
		})
	}
	if err := protectDeliveryBucket(bOutbox); err != nil {
		return CleanupPlan{}, nil, err
	}
	if err := protectDeliveryBucket(bOutboxDead); err != nil {
		return CleanupPlan{}, nil, err
	}
	if receipts := tx.Bucket(bOutboxReceipts); receipts != nil {
		if err := receipts.ForEach(func(key, _ []byte) error {
			executionID, ok := deliveryExecutions[string(key)]
			if !ok {
				return fmt.Errorf("%w: receipt delivery missing", ErrCorrupt)
			}
			protect(executionID, "delivery_receipt")
			return nil
		}); err != nil {
			return CleanupPlan{}, nil, err
		}
	}

	// A protected member protects its full relationship component. Deleting a
	// sibling while retaining a parent, supersession target, or promotion peer
	// would leave a dangling execution reference even when both are terminal.
	queue := make([]string, 0, len(protected))
	for id := range protected {
		queue = append(queue, id)
	}
	seenProtected := map[string]bool{}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if seenProtected[current] {
			continue
		}
		seenProtected[current] = true
		for other := range adjacency[current] {
			if base[other] {
				protect(other, "execution_graph_reference")
				queue = append(queue, other)
			}
		}
	}

	targets := []cleanupTarget{}
	ids := make([]string, 0, len(base))
	for id, eligible := range base {
		if eligible {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		value := executions[id]
		if reasons := protected[id]; len(reasons) > 0 {
			list := make([]string, 0, len(reasons))
			for reason := range reasons {
				list = append(list, reason)
			}
			sort.Strings(list)
			plan.Protected = append(plan.Protected, ProtectedExecution{ExecutionID: id, TerminalAt: *value.TerminalAt, Reasons: list})
			continue
		}
		target, err := buildCleanupTarget(tx, value)
		if err != nil {
			return CleanupPlan{}, nil, err
		}
		targets = append(targets, target)
		plan.Eligible = append(plan.Eligible, target.CleanupExecution)
		addCleanupCounts(&plan.Records, target.Records)
		plan.LogicalBytes += target.LogicalBytes
	}
	digestInput := struct {
		SchemaVersion int                `json:"schema_version"`
		Before        time.Time          `json:"before"`
		Eligible      []CleanupExecution `json:"eligible"`
	}{RetentionSchemaVersion, before, plan.Eligible}
	raw, err := json.Marshal(digestInput)
	if err != nil {
		return CleanupPlan{}, nil, err
	}
	sum := sha256.Sum256(raw)
	plan.PlanDigest = "sha256:" + hex.EncodeToString(sum[:])
	return plan, targets, nil
}

func buildCleanupTarget(tx *bbolt.Tx, execution model.Execution) (cleanupTarget, error) {
	id := execution.ID.String()
	target := cleanupTarget{CleanupExecution: CleanupExecution{ExecutionID: id, TerminalAt: *execution.TerminalAt}, events: []cleanupEvent{}, idempotencyKeys: [][]byte{}}
	add := func(key, value []byte) { target.LogicalBytes += int64(len(key) + len(value)) }
	execRaw := tx.Bucket(bExecutions).Get([]byte(id))
	target.Records.Executions = 1
	add([]byte(id), execRaw)
	if outcome := tx.Bucket(bOutcomes).Get([]byte(id)); outcome != nil {
		target.Records.Outcomes++
		add([]byte(id), outcome)
	}
	if acks := tx.Bucket(bAcknowledgements); acks != nil {
		if raw := acks.Get([]byte(id)); raw != nil {
			target.Records.Acknowledgements++
			add([]byte(id), raw)
		}
	}
	if terminal := tx.Bucket(bTerminal).Get([]byte(id)); terminal != nil {
		target.Records.TerminalIndexes++
		add([]byte(id), terminal)
	}
	if nested := tx.Bucket(bEventsByExecution).Bucket([]byte(id)); nested != nil {
		target.LogicalBytes += int64(len(id))
		if err := nested.ForEach(func(seq, eventID []byte) error {
			raw := tx.Bucket(bEvents).Get(eventID)
			if raw == nil {
				return fmt.Errorf("%w: event index target missing", ErrCorrupt)
			}
			var stored storedEvent
			if err := json.Unmarshal(raw, &stored); err != nil {
				return corrupt(err)
			}
			if stored.Event.ExecutionID != execution.ID {
				return fmt.Errorf("%w: event index execution mismatch", ErrCorrupt)
			}
			dedupeRaw := tx.Bucket(bDedupe).Get([]byte(stored.Event.DedupeKey))
			if dedupeRaw == nil {
				return fmt.Errorf("%w: event dedupe index missing", ErrCorrupt)
			}
			var dedupe dedupeRecord
			if err := json.Unmarshal(dedupeRaw, &dedupe); err != nil {
				return corrupt(err)
			}
			if dedupe.EventID != string(eventID) {
				return fmt.Errorf("%w: event dedupe target mismatch", ErrCorrupt)
			}
			target.events = append(target.events, cleanupEvent{id: string(eventID), dedupeKey: stored.Event.DedupeKey})
			target.Records.Events++
			target.Records.EventIndexes++
			target.Records.EventDedupe++
			add(eventID, raw)
			add(seq, eventID)
			add([]byte(stored.Event.DedupeKey), dedupeRaw)
			return nil
		}); err != nil {
			return cleanupTarget{}, err
		}
	}
	if bucket := tx.Bucket(bIdempotency); bucket != nil {
		if err := bucket.ForEach(func(key, raw []byte) error {
			var record mutationRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			if record.ObjectType == "execution" && record.ObjectID == id {
				target.idempotencyKeys = append(target.idempotencyKeys, append([]byte(nil), key...))
				target.Records.IdempotencyKeys++
				add(key, raw)
			}
			return nil
		}); err != nil {
			return cleanupTarget{}, err
		}
	}
	return target, nil
}

func addCleanupCounts(target *CleanupRecordCounts, value CleanupRecordCounts) {
	target.Executions += value.Executions
	target.Events += value.Events
	target.EventIndexes += value.EventIndexes
	target.EventDedupe += value.EventDedupe
	target.TerminalIndexes += value.TerminalIndexes
	target.Outcomes += value.Outcomes
	target.IdempotencyKeys += value.IdempotencyKeys
	target.Acknowledgements += value.Acknowledgements
}
