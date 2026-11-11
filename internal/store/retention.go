package store

// Retention never runs on open. Explicit cleanup and supervisor maintenance
// share protective selection and bounded transactions.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"github.com/Git-on-my-level/agentctl/internal/ids"
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
	ReplayTombstones int `json:"replay_tombstones,omitempty"`
	Acknowledgements int `json:"acknowledgements,omitempty"`
	StatusContacts   int `json:"status_contacts,omitempty"`
}

func (c CleanupRecordCounts) Total() int {
	return c.Executions + c.Events + c.EventIndexes + c.EventDedupe + c.TerminalIndexes + c.Outcomes + c.IdempotencyKeys + c.Acknowledgements + c.StatusContacts
}

type CleanupExecution struct {
	Partial        bool                   `json:"partial,omitempty"`
	DiagnosticFile *diagnostic.OutputFile `json:"diagnostic_file,omitempty"`
	ExecutionID    string                 `json:"execution_id"`
	TerminalAt     time.Time              `json:"terminal_at"`
	Records        CleanupRecordCounts    `json:"records"`
	LogicalBytes   int64                  `json:"logical_bytes"`
}

type ProtectedExecution struct {
	ExecutionID string    `json:"execution_id"`
	TerminalAt  time.Time `json:"terminal_at"`
	Reasons     []string  `json:"reasons"`
}

type CleanupPlan struct {
	HasMore         bool                 `json:"has_more"`
	DiagnosticFiles int                  `json:"diagnostic_files"`
	DiagnosticBytes int64                `json:"diagnostic_bytes"`
	SchemaVersion   int                  `json:"schema_version"`
	Before          time.Time            `json:"before"`
	GeneratedAt     time.Time            `json:"generated_at"`
	Applied         bool                 `json:"applied"`
	PlanDigest      string               `json:"plan_digest"`
	Eligible        []CleanupExecution   `json:"eligible"`
	Protected       []ProtectedExecution `json:"protected"`
	Records         CleanupRecordCounts  `json:"records"`
	LogicalBytes    int64                `json:"logical_bytes"`
	// IncludeUnreconciled reports whether the caller explicitly opted out of
	// uncollected-result protection. ProtectedUnreconciled counts the terminal
	// executions retained solely because nobody has collected their result.
	IncludeUnreconciled     bool `json:"include_unreconciled"`
	ProtectedUnreconciled   int  `json:"protected_unreconciled"`
	PreTrackingUnreconciled int  `json:"pre_tracking_unreconciled"`
}

// CleanupOptions carries the explicit caller intent that widens deletion
// beyond the safe default. A zero value is the protective policy.
type CleanupOptions struct {
	// IncludeUnreconciled deletes terminal executions whose result was never
	// acknowledged by result or await. It is destructive of uncollected
	// evidence and must come from an explicit caller flag.
	IncludeUnreconciled bool
	// ANXRunLinks is read-only legacy spool evidence for links predating stamps.
	ANXRunLinks []ids.ExecutionID
	automatic   bool
}

type cleanupEvent struct {
	kind      model.EventKind
	id        string
	dedupeKey string
	sequence  []byte
	raw       []byte
	dedupeRaw []byte
}

