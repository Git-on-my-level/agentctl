package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
)

var (
	ErrNotFound         = errors.New("journal record not found")
	ErrConflict         = errors.New("journal conflict")
	ErrPermission       = errors.New("unsafe journal permissions")
	ErrReadOnly         = errors.New("journal is read-only")
	ErrCorrupt          = errors.New("journal corruption")
	ErrTerminalConflict = errors.New("contradictory terminal event")
)

var (
	bMetadata          = []byte("metadata")
	bExecutions        = []byte("executions")
	bEvents            = []byte("events")
	bEventsByExecution = []byte("events_by_execution")
	bDedupe            = []byte("event_dedupe")
	bTerminal          = []byte("terminal_events")
	bIdempotency       = []byte("idempotency")
	bOutcomes          = []byte("outcomes")
	keySchema          = []byte("schema_version")
	keyHost            = []byte("origin_host_id")
)

type Options struct {
	ReadOnly    bool
	Generator   ids.Generator
	Clock       func() time.Time
	LockTimeout time.Duration
}

type Journal struct {
	db        *bbolt.DB
	path      string
	readOnly  bool
	generator ids.Generator
	clock     func() time.Time
}

func DefaultPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("AGENTCTL_STATE_HOME")); explicit != "" {
		return filepath.Join(filepath.Clean(explicit), "journal.db"), nil
	}
	if root := strings.TrimSpace(os.Getenv("XDG_STATE_HOME")); root != "" {
		return filepath.Join(filepath.Clean(root), "agentctl", "journal.db"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home directory: %w", err)
	}
	return filepath.Join(home, ".local", "state", "agentctl", "journal.db"), nil
}

func Open(path string, options Options) (*Journal, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("journal path is required")
	}
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, fmt.Errorf("resolve journal path: %w", err)
	}
	if err := rejectSymlinks(absolute); err != nil {
		return nil, err
	}
	root := filepath.Dir(absolute)
	if options.ReadOnly {
		if err := verifyOwnerDirectory(root); err != nil {
			return nil, err
		}
		if err := verifyOwnerFile(absolute); err != nil {
			return nil, err
		}
	} else {
		if err := os.MkdirAll(root, 0o700); err != nil {
			return nil, fmt.Errorf("create state root: %w", err)
		}
		if err := verifyOwnerDirectory(root); err != nil {
			return nil, err
		}
		if _, err := os.Lstat(absolute); err == nil {
			if err := verifyOwnerFile(absolute); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	timeout := options.LockTimeout
	if timeout == 0 {
		timeout = 5 * time.Second
	}
	db, err := bbolt.Open(absolute, 0o600, &bbolt.Options{ReadOnly: options.ReadOnly, Timeout: timeout, NoFreelistSync: false})
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("open journal: %w", err)
	}
	j := &Journal{db: db, path: absolute, readOnly: options.ReadOnly, generator: options.Generator, clock: options.Clock}
	if j.generator == nil {
		j.generator = ids.CryptoGenerator{}
	}
	if j.clock == nil {
		j.clock = time.Now
	}
	if !options.ReadOnly {
		if err := j.initialize(); err != nil {
			_ = db.Close()
			return nil, err
		}
	} else if err := j.validateSchema(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return j, nil
}

func OpenDefault(options Options) (*Journal, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return Open(path, options)
}
func (j *Journal) Close() error {
	if j == nil || j.db == nil {
		return nil
	}
	return j.db.Close()
}
func (j *Journal) Path() string { return j.path }

func (j *Journal) initialize() error {
	return j.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range [][]byte{bMetadata, bExecutions, bEvents, bEventsByExecution, bDedupe, bTerminal, bIdempotency, bOutcomes} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		metadata := tx.Bucket(bMetadata)
		if current := metadata.Get(keySchema); current != nil && string(current) != "1" {
			return fmt.Errorf("%w: unsupported journal schema %q", ErrCorrupt, current)
		}
		if err := metadata.Put(keySchema, []byte("1")); err != nil {
			return err
		}
		if metadata.Get(keyHost) == nil {
			host, err := newHostID(j.generator)
			if err != nil {
				return err
			}
			if err := metadata.Put(keyHost, []byte(host)); err != nil {
				return err
			}
		}
		return nil
	})
}

