package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"go.etcd.io/bbolt"
)

var errCleanupDeferred = errors.New("cleanup target exceeds final commit budget")

type cleanupRecord struct {
	bucket, key, raw []byte
	replacement      []byte
}

// PlanCleanup uses a caller-owned handle. CLI maintenance should use Cleanup
// instead, which closes the file handle between pages as well as transactions.
func (j *Journal) PlanCleanup(ctx context.Context, before time.Time, options CleanupOptions) (CleanupPlan, error) {
	plan, _, err := j.cleanupReader(ctx).plan(before, options, cleanupEventBatch)
	return plan, err
}

// Plan returns one read-only, digest-bound batch, with HasMore for subsequent
// plans. No journal or diagnostic cache is created as part of planning.
func (c Cleanup) Plan(ctx context.Context, before time.Time, options CleanupOptions) (CleanupPlan, error) {
	plan, _, err := c.reader(ctx).plan(before, options, cleanupEventBatch)
	return plan, err
}

func (r *cleanupReader) plan(before time.Time, options CleanupOptions, eventLimit int) (CleanupPlan, []cleanupTarget, error) {
	if err := r.ctx.Err(); err != nil {
		return CleanupPlan{}, nil, err
	}
	if before.IsZero() {
		return CleanupPlan{}, nil, fmt.Errorf("cleanup cutoff is required")
	}
	snapshot, err := r.snapshot(false)
	if err != nil {
		return CleanupPlan{}, nil, err
	}
	plan, executions, err := snapshot.selectCleanup(before.UTC(), r.now, options)
	if err != nil {
		return plan, nil, err
	}
	limit := 8
	if options.automatic {
		limit = 1
	}
	// Index mutation records once, rather than scanning the entire bucket for
	// every eligible execution (quadratic on a journal with years of history).
	mutations := map[string][]cleanupRecord{}
	if err := snapshot.forEach(bIdempotency, func(key, raw []byte) error {
		var record mutationRecord
		if err := json.Unmarshal(raw, &record); err != nil {
			return corrupt(err)
		}
		if record.ObjectType == "execution" {
			mutations[record.ObjectID] = append(mutations[record.ObjectID], cleanupRecord{bucket: bIdempotency, key: key, raw: raw})
		}
		return nil
	}); err != nil {
		return plan, nil, err
	}
	targets := make([]cleanupTarget, 0, len(executions))
	for _, execution := range executions {
		if err := r.ctx.Err(); err != nil {
			return plan, nil, err
		}
		target, err := r.target(execution, snapshot.Bucket(bExecutions).Get([]byte(execution.ID.String())), mutations[execution.ID.String()], eventLimit)
		if errors.Is(err, errCleanupDeferred) {
			plan.Protected = append(plan.Protected, ProtectedExecution{ExecutionID: execution.ID.String(), TerminalAt: *execution.TerminalAt, Reasons: []string{"cleanup_final_batch_too_large"}})
			continue
		}
		if err != nil {
			return plan, nil, err
		}
		// A deferred tail is not another selectable batch. Inspect candidates
		// before announcing more work, without adding them to this deletion plan.
		if len(targets) >= limit {
			plan.HasMore = true
			break
		}
		if !target.complete {
			plan.HasMore = true
		}
		targets = append(targets, target)
		plan.Eligible = append(plan.Eligible, target.CleanupExecution)
		addCleanupCounts(&plan.Records, target.Records)
		plan.LogicalBytes += target.LogicalBytes
		if target.DiagnosticFile != nil {
			plan.DiagnosticFiles++
			plan.DiagnosticBytes += target.DiagnosticFile.Bytes
		}
	}
	// Bind the actual event prefix as well as aggregate counts. Two partial
	// batches can have identical sizes; a stale digest must not authorize the
	// next prefix just because the execution and counts are unchanged.
	type eventSelection struct {
		ExecutionID string   `json:"execution_id"`
		EventIDs    []string `json:"event_ids"`
	}
	selections := make([]eventSelection, 0, len(targets))
	for _, target := range targets {
		selection := eventSelection{ExecutionID: target.ExecutionID, EventIDs: []string{}}
		for _, event := range target.events {
			selection.EventIDs = append(selection.EventIDs, event.id)
		}
		selections = append(selections, selection)
	}
	raw, err := json.Marshal(struct {
		SchemaVersion       int                `json:"schema_version"`
		Before              time.Time          `json:"before"`
		IncludeUnreconciled bool               `json:"include_unreconciled"`
		Eligible            []CleanupExecution `json:"eligible"`
		Selections          []eventSelection   `json:"selections"`
	}{RetentionSchemaVersion, before.UTC(), options.IncludeUnreconciled, plan.Eligible, selections})
	if err != nil {
		return plan, nil, err
	}
	sum := sha256.Sum256(raw)
	plan.PlanDigest = "sha256:" + hex.EncodeToString(sum[:])
	return plan, targets, nil
}