type cleanupTarget struct {
	CleanupExecution
	lastEventID     string
	anchorCount     int
	outputRef       string
	rawRecords      []cleanupRecord
	complete        bool
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
			if err := measureBucket(ctx, bucket, &usage); err != nil {
				return err
			}
			result.LogicalBytes += usage.KeyBytes + usage.ValueBytes
			result.Buckets = append(result.Buckets, usage)
			return nil
		}); err != nil {
			return err
		}
		executions := tx.Bucket(bExecutions)
		if executions != nil {
			if err := executions.ForEach(func(_, raw []byte) error {
				if err := ctx.Err(); err != nil {
					return err
				}
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
				if err := ctx.Err(); err != nil {
					return err
				}
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

func measureBucket(ctx context.Context, bucket *bbolt.Bucket, usage *BucketUsage) error {
	return bucket.ForEach(func(key, value []byte) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		usage.KeyBytes += int64(len(key))
		if value == nil {
			usage.NestedBuckets++
			if child := bucket.Bucket(key); child != nil {
				if err := measureBucket(ctx, child, usage); err != nil {
					return err
				}
			}
			return nil
		}
		usage.Records++
		usage.ValueBytes += int64(len(value))
		return nil
	})
}

func (snapshot *cleanupSnapshot) selectCleanup(before, now time.Time, options CleanupOptions) (CleanupPlan, []model.Execution, error) {
	plan := CleanupPlan{SchemaVersion: RetentionSchemaVersion, Before: before, GeneratedAt: now, Eligible: []CleanupExecution{}, Protected: []ProtectedExecution{}, IncludeUnreconciled: options.IncludeUnreconciled}
	snapshot.nextBefore = time.Time{}
	snapshot.nextCheckAt = time.Time{}
	executions := map[string]model.Execution{}
	base := map[string]bool{}
	if err := snapshot.forEach(bExecutions, func(key, raw []byte) error {
		if err := snapshot.ctx.Err(); err != nil {
			return err
		}
		var value model.Execution
		if err := json.Unmarshal(raw, &value); err != nil {
			return corrupt(err)
		}
		binding := snapshot.Bucket(bIdempotency).Get([]byte(attributionBindingScope + "\x00" + value.ID.String()))
		if err := hydrateAttributionRecord(binding, &value); err != nil {
			return corrupt(err)
		}
		if err := value.Validate(); err != nil {
			return corrupt(err)
		}
		if value.State.Terminal() && value.TerminalAt != nil && !value.TerminalAt.Before(before) && (snapshot.nextBefore.IsZero() || value.TerminalAt.Before(snapshot.nextBefore)) {
			snapshot.nextBefore = *value.TerminalAt
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

	// A run link is an external durable reference, independent of collection.
	for _, id := range options.ANXRunLinks {
		protect(id.String(), "anx_run_link")
	}
	for id := range snapshot.Bucket(bANXRunLinks) {
		protect(id, "anx_run_link")
	}
	// Automatic retention cannot revoke durable request-key replay or cached
	// authority bindings. Manual digest-reviewed cleanup remains explicit.
	if options.automatic {
		for id, value := range executions {
			if len(value.SourceBindings) > 0 {
				protect(id, "authority_binding")
			}
		}
		if err := snapshot.forEach(bIdempotency, func(_, raw []byte) error {
			var record mutationRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			// Launch snapshots and caller indexes are execution-owned metadata,
			// not request-key replay promises (including existing PR records).
			if record.ObjectType == "execution" && record.Scope != automaticContinuationScope && record.Scope != attributionBindingScope && !strings.HasPrefix(record.Scope, childrenIndexScope) {
				protect(record.ObjectID, "request_key_replay")
			}
			return nil
		}); err != nil {
			return CleanupPlan{}, nil, err
		}
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
		if err := snapshot.ctx.Err(); err != nil {
			return CleanupPlan{}, nil, err
		}
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
	for id, value := range executions {
		if err := snapshot.ctx.Err(); err != nil {
			return CleanupPlan{}, nil, err
		}
		if len(adjacency[id]) > 0 || value.ParentExecutionID != nil || len(value.Supersedes) > 0 || value.SupersededBy != nil || value.Promotion != nil {
			protect(id, "execution_graph_reference")
		}
	}
	visited := map[string]bool{}
	for id := range executions {
		if err := snapshot.ctx.Err(); err != nil {
			return CleanupPlan{}, nil, err
		}
		if visited[id] {
			continue
		}
		queue := []string{id}
		component := []string{}
		allEligible := true
		for len(queue) > 0 {
			if err := snapshot.ctx.Err(); err != nil {
				return CleanupPlan{}, nil, err
			}
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
	if bucket := snapshot.Bucket(bSubscriptions); bucket != nil {
		if err := snapshot.forEach(bSubscriptions, func(_, raw []byte) error {
			var record SubscriptionRecord
			if err := json.Unmarshal(raw, &record); err != nil {
				return corrupt(err)
			}
			value := record.Subscription
			active := (value.State == subscription.StateActive || value.State == subscription.StatePaused) && (value.ExpiresAt.IsZero() || now.Before(value.ExpiresAt))
			if !active {
				return nil
			}
			if !value.ExpiresAt.IsZero() && (snapshot.nextCheckAt.IsZero() || value.ExpiresAt.Before(snapshot.nextCheckAt)) {
				snapshot.nextCheckAt = value.ExpiresAt
			}
			for _, id := range value.Filter.ExecutionIDs {
				protect(id, "active_subscription")
			}
			protect(value.CoordinatorExecutionID, "active_subscription")
			if value.Cursor.EventID != "" {
				if eventID := snapshot.Bucket(bEvents).Get([]byte(value.Cursor.EventID)); eventID != nil {
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
		bucket := snapshot.Bucket(bucketName)
		if bucket == nil {
			return nil
		}
		return snapshot.forEach(bucketName, func(key, raw []byte) error {
			item, err := decodeDelivery(raw)
			if err != nil {
				return err
			}
			executionID := item.ExecutionID
			if executionID == "" {
				eventRaw := snapshot.Bucket(bEvents).Get([]byte(item.EventID))
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
	if receipts := snapshot.Bucket(bOutboxReceipts); receipts != nil {
		if err := snapshot.forEach(bOutboxReceipts, func(key, _ []byte) error {
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

	// A terminal result nobody collected is uncollected evidence, not garbage.
	// Cleanup reclaims space; it must not double as an acknowledgement that a
	// human or agent already read the outcome. The epoch guard keeps this from
	// freezing old journals: terminals that predate acknowledgement tracking on
	// this journal may lack a stamp, so manual cleanup leaves them eligible.
	epoch, err := snapshot.acknowledgementEpoch()
	if err != nil {
		return CleanupPlan{}, nil, err
	}
	for id, value := range executions {
		if err := snapshot.ctx.Err(); err != nil {
			return CleanupPlan{}, nil, err
		}
		if !base[id] || snapshot.Bucket(bAcknowledgements).Get([]byte(id)) != nil {
			continue
		}
		if epoch.IsZero() || value.TerminalAt.Before(epoch) {
			plan.PreTrackingUnreconciled++
		} else if !options.IncludeUnreconciled {
			protect(id, "result_unreconciled")
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
		if err := snapshot.ctx.Err(); err != nil {
			return CleanupPlan{}, nil, err
		}
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

	targets := []model.Execution{}
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
			if reasons["result_unreconciled"] {
				plan.ProtectedUnreconciled++
			}
			plan.Protected = append(plan.Protected, ProtectedExecution{ExecutionID: id, TerminalAt: *value.TerminalAt, Reasons: list})
			continue
		}
		if options.automatic && snapshot.Bucket(bAcknowledgements).Get([]byte(id)) == nil {
			continue
		}
		targets = append(targets, value)
	}
	return plan, targets, nil
}
func addCleanupCounts(target *CleanupRecordCounts, value CleanupRecordCounts) {
	target.Executions += value.Executions
	target.Events += value.Events
	target.EventIndexes += value.EventIndexes
	target.EventDedupe += value.EventDedupe
	target.TerminalIndexes += value.TerminalIndexes
	target.Outcomes += value.Outcomes
	target.IdempotencyKeys += value.IdempotencyKeys
	target.ReplayTombstones += value.ReplayTombstones
	target.Acknowledgements += value.Acknowledgements
	target.StatusContacts += value.StatusContacts
}