func (j *Journal) validateSchema() error {
	return j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bMetadata)
		if bucket == nil || string(bucket.Get(keySchema)) != "1" {
			return fmt.Errorf("%w: missing or unsupported journal schema", ErrCorrupt)
		}
		return nil
	})
}

func (j *Journal) HostID(ctx context.Context) (ids.HostID, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	var host ids.HostID
	err := j.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bMetadata).Get(keyHost)
		if raw == nil {
			return fmt.Errorf("%w: missing origin host", ErrCorrupt)
		}
		parsed, err := ids.ParseHostID(string(raw))
		host = parsed
		return err
	})
	return host, err
}

func (j *Journal) CreateExecution(ctx context.Context, execution model.Execution, mutation contracts.MutationKey) (model.Execution, bool, error) {
	if j.readOnly {
		return model.Execution{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return model.Execution{}, false, err
	}
	var result model.Execution
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		if mutation.Enabled() {
			record, err := lookupMutation(tx, mutation)
			if err != nil {
				return err
			}
			if record != nil {
				raw := tx.Bucket(bExecutions).Get([]byte(record.ObjectID))
				if raw == nil {
					return fmt.Errorf("%w: idempotency target missing", ErrCorrupt)
				}
				if err := json.Unmarshal(raw, &result); err != nil {
					return corrupt(err)
				}
				reused = true
				return nil
			}
		}
		host, err := ids.ParseHostID(string(tx.Bucket(bMetadata).Get(keyHost)))
		if err != nil {
			return corrupt(err)
		}
		if execution.ID.IsZero() {
			execution.ID, err = newUniqueExecutionID(tx, j.generator)
			if err != nil {
				return err
			}
		}
		if execution.OriginHostID.IsZero() {
			execution.OriginHostID = host
		} else if execution.OriginHostID != host {
			return fmt.Errorf("%w: origin host does not match journal", ErrConflict)
		}
		now := j.clock().UTC()
		execution.SchemaVersion = model.SchemaVersion
		execution.Revision = 1
		if execution.CreatedAt.IsZero() {
			execution.CreatedAt = now
		}
		if execution.UpdatedAt.IsZero() {
			execution.UpdatedAt = execution.CreatedAt
		}
		if execution.Observation.ObservedAt.IsZero() {
			execution.Observation.ObservedAt = execution.UpdatedAt
		}
		if execution.SourceBindings == nil {
			execution.SourceBindings = []model.SourceBinding{}
		}
		if execution.Supersedes == nil {
			execution.Supersedes = []ids.ExecutionID{}
		}
		if execution.Capabilities.Items == nil {
			execution.Capabilities.Items = []model.CapabilityItem{}
		}
		if err := execution.Validate(); err != nil {
			return fmt.Errorf("execution validation: %w", err)
		}
		key := []byte(execution.ID.String())
		if tx.Bucket(bExecutions).Get(key) != nil {
			return fmt.Errorf("%w: execution ID exists", ErrConflict)
		}
		encoded, err := json.Marshal(execution)
		if err != nil {
			return err
		}
		if err := tx.Bucket(bExecutions).Put(key, encoded); err != nil {
			return err
		}
		if mutation.Enabled() {
			if err := putMutation(tx, mutation, "execution", execution.ID.String()); err != nil {
				return err
			}
		}
		result = execution
		return nil
	})
	return result, reused, err
}

func (j *Journal) GetExecution(ctx context.Context, id ids.ExecutionID) (model.Execution, error) {
	if err := ctx.Err(); err != nil {
		return model.Execution{}, err
	}
	var value model.Execution
	err := j.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bExecutions).Get([]byte(id.String()))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &value); err != nil {
			return corrupt(err)
		}
		return value.Validate()
	})
	return value, err
}