func (r *cleanupReader) target(execution model.Execution, executionRaw []byte, mutations []cleanupRecord, eventLimit int) (cleanupTarget, error) {
	id := []byte(execution.ID.String())
	target := cleanupTarget{CleanupExecution: CleanupExecution{ExecutionID: string(id), TerminalAt: *execution.TerminalAt}, complete: true}
	outputRef := ""
	add := func(bucket, key, raw []byte) {
		target.rawRecords = append(target.rawRecords, cleanupRecord{bucket: bucket, key: bytes.Clone(key), raw: bytes.Clone(raw)})
		if raw != nil {
			target.LogicalBytes += int64(len(key) + len(raw))
		}
	}
	err := r.view(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bExecutions).Get(id)
		if !bytes.Equal(raw, executionRaw) {
			return fmt.Errorf("%w: cleanup execution changed", ErrConflict)
		}
		add(bExecutions, id, raw)
		target.Records.Executions = 1
		outcomeRaw := tx.Bucket(bOutcomes).Get(id)
		add(bOutcomes, id, outcomeRaw)
		if raw := outcomeRaw; raw != nil {
			var outcome model.Outcome
			if err := json.Unmarshal(raw, &outcome); err != nil {
				return corrupt(err)
			}
			outputRef = outcome.OutputRef
			target.Records.Outcomes++
		}
		for _, entry := range []struct {
			name  []byte
			count *int
		}{{bAcknowledgements, &target.Records.Acknowledgements}, {bStatusContacts, &target.Records.StatusContacts}, {bTerminal, &target.Records.TerminalIndexes}} {
			var raw []byte
			if bucket := tx.Bucket(entry.name); bucket != nil {
				raw = bucket.Get(id)
			}
			add(entry.name, id, raw)
			if raw != nil {
				*entry.count++

			}
		}
		if nested := tx.Bucket(bEventsByExecution).Bucket(id); nested != nil {
			_, latestID := nested.Cursor().Last()
			target.lastEventID = string(latestID)
		}
		return nil
	})
	if err != nil {
		return target, err
	}
	target.outputRef = outputRef
	outputs, err := diagnostic.OpenOutputs(filepath.Dir(r.path), false)
	if err != nil {
		return target, err
	}
	target.DiagnosticFile, err = outputs.Inspect(string(id), outputRef)
	outputs.Close()
	if err != nil {
		return target, err
	}
	if len(target.rawRecords)+len(mutations) > cleanupPageSize {
		return target, errCleanupDeferred
	}
	for _, record := range mutations {
		target.idempotencyKeys = append(target.idempotencyKeys, record.key)
		add(record.bucket, record.key, record.raw)
		replacement, err := replayTombstone(record.raw)
		if err != nil {
			return target, err
		}
		target.rawRecords[len(target.rawRecords)-1].replacement = replacement
		if replacement == nil {
			target.Records.IdempotencyKeys++
		}
	}
	var after []byte
	retained := []cleanupEvent{}
	retainedBytes := 0
	for {
		page := []cleanupEvent{}
		var last []byte
		done := false
		err := r.view(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket(bEventsByExecution).Bucket(id)
			if bucket == nil {
				done = true
				return nil
			}
			c := bucket.Cursor()
			key, eventID := c.First()
			if after != nil {
				key, eventID = c.Seek(after)
				if bytes.Equal(key, after) {
					key, eventID = c.Next()
				}
			}
			size := 0
			for ; key != nil && len(page) < cleanupPageSize && size < cleanupPageBytes; key, eventID = c.Next() {
				if err := r.ctx.Err(); err != nil {
					return err
				}
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
				page = append(page, cleanupEvent{id: string(eventID), dedupeKey: stored.Event.DedupeKey, sequence: bytes.Clone(key), raw: bytes.Clone(raw), dedupeRaw: bytes.Clone(dedupeRaw), kind: stored.Event.Kind})
				last = bytes.Clone(key)
				size += len(raw) + len(dedupeRaw)
			}
			done = key == nil
			return nil
		})
		if err != nil {
			return target, err
		}
		for _, event := range page {
			// appendEventTx already refuses these four kinds on a terminal.
			// Every other kind can still be replayed, so its event and dedupe
			// record must stay until the execution is removed atomically.
			if event.id == target.lastEventID || !terminalRejectsEvent(event.kind) {
				retained = append(retained, event)
				retainedBytes += len(event.raw) + len(event.dedupeRaw)
				if len(retained) > cleanupEventBatch || (len(retained) > 1 && retainedBytes > cleanupPageBytes) {
					return target, errCleanupDeferred
				}
				continue
			}
			target.events = append(target.events, event)
			if eventLimit > 0 && len(target.events) >= eventLimit {
				target.complete = false
				break
			}
		}
		if !target.complete || done {
			break
		}
		after = last
	}
	// Retain replayable kinds plus the sequence high watermark. They commit
	// with the identity, preserving dedupe for every event a terminal accepts.
	if target.complete && eventLimit > 0 && len(target.events)+len(retained) > eventLimit {
		target.complete = false
	}
	if target.complete {
		target.anchorCount = len(retained)
		target.events = append(target.events, retained...)
	}

	for _, event := range target.events {
		target.Records.Events++
		target.Records.EventIndexes++
		target.Records.EventDedupe++
		target.LogicalBytes += int64(len(event.id) + len(event.raw) + len(event.sequence) + len(event.id) + len(event.dedupeKey) + len(event.dedupeRaw))
	}
	if target.complete && len(target.events) > 0 {
		target.LogicalBytes += int64(len(id))
	}
	if !target.complete {
		target.Partial = true
		target.DiagnosticFile = nil
		// Only event triples are removed by a partial batch. Identity,
		// outcome, terminal index, acknowledgement and mutation keys remain.
		target.Records.Executions = 0
		target.Records.Outcomes = 0
		target.Records.TerminalIndexes = 0
		target.Records.IdempotencyKeys = 0
		target.Records.Acknowledgements = 0
		target.Records.StatusContacts = 0
		for _, record := range target.rawRecords {
			if record.raw != nil {
				target.LogicalBytes -= int64(len(record.key) + len(record.raw))
			}
		}
	}
	if target.complete {
		for _, record := range target.rawRecords {
			if record.replacement != nil {
				target.Records.ReplayTombstones++
				target.LogicalBytes -= int64(len(record.key) + len(record.replacement))
			}
		}
	}
	return target, nil
}