func (j *Journal) UpdateExecution(ctx context.Context, next model.Execution, expectedRevision uint64) (model.Execution, error) {
	if j.readOnly {
		return model.Execution{}, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return model.Execution{}, err
	}
	var result model.Execution
	err := j.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bExecutions)
		raw := bucket.Get([]byte(next.ID.String()))
		if raw == nil {
			return ErrNotFound
		}
		var previous model.Execution
		if err := json.Unmarshal(raw, &previous); err != nil {
			return corrupt(err)
		}
		if previous.Revision != expectedRevision {
			return fmt.Errorf("%w: expected revision %d, current %d", ErrConflict, expectedRevision, previous.Revision)
		}
		next.Revision = expectedRevision + 1
		if next.UpdatedAt.IsZero() {
			next.UpdatedAt = j.clock().UTC()
		}
		if next.Observation.ObservedAt.IsZero() {
			next.Observation.ObservedAt = next.UpdatedAt
		}
		if err := model.ValidateTransition(previous, next); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := bucket.Put([]byte(next.ID.String()), encoded); err != nil {
			return err
		}
		result = next
		return nil
	})
	return result, err
}

func (j *Journal) ListExecutions(ctx context.Context, nonTerminalOnly bool) ([]model.Execution, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	values := []model.Execution{}
	err := j.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket(bExecutions).ForEach(func(_, raw []byte) error {
			var value model.Execution
			if err := json.Unmarshal(raw, &value); err != nil {
				return corrupt(err)
			}
			if err := value.Validate(); err != nil {
				return corrupt(err)
			}
			if !nonTerminalOnly || !value.State.Terminal() {
				values = append(values, value)
			}
			return nil
		})
	})
	sort.Slice(values, func(a, b int) bool {
		if values[a].CreatedAt.Equal(values[b].CreatedAt) {
			return values[a].ID.String() < values[b].ID.String()
		}
		return values[a].CreatedAt.Before(values[b].CreatedAt)
	})
	return values, err
}

func (j *Journal) AppendEvent(ctx context.Context, event model.Event, canonicalProjection []byte) (model.Event, bool, error) {
	if j.readOnly {
		return model.Event{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return model.Event{}, false, err
	}
	if len(canonicalProjection) == 0 {
		return model.Event{}, false, errors.New("canonical semantic projection is required")
	}
	if err := validateEventProjection(event, canonicalProjection); err != nil {
		return model.Event{}, false, err
	}
	var result model.Event
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		var err error
		result, reused, err = j.appendEventTx(tx, event, canonicalProjection)
		return err
	})
	return result, reused, err
}

func validateEventProjection(event model.Event, canonicalProjection []byte) error {
	var projectionValue any
	if err := json.Unmarshal(canonicalProjection, &projectionValue); err != nil {
		return fmt.Errorf("canonical semantic projection: %w", err)
	}
	if err := model.ValidateBoundedMetadata(projectionValue, 64*1024); err != nil {
		return fmt.Errorf("canonical semantic projection: %w", err)
	}
	expectedKey, normalizedProjection, err := callback.SemanticDedupeKey(event.Adapter, uint32(event.DedupeVersion), projectionValue)
	if err != nil {
		return fmt.Errorf("canonical semantic projection: %w", err)
	}
	if !bytes.Equal(normalizedProjection, canonicalProjection) {
		return fmt.Errorf("%w: semantic projection is not canonical JSON", ErrConflict)
	}
	if event.DedupeKey != expectedKey {
		return fmt.Errorf("%w: event dedupe key does not match its semantic projection", ErrConflict)
	}
	return nil
}

func (j *Journal) appendEventTx(tx *bbolt.Tx, event model.Event, canonicalProjection []byte) (model.Event, bool, error) {
	if known := tx.Bucket(bDedupe).Get([]byte(event.DedupeKey)); known != nil {
		var record dedupeRecord
		if err := json.Unmarshal(known, &record); err != nil {
			return model.Event{}, false, corrupt(err)
		}
		digest := projectionDigest(canonicalProjection)
		if record.ProjectionDigest != digest {
			return model.Event{}, false, fmt.Errorf("%w: unequal projections share dedupe key", ErrCorrupt)
		}
		raw := tx.Bucket(bEvents).Get([]byte(record.EventID))
		if raw == nil {
			return model.Event{}, false, fmt.Errorf("%w: dedupe target missing", ErrCorrupt)
		}
		var stored storedEvent
		if err := json.Unmarshal(raw, &stored); err != nil {
			return model.Event{}, false, corrupt(err)
		}
		if err := stored.Event.Validate(); err != nil {
			return model.Event{}, false, corrupt(err)
		}
		return stored.Event, true, nil
	}
	execRaw := tx.Bucket(bExecutions).Get([]byte(event.ExecutionID.String()))
	if execRaw == nil {
		return model.Event{}, false, ErrNotFound
	}
	var execution model.Execution
	if err := json.Unmarshal(execRaw, &execution); err != nil {
		return model.Event{}, false, corrupt(err)
	}
	if execution.State.Terminal() {
		switch event.Kind {
		case model.EventStarted, model.EventProgress, model.EventAttention, model.EventHealth:
			return model.Event{}, false, ErrTerminalConflict
		}
	}
	if event.ID.IsZero() {
		allocated, err := newUniqueEventID(tx, j.generator)
		if err != nil {
			return model.Event{}, false, err
		}
		event.ID = allocated
	}
	if event.OriginHostID.IsZero() {
		event.OriginHostID = execution.OriginHostID
	}
	if event.OriginHostID != execution.OriginHostID || event.Authority != execution.Authority || event.Adapter != execution.Adapter {
		return model.Event{}, false, fmt.Errorf("%w: event provenance disagrees with execution", ErrConflict)
	}
	byExec := tx.Bucket(bEventsByExecution)
	nested, err := byExec.CreateBucketIfNotExists([]byte(event.ExecutionID.String()))
	if err != nil {
		return model.Event{}, false, err
	}
	sequence := uint64(nested.Stats().KeyN) + 1
	if lastKey, _ := nested.Cursor().Last(); lastKey != nil {
		sequence = binary.BigEndian.Uint64(lastKey) + 1
	}
	event.Sequence = sequence
	event.SchemaVersion = model.SchemaVersion
	if event.ObservedAt.IsZero() {
		event.ObservedAt = j.clock().UTC()
	}
	if event.Kind == model.EventTerminal && tx.Bucket(bTerminal).Get([]byte(event.ExecutionID.String())) != nil {
		return model.Event{}, false, ErrTerminalConflict
	}
	if err := event.Validate(); err != nil {
		return model.Event{}, false, fmt.Errorf("event validation: %w", err)
	}
	stored := storedEvent{Event: event, Projection: append([]byte(nil), canonicalProjection...)}
	encoded, err := json.Marshal(stored)
	if err != nil {
		return model.Event{}, false, err
	}
	if err := tx.Bucket(bEvents).Put([]byte(event.ID.String()), encoded); err != nil {
		return model.Event{}, false, err
	}
	var seqKey [8]byte
	binary.BigEndian.PutUint64(seqKey[:], sequence)
	if err := nested.Put(seqKey[:], []byte(event.ID.String())); err != nil {
		return model.Event{}, false, err
	}
	dedupe, err := json.Marshal(dedupeRecord{EventID: event.ID.String(), ProjectionDigest: projectionDigest(canonicalProjection)})
	if err != nil {
		return model.Event{}, false, err
	}
	if err := tx.Bucket(bDedupe).Put([]byte(event.DedupeKey), dedupe); err != nil {
		return model.Event{}, false, err
	}
	if event.Kind == model.EventTerminal {
		if err := tx.Bucket(bTerminal).Put([]byte(event.ExecutionID.String()), []byte(event.ID.String())); err != nil {
			return model.Event{}, false, err
		}
	}
	if err := enqueueMatchingSubscriptionsTx(tx, &event, canonicalProjection, event.ObservedAt, j.generator); err != nil {
		return model.Event{}, false, fmt.Errorf("subscription fan-out: %w", err)
	}
	return event, false, nil
}

// CommitObservedEvent atomically records a nonterminal execution observation,
// its normalized event, and matching callback deliveries. An exact duplicate
// event is reused without advancing the execution revision.
func (j *Journal) CommitObservedEvent(ctx context.Context, next model.Execution, expectedRevision uint64, event model.Event, canonicalProjection []byte) (model.Execution, model.Event, bool, error) {
	if j.readOnly {
		return model.Execution{}, model.Event{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return model.Execution{}, model.Event{}, false, err
	}
	if event.Kind == model.EventTerminal || next.State.Terminal() {
		return model.Execution{}, model.Event{}, false, fmt.Errorf("%w: observed event must be nonterminal", ErrConflict)
	}
	if err := validateEventProjection(event, canonicalProjection); err != nil {
		return model.Execution{}, model.Event{}, false, err
	}
	var storedExecution model.Execution
	var storedEvent model.Event
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		executions := tx.Bucket(bExecutions)
		raw := executions.Get([]byte(next.ID.String()))
		if raw == nil {
			return ErrNotFound
		}
		var previous model.Execution
		if err := json.Unmarshal(raw, &previous); err != nil {
			return corrupt(err)
		}
		if previous.State.Terminal() {
			return ErrTerminalConflict
		}
		if previous.Revision != expectedRevision {
			return fmt.Errorf("%w: expected revision %d, current %d", ErrConflict, expectedRevision, previous.Revision)
		}
		var appendErr error
		storedEvent, reused, appendErr = j.appendEventTx(tx, event, canonicalProjection)
		if appendErr != nil {
			return appendErr
		}
		if reused {
			storedExecution = previous
			return nil
		}
		next.Revision = expectedRevision + 1
		if next.UpdatedAt.IsZero() {
			next.UpdatedAt = j.clock().UTC()
		}
		if next.Observation.ObservedAt.IsZero() {
			next.Observation.ObservedAt = next.UpdatedAt
		}
		if err := model.ValidateTransition(previous, next); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		encoded, err := json.Marshal(next)
		if err != nil {
			return err
		}
		if err := executions.Put([]byte(next.ID.String()), encoded); err != nil {
			return err
		}
		storedExecution = next
		return nil
	})
	return storedExecution, storedEvent, reused, err
}

// CommitTerminalOutcome atomically records terminal execution state, its
// bounded outcome, the terminal event, and matching callback deliveries.
func (j *Journal) CommitTerminalOutcome(ctx context.Context, next model.Execution, expectedRevision uint64, outcome model.Outcome, event model.Event, canonicalProjection []byte) (model.Execution, model.Outcome, model.Event, bool, error) {
	if j.readOnly {
		return model.Execution{}, model.Outcome{}, model.Event{}, false, ErrReadOnly
	}
	if err := ctx.Err(); err != nil {
		return model.Execution{}, model.Outcome{}, model.Event{}, false, err
	}
	if err := outcome.Validate(); err != nil {
		return model.Execution{}, model.Outcome{}, model.Event{}, false, fmt.Errorf("outcome validation: %w", err)
	}
	if err := validateEventProjection(event, canonicalProjection); err != nil {
		return model.Execution{}, model.Outcome{}, model.Event{}, false, err
	}
	var storedExecution model.Execution
	var storedOutcome model.Outcome
	var storedEvent model.Event
	var reused bool
	err := j.db.Update(func(tx *bbolt.Tx) error {
		executions := tx.Bucket(bExecutions)
		raw := executions.Get([]byte(next.ID.String()))
		if raw == nil {
			return ErrNotFound
		}
		var previous model.Execution
		if err := json.Unmarshal(raw, &previous); err != nil {
			return corrupt(err)
		}
		outcomes, err := ensureJournalBucket(tx, bOutcomes)
		if err != nil {
			return err
		}
		if previous.State.Terminal() {
			existing := outcomes.Get([]byte(next.ID.String()))
			if existing == nil {
				return ErrTerminalConflict
			}
			if err := json.Unmarshal(existing, &storedOutcome); err != nil {
				return corrupt(err)
			}
			candidate, _ := json.Marshal(outcome)
			if !bytes.Equal(existing, candidate) {
				return ErrTerminalConflict
			}
			storedExecution = previous
			storedEvent, reused, err = j.appendEventTx(tx, event, canonicalProjection)
			return err
		}
		if previous.Revision != expectedRevision {
			return fmt.Errorf("%w: expected revision %d, current %d", ErrConflict, expectedRevision, previous.Revision)
		}
		next.Revision = expectedRevision + 1
		if err := model.ValidateTransition(previous, next); err != nil {
			return fmt.Errorf("%w: %v", ErrConflict, err)
		}
		outcome.Revision = 1
		expectedResultRef := fmt.Sprintf("agentctl://%s/%s", next.OriginHostID, next.ID)
		if outcome.ExecutionID != next.ID || outcome.State != next.State || outcome.Source != next.Adapter || outcome.ResultRef != expectedResultRef {
			return fmt.Errorf("%w: outcome disagrees with execution", ErrConflict)
		}
		if err := outcome.Validate(); err != nil {
			return fmt.Errorf("outcome validation: %w", err)
		}
		encodedExecution, err := json.Marshal(next)
		if err != nil {
			return err
		}
		encodedOutcome, err := json.Marshal(outcome)
		if err != nil {
			return err
		}
		if err := executions.Put([]byte(next.ID.String()), encodedExecution); err != nil {
			return err
		}
		if err := outcomes.Put([]byte(next.ID.String()), encodedOutcome); err != nil {
			return err
		}
		storedEvent, reused, err = j.appendEventTx(tx, event, canonicalProjection)
		if err != nil {
			return err
		}
		storedExecution, storedOutcome = next, outcome
		return nil
	})
	return storedExecution, storedOutcome, storedEvent, reused, err
}

func (j *Journal) GetOutcome(ctx context.Context, executionID ids.ExecutionID) (model.Outcome, error) {
	if err := ctx.Err(); err != nil {
		return model.Outcome{}, err
	}
	var outcome model.Outcome
	err := j.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket(bOutcomes)
		if bucket == nil {
			return ErrNotFound
		}
		raw := bucket.Get([]byte(executionID.String()))
		if raw == nil {
			return ErrNotFound
		}
		if err := json.Unmarshal(raw, &outcome); err != nil {
			return corrupt(err)
		}
		return outcome.Validate()
	})
	return outcome, err
}

func (j *Journal) GetEvent(ctx context.Context, id ids.EventID) (model.Event, error) {
	if err := ctx.Err(); err != nil {
		return model.Event{}, err
	}
	var event model.Event
	err := j.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket(bEvents).Get([]byte(id.String()))
		if raw == nil {
			return ErrNotFound
		}
		var stored storedEvent
		if err := json.Unmarshal(raw, &stored); err != nil {
			return corrupt(err)
		}
		event = stored.Event
		return event.Validate()
	})
	return event, err
}

func (j *Journal) ListEvents(ctx context.Context, executionID ids.ExecutionID, query contracts.EventQuery) ([]model.Event, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if query.Limit < 0 {
		return nil, errors.New("limit cannot be negative")
	}
	if query.Limit == 0 {
		query.Limit = 100
	}
	if query.Limit > 1000 {
		return nil, errors.New("limit exceeds 1000")
	}
	kinds := map[model.EventKind]bool{}
	for _, kind := range query.Kinds {
		kinds[kind] = true
	}
	events := []model.Event{}
	err := j.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket(bExecutions).Get([]byte(executionID.String())) == nil {
			return ErrNotFound
		}
		nested := tx.Bucket(bEventsByExecution).Bucket([]byte(executionID.String()))
		if nested == nil {
			return nil
		}
		var seek [8]byte
		binary.BigEndian.PutUint64(seek[:], query.AfterSequence+1)
		cursor := nested.Cursor()
		for _, eventID := cursor.Seek(seek[:]); eventID != nil && len(events) < query.Limit; _, eventID = cursor.Next() {
			raw := tx.Bucket(bEvents).Get(eventID)
			if raw == nil {
				return fmt.Errorf("%w: event index target missing", ErrCorrupt)
			}
			var stored storedEvent
			if err := json.Unmarshal(raw, &stored); err != nil {
				return corrupt(err)
			}
			if len(kinds) == 0 || kinds[stored.Event.Kind] {
				events = append(events, stored.Event)
			}
		}
		return nil
	})
	return events, err
}

type storedEvent struct {
	Event      model.Event `json:"event"`
	Projection []byte      `json:"projection"`
}
type dedupeRecord struct {
	EventID          string `json:"event_id"`
	ProjectionDigest string `json:"projection_digest"`
}
type mutationRecord struct {
	Scope       string `json:"scope"`
	Key         string `json:"key"`
	InputDigest string `json:"input_digest"`
	ObjectType  string `json:"object_type"`
	ObjectID    string `json:"object_id"`
}

func lookupMutation(tx *bbolt.Tx, key contracts.MutationKey) (*mutationRecord, error) {
	if key.Scope == "" || key.InputDigest == "" {
		return nil, errors.New("idempotency scope and input digest are required")
	}
	raw := tx.Bucket(bIdempotency).Get([]byte(key.Scope + "\x00" + key.Key))
	if raw == nil {
		return nil, nil
	}
	var record mutationRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		return nil, corrupt(err)
	}
	if record.InputDigest != key.InputDigest {
		return nil, fmt.Errorf("%w: idempotency key reused with different input digest", ErrConflict)
	}
	return &record, nil
}
func putMutation(tx *bbolt.Tx, key contracts.MutationKey, objectType, objectID string) error {
	record := mutationRecord{Scope: key.Scope, Key: key.Key, InputDigest: key.InputDigest, ObjectType: objectType, ObjectID: objectID}
	raw, err := json.Marshal(record)
	if err != nil {
		return err
	}
	return tx.Bucket(bIdempotency).Put([]byte(key.Scope+"\x00"+key.Key), raw)
}
func newHostID(g ids.Generator) (ids.HostID, error) {
	id, err := g.New(ids.TypeHost)
	if err != nil {
		return "", err
	}
	return ids.ParseHostID(id.String())
}
func newUniqueExecutionID(tx *bbolt.Tx, g ids.Generator) (ids.ExecutionID, error) {
	for range 16 {
		id, err := g.New(ids.TypeExecution)
		if err != nil {
			return "", err
		}
		parsed, err := ids.ParseExecutionID(id.String())
		if err != nil {
			return "", err
		}
		if tx.Bucket(bExecutions).Get([]byte(parsed.String())) == nil {
			return parsed, nil
		}
	}
	return "", fmt.Errorf("%w: exhausted execution ID collision retries", ErrConflict)
}
func newUniqueEventID(tx *bbolt.Tx, g ids.Generator) (ids.EventID, error) {
	for range 16 {
		id, err := g.New(ids.TypeEvent)
		if err != nil {
			return "", err
		}
		parsed, err := ids.ParseEventID(id.String())
		if err != nil {
			return "", err
		}
		if tx.Bucket(bEvents).Get([]byte(parsed.String())) == nil {
			return parsed, nil
		}
	}
	return "", fmt.Errorf("%w: exhausted event ID collision retries", ErrConflict)
}
func projectionDigest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}
func corrupt(err error) error { return fmt.Errorf("%w: %v", ErrCorrupt, err) }

func verifyOwnerDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%w: state root is not a directory", ErrPermission)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be 0700 or stricter (is %04o)", ErrPermission, path, info.Mode().Perm())
	}
	return nil
}
func verifyOwnerFile(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return ErrNotFound
		}
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%w: journal must be a regular non-symlink file", ErrPermission)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%w: %s must be 0600 or stricter (is %04o)", ErrPermission, path, info.Mode().Perm())
	}
	return nil
}
func rejectSymlinks(path string) error {
	current := string(filepath.Separator)
	volume := filepath.VolumeName(path)
	rest := strings.TrimPrefix(path, volume+string(filepath.Separator))
	if volume != "" {
		current = volume + string(filepath.Separator)
	}
	for _, part := range strings.Split(rest, string(filepath.Separator)) {
		if part == "" {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: symlink traversal at %s", ErrPermission, current)
		}
	}
	return nil
}

var _ contracts.Journal = (*Journal)(nil)
