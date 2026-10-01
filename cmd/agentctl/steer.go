package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Steering crosses a process boundary: the native child belongs to the
// foreground `run`/`delegate` process, while `agentctl steer` is a separate
// invocation. The two meet in an owner-only spool directory beside the
// journal. A request file carries the message; the journal carries only its
// digest and the delivery outcome. The owner claims a request by rename, so a
// caller that gives up can withdraw an unclaimed request and know it was never
// delivered.
const (
	steerSchemaVersion   = 1
	steerPollEvery       = 200 * time.Millisecond
	steerDefaultTimeout  = 60 * time.Second
	steerClaimedGrace    = 20 * time.Second
	steerRequestSuffix   = ".json"
	steerClaimedSuffix   = ".claimed"
	steerMaxRequestBytes = 8 << 20
)

type steerRequestFile struct {
	SchemaVersion  int       `json:"schema_version"`
	RequestID      string    `json:"request_id"`
	ExecutionID    string    `json:"execution_id"`
	CreatedAt      time.Time `json:"created_at"`
	ExpiresAt      time.Time `json:"expires_at"`
	AllowInterrupt bool      `json:"allow_interrupt"`
	MessageSHA256  string    `json:"message_sha256"`
	Message        string    `json:"message"`
	// MessageBytes stands in for the message once the owner has dropped it.
	MessageBytes int `json:"-"`
}

func steerSpoolDir(journalPath string, id ids.ExecutionID) string {
	return filepath.Join(filepath.Dir(journalPath), "steer", id.String())
}

func executionCapability(execution model.Execution, name adapter.CapabilityName) (model.CapabilityItem, bool) {
	for _, item := range execution.Capabilities.Items {
		if item.Name == string(name) {
			return item, true
		}
	}
	return model.CapabilityItem{}, false
}

// steerInbox is the owning process's side of the spool.
type steerInbox struct {
	dir      string
	nextPoll time.Time
	// settled holds request IDs this owner already delivered or rejected.
	// Concurrent retries of one idempotent request queue separate files; the
	// owner applies the first and drops the rest.
	settled map[string]bool
	// awaiting holds live-input requests that were handed to the native
	// input stream and not yet acknowledged, by input sequence. Only their
	// metadata is kept; the message text is dropped once it is written.
	awaiting map[int]steerRequestFile
}

// openSteerInbox creates the spool directory for an execution whose
// invocation negotiated a steering route. Its existence is the owner's signal
// that requests will be serviced; it is removed when the owner returns.
func (a *app) openSteerInbox(c common, execution model.Execution) *steerInbox {
	item, ok := executionCapability(execution, adapter.CapabilitySteer)
	if !ok || item.Status == model.CapabilityStatus(adapter.CapabilityUnavailable) {
		return nil
	}
	path, err := a.journalPath(c)
	if err != nil {
		return nil
	}
	dir := steerSpoolDir(path, execution.ID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	// MkdirAll honors umask but not an existing wider mode.
	_ = os.Chmod(filepath.Dir(dir), 0o700)
	_ = os.Chmod(dir, 0o700)
	return &steerInbox{dir: dir, settled: map[string]bool{}, awaiting: map[int]steerRequestFile{}}
}

// settleSteerInbox rejects the live-input requests the native session never
// acknowledged. The owner calls it once it has drained the session's last
// events and before it commits the terminal outcome, because a terminal
// execution accepts no further progress events. A message that was written to
// the native input and never taken was not delivered.
func (a *app) settleSteerInbox(c common, id ids.ExecutionID, inbox *steerInbox) {
	if inbox == nil {
		return
	}
	sequences := make([]int, 0, len(inbox.awaiting))
	for sequence := range inbox.awaiting {
		sequences = append(sequences, sequence)
	}
	sort.Ints(sequences)
	for _, sequence := range sequences {
		a.recordSteer(c, id, inbox.awaiting[sequence], "rejected", string(adapter.SteerLiveInput), "steer_unacknowledged", "the native session ended before it took the message", sequence)
	}
	inbox.awaiting = map[int]steerRequestFile{}
}

func (a *app) closeSteerInbox(c common, id ids.ExecutionID, inbox *steerInbox) {
	if inbox == nil {
		return
	}
	a.settleSteerInbox(c, id, inbox)
	_ = os.RemoveAll(inbox.dir)
}

// acknowledgeSteer records delivery for each live-input request whose message
// the native session has now taken.
func (a *app) acknowledgeSteer(c common, id ids.ExecutionID, inbox *steerInbox, events []adapter.Event) {
	if inbox == nil || len(inbox.awaiting) == 0 {
		return
	}
	for _, event := range events {
		if event.SourceState != "input_acknowledged" {
			continue
		}
		sequence := 0
		switch value := event.Payload["input_sequence"].(type) {
		case int:
			sequence = value
		case int64:
			sequence = int(value)
		case float64:
			sequence = int(value)
		}
		request, ok := inbox.awaiting[sequence]
		if !ok {
			continue
		}
		delete(inbox.awaiting, sequence)
		a.recordSteer(c, id, request, "delivered", string(adapter.SteerLiveInput), "", "", sequence)
	}
}

// service delivers queued requests in arrival order. It is called from the
// owner's observation loop, so a delivery never races that loop's own reads of
// the native session.
func (a *app) serviceSteerInbox(ctx context.Context, c common, runtime adapter.Adapter, ref adapter.SourceRef, execution model.Execution, inbox *steerInbox) {
	if inbox == nil || a.now().Before(inbox.nextPoll) {
		return
	}
	inbox.nextPoll = a.now().Add(steerPollEvery)
	entries, err := os.ReadDir(inbox.dir)
	if err != nil {
		return
	}
	names := []string{}
	for _, entry := range entries {
		if strings.HasSuffix(entry.Name(), steerRequestSuffix) {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		pending := filepath.Join(inbox.dir, name)
		claimed := strings.TrimSuffix(pending, steerRequestSuffix) + steerClaimedSuffix
		if err := os.Rename(pending, claimed); err != nil {
			continue // withdrawn by its caller
		}
		request, err := readSteerRequest(claimed, execution.ID)
		if err != nil {
			_ = os.Remove(claimed)
			continue
		}
		if inbox.settled[request.RequestID] {
			_ = os.Remove(claimed)
			continue
		}
		if a.now().After(request.ExpiresAt) {
			inbox.settled[request.RequestID] = true
			a.recordSteer(c, execution.ID, request, "rejected", "", "steer_expired", "request expired before it could be delivered", 0)
			_ = os.Remove(claimed)
			continue
		}
		result, steerErr := runtime.Steer(ctx, adapter.SteerRequest{Ref: ref, Message: []byte(request.Message), AllowInterrupt: request.AllowInterrupt})
		var adapterErr *adapter.AdapterError
		if steerErr != nil && errors.As(steerErr, &adapterErr) && adapterErr.Retryable && adapterErr.Code == adapter.ErrInvalidState {
			// Not deliverable yet: the native session id is still unknown, or
			// its live input has a backlog.
			// Hand the request back so it is retried or withdrawn.
			_ = os.Rename(claimed, pending)
			return
		}
		inbox.settled[request.RequestID] = true
		if steerErr != nil {
			code, reason := "steer_failed", steerErr.Error()
			if adapterErr != nil {
				reason = adapterErr.Message
				if diagnostic, ok := adapterErr.Details["diagnostic_code"].(string); ok {
					code = diagnostic
				} else {
					code = "steer_" + string(adapterErr.Code)
				}
			}
			a.recordSteer(c, execution.ID, request, "rejected", "", code, reason, 0)
		} else if result.Delivery == adapter.SteerLiveInput && result.InputSequence > 0 {
			// Accepted for the native input stream. It is delivered only
			// once the native session acknowledges it.
			request.MessageBytes, request.Message = len(request.Message), ""
			inbox.awaiting[result.InputSequence] = request
			a.recordSteer(c, execution.ID, request, "queued", string(result.Delivery), "", "", result.InputSequence)
		} else {
			a.recordSteer(c, execution.ID, request, "delivered", string(result.Delivery), "", "", 0)
		}
		_ = os.Remove(claimed)
	}
}

func readSteerRequest(path string, id ids.ExecutionID) (steerRequestFile, error) {
	var request steerRequestFile
	file, err := openRegularNoFollow(path)
	if err != nil {
		return request, err
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, steerMaxRequestBytes+1))
	if err != nil {
		return request, err
	}
	if len(body) > steerMaxRequestBytes {
		return request, errors.New("steer request exceeds its size bound")
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return request, err
	}
	if request.SchemaVersion != steerSchemaVersion || request.ExecutionID != id.String() || request.RequestID == "" ||
		len(request.Message) == 0 || len(request.Message) > adapter.MaxSteerMessageBytes || sha256Digest([]byte(request.Message)) != request.MessageSHA256 {
		return request, errors.New("steer request is not valid for this execution")
	}
	return request, nil
}

// recordSteer journals one step of a request: queued (written to a live input
// stream, not yet taken), delivered, or rejected. The payload is metadata
// only: the message is identified by digest and size.
func (a *app) recordSteer(c common, id ids.ExecutionID, request steerRequestFile, status, delivery, diagnostic, reason string, inputSequence int) {
	journal, current, problem := a.openExecutionWriteOwned(c, id)
	if problem != nil {
		return
	}
	defer journal.Close()
	size := request.MessageBytes
	if request.Message != "" {
		size = len(request.Message)
	}
	steer := map[string]any{"request_id": request.RequestID, "status": status, "message_sha256": request.MessageSHA256, "message_bytes": size}
	if delivery != "" {
		steer["delivery"] = delivery
	}
	if inputSequence > 0 {
		steer["input_sequence"] = inputSequence
	}
	if diagnostic != "" {
		steer["diagnostic_code"] = diagnostic
		steer["reason"] = boundedText(reason, 512)
	}
	event, canonical, err := syntheticEvent(current, model.EventProgress, current.State, map[string]any{"steer": steer}, "steer:"+request.RequestID+":"+status, a.now().UTC())
	if err != nil {
		return
	}
	sourceState := "steer_" + status
	event.SourceState = &sourceState
	_, _, _ = journal.AppendEvent(context.Background(), event, canonical)
}

func boundedText(value string, max int) string {
	if len(value) <= max {
		return value
	}
	for max > 0 && !utf8.RuneStart(value[max]) {
		max--
	}
	return value[:max]
}

type steerOptions struct {
	promptFile, idempotencyKey     string
	promptStdin, plan, allowInterr bool
	timeout                        time.Duration
}

func parseSteer(args []string) (string, steerOptions, *output.Error) {
	opts := steerOptions{timeout: steerDefaultTimeout}
	usage := "usage: agentctl steer <execution-id> (--prompt-file path|--prompt-stdin) [--allow-interrupt] [--idempotency-key key] [--timeout duration] [--plan]"
	ref := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--prompt-stdin":
			opts.promptStdin = true
		case "--allow-interrupt":
			opts.allowInterr = true
		case "--plan":
			opts.plan = true
		case "--prompt-file", "--idempotency-key", "--timeout":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return "", opts, output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			switch args[i-1] {
			case "--prompt-file":
				opts.promptFile = args[i]
			case "--idempotency-key":
				opts.idempotencyKey = args[i]
			case "--timeout":
				value, err := time.ParseDuration(args[i])
				if err != nil || value <= 0 {
					return "", opts, output.NewError(output.CodeUsage, "--timeout must be a positive Go duration", false)
				}
				opts.timeout = value
			}
		default:
			if strings.HasPrefix(args[i], "-") || ref != "" {
				return "", opts, output.NewError(output.CodeUsage, usage, false).WithDetail("argument", args[i])
			}
			ref = args[i]
		}
	}
	if ref == "" || boolCount(opts.promptFile != "", opts.promptStdin) != 1 {
		return "", opts, output.NewError(output.CodeUsage, usage, false)
	}
	return ref, opts, nil
}

func (a *app) steerCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	ref, opts, problem := parseSteer(args)
	if problem != nil {
		return problem
	}
	id, problem := parseExecutionRef(ref, c)
	if problem != nil {
		return problem
	}
	prompt, problem := a.loadPrompt(runOptions{promptFile: opts.promptFile, promptStdin: opts.promptStdin, promptDelivery: "argv"})
	if problem != nil {
		return problem
	}
	message := prompt.Bytes
	if len(bytes.TrimSpace(message)) == 0 {
		return output.NewError(output.CodeUsage, "steer message must not be empty", false)
	}
	if len(message) > adapter.MaxSteerMessageBytes {
		return output.NewError(output.CodeUsage, "steer message exceeds 1 MiB limit", false).WithDetail("max_bytes", adapter.MaxSteerMessageBytes)
	}
	if !utf8.Valid(message) || bytes.IndexByte(message, 0) >= 0 {
		return output.NewError(output.CodeUsage, "steer message must be valid UTF-8 without NUL bytes", false)
	}
	execution, baseline, problem := a.readSteerState(ctx, c, id)
	if problem != nil {
		return problem
	}
	withID := func(problem *output.Error) *output.Error {
		return problem.WithDetail("execution_id", id.String()).WithDetail("state", execution.State)
	}
	if execution.Authority != model.AuthorityNative {
		return withID(output.NewError(output.CodeCapabilityUnavailable, "steering is available only for native executions owned by a local agentctl process; Multica owns run input", false).
			WithDetail("authority", execution.Authority))
	}
	messageDigest := sha256Digest(message)
	requestID, err := steerRequestID(id, opts.idempotencyKey, messageDigest)
	if err != nil {
		return output.Wrap(output.CodeInternal, "allocate steer request", false, err)
	}
	replaying := false
	if opts.idempotencyKey != "" && !opts.plan {
		// A retry of the same key and message returns the recorded outcome,
		// even after the execution finished, instead of delivering twice.
		outcome, problem := a.findSteerOutcome(ctx, c, id, 0, requestID)
		if problem != nil {
			return problem
		}
		if outcome != nil && (!steerQueued(outcome) || opts.plan) {
			return writeSteerOutcome(renderer, execution, outcome, nil, true)
		}
		replaying = outcome != nil
	}
	if replaying {
		// The message is already in the native input stream. Wait for it to be
		// taken instead of writing it again.
		return a.awaitSteerOutcome(ctx, renderer, c, execution, 0, requestID, "", a.now().Add(opts.timeout), nil, true)
	}
	if execution.State.Terminal() {
		return withID(output.NewError(output.CodeInvalidState, "terminal execution cannot be steered", false))
	}
	item, declared := executionCapability(execution, adapter.CapabilitySteer)
	if !declared || item.Status == model.CapabilityStatus(adapter.CapabilityUnavailable) {
		unavailable := output.NewError(output.CodeCapabilityUnavailable, "this execution has no steering route", false).WithDetail("adapter", execution.Adapter).
			WithActions(output.NextAction{Label: "Inspect adapter steering routes", Argv: []string{"agentctl", "capabilities", execution.Adapter, "--full"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		if item.Reason != nil {
			unavailable.WithDetail("reason", *item.Reason)
		}
		if required, ok := item.Constraints["required_argv"]; ok {
			unavailable.WithDetail("required_argv", required)
		}
		return withID(unavailable)
	}
	delivery, _ := item.Constraints["delivery"].(string)
	route := map[string]any{"delivery": delivery, "status": item.Status, "constraints": item.Constraints}
	if delivery == string(adapter.SteerInterruptResume) && !opts.allowInterr && !opts.plan {
		return withID(output.NewError(output.CodeCapabilityUnavailable, "this execution can only be steered by interrupting its native process and resuming the session, which discards the in-flight turn", false).
			WithDetail("diagnostic_code", "steer_interrupt_not_permitted").WithDetail("route", route).
			WithActions(output.NextAction{Label: "Permit interrupt-and-resume steering", Argv: []string{"agentctl", "steer", id.String(), "--allow-interrupt", "--prompt-file", "<path>"}, Mutates: true, SideEffectClass: output.ExternalSideEffect, Preconditions: []string{"accept that the in-flight native turn is discarded"}}))
	}
	if opts.plan {
		result := map[string]any{"plan": true, "id": id, "adapter": execution.Adapter, "state": execution.State, "route": route,
			"requires_allow_interrupt": delivery == string(adapter.SteerInterruptResume), "message": map[string]any{"bytes": len(message), "sha256": messageDigest}, "side_effect_class": output.ReadOnly}
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "steer.plan", Fields: []output.Field{{Name: "id", Value: id}, {Name: "delivery", Value: delivery}, {Name: "status", Value: item.Status}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	if fresh := execution.Observation.FreshForSeconds; fresh != nil && a.now().After(execution.Observation.ObservedAt.Add(time.Duration(*fresh)*time.Second+10*time.Second)) {
		return withID(output.NewError(output.CodeExecutionUnknown, "the owning agentctl process has not renewed its lease; nothing was delivered", false))
	}
	path, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	dir := steerSpoolDir(path, id)
	// The owner removes its inbox when it returns, which can happen at any
	// point after the checks above. Report what actually happened to this
	// request rather than a stale state or a filesystem error.
	inboxGone := func() *output.Error {
		var problem *output.Error
		for attempt := 0; attempt < 20; attempt++ {
			var outcome map[string]any
			if outcome, problem = a.findSteerOutcome(ctx, c, id, 0, requestID); outcome != nil {
				return writeSteerOutcome(renderer, execution, outcome, route, true)
			}
			if problem == nil {
				var latest model.Execution
				if latest, _, problem = a.readSteerState(ctx, c, id); problem == nil {
					execution = latest
					break
				}
			}
			time.Sleep(steerPollEvery)
		}
		if problem != nil {
			return problem.WithDetail("execution_id", id.String())
		}
		if execution.State.Terminal() {
			return withID(output.NewError(output.CodeInvalidState, "execution reached a terminal state before the message was delivered; nothing was delivered", false))
		}
		return withID(output.NewError(output.CodeCapabilityUnavailable, "the owning agentctl process is not accepting steering requests", false).WithDetail("diagnostic_code", "steer_inbox_missing"))
	}
	// An execution that is still starting has not opened its inbox yet.
	for wait := time.Duration(0); execution.State == model.StateStarting && wait < 5*time.Second && wait < opts.timeout; wait += steerPollEvery {
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			break
		}
		time.Sleep(steerPollEvery)
		if latest, _, problem := a.readSteerState(ctx, c, id); problem == nil {
			execution = latest
		}
	}
	if info, err := os.Lstat(dir); err != nil || !info.IsDir() {
		return inboxGone()
	}
	now := a.now().UTC()
	request := steerRequestFile{SchemaVersion: steerSchemaVersion, RequestID: requestID, ExecutionID: id.String(), CreatedAt: now, ExpiresAt: now.Add(opts.timeout),
		AllowInterrupt: opts.allowInterr, MessageSHA256: messageDigest, Message: string(message)}
	body, err := json.Marshal(request)
	if err != nil {
		return output.Wrap(output.CodeInternal, "encode steer request", false, err)
	}
	// The time prefix orders requests; the random suffix keeps two callers
	// retrying one idempotent request in the same instant from sharing a file.
	unique := make([]byte, 6)
	if _, err := rand.Read(unique); err != nil {
		return output.Wrap(output.CodeInternal, "allocate steer request", false, err)
	}
	name := strconv.FormatInt(now.UnixNano(), 10) + "-" + strings.TrimPrefix(requestID, "sha256:")[:16] + "-" + hex.EncodeToString(unique)
	pending := filepath.Join(dir, name+steerRequestSuffix)
	staged := filepath.Join(dir, name+".tmp")
	queueErr := os.WriteFile(staged, body, 0o600)
	if queueErr == nil {
		if queueErr = os.Rename(staged, pending); queueErr != nil {
			_ = os.Remove(staged)
		}
	}
	if queueErr != nil {
		return inboxGone()
	}
	return a.awaitSteerOutcome(ctx, renderer, c, execution, baseline, requestID, pending, now.Add(opts.timeout), route, false)
}

// awaitSteerOutcome waits for the owner to settle one request. pending is the
// caller's queued request file, or empty when the owner already holds the
// request.
func (a *app) awaitSteerOutcome(ctx context.Context, renderer output.Renderer, c common, execution model.Execution, baseline uint64, requestID, pending string, deadline time.Time, route map[string]any, replayed bool) *output.Error {
	id := execution.ID
	withID := func(problem *output.Error) *output.Error {
		return problem.WithDetail("execution_id", id.String()).WithDetail("state", execution.State).WithDetail("request_id", requestID)
	}
	var terminalSince time.Time
	for {
		select {
		case <-ctx.Done():
		case <-time.After(steerPollEvery):
		}
		outcome, _ := a.findSteerOutcome(ctx, c, id, baseline, requestID)
		if latest, _, problem := a.readSteerState(context.WithoutCancel(ctx), c, id); problem == nil {
			execution = latest
		}
		if outcome != nil && !steerQueued(outcome) {
			return writeSteerOutcome(renderer, execution, outcome, route, replayed)
		}
		expired := ctx.Err() != nil || a.now().After(deadline)
		if steerQueued(outcome) {
			// Written to the native input stream; the native session takes it
			// at its next turn boundary, which can be later than this caller
			// is willing to wait.
			if execution.State.Terminal() {
				// The owner settles what it still holds as it returns.
				if terminalSince.IsZero() {
					terminalSince = a.now()
					if execution.TerminalAt != nil {
						terminalSince = *execution.TerminalAt
					}
				}
				if a.now().After(terminalSince.Add(steerClaimedGrace)) || ctx.Err() != nil {
					// The owner records a rejection for every message the
					// session did not take. With no record at all (the
					// execution was terminalized under the owner), delivery is
					// not known either way.
					return withID(output.NewError(output.CodeExecutionUnknown, "the execution ended before the owner recorded whether the native session took the steering message; delivery is unknown", false).
						WithDetail("diagnostic_code", "steer_outcome_unrecorded").
						WithActions(output.NextAction{Label: "Inspect execution events", Argv: []string{"agentctl", "events", id.String()}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}))
				}
				continue
			}
			if expired {
				return writeSteerOutcome(renderer, execution, outcome, route, replayed)
			}
			continue
		}
		if !expired && !execution.State.Terminal() {
			continue
		}
		// Withdrawal and the owner's claim are both renames of the same
		// file, so exactly one of them succeeds.
		if pending != "" && os.Remove(pending) == nil {
			if execution.State.Terminal() {
				return withID(output.NewError(output.CodeInvalidState, "execution reached a terminal state before the message was delivered; nothing was delivered", false))
			}
			return withID(output.NewError(output.CodeTimeout, "the owner did not take the steering request in time; it was withdrawn and nothing was delivered", true))
		}
		if a.now().After(deadline.Add(steerClaimedGrace)) || ctx.Err() != nil {
			return withID(output.NewError(output.CodeExecutionUnknown, "the owner took the steering request but recorded no outcome; delivery is unknown and it was not retried", false).
				WithActions(output.NextAction{Label: "Inspect execution events", Argv: []string{"agentctl", "events", id.String()}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}))
		}
	}
}

func steerRequestID(id ids.ExecutionID, key, messageDigest string) (string, error) {
	if key == "" {
		nonce := make([]byte, 16)
		if _, err := rand.Read(nonce); err != nil {
			return "", err
		}
		key = "nonce:" + hex.EncodeToString(nonce)
	} else {
		key = "key:" + key
	}
	return digestJSON(map[string]string{"execution_id": id.String(), "key": key, "message_sha256": messageDigest})
}

// readSteerState returns the execution and its newest event sequence, which
// bounds the later search for this request's outcome.
func (a *app) readSteerState(ctx context.Context, c common, id ids.ExecutionID) (model.Execution, uint64, *output.Error) {
	journal, problem := a.openRead(c)
	if problem != nil {
		return model.Execution{}, 0, problem
	}
	defer journal.Close()
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		return model.Execution{}, 0, mapStoreError("read execution", err)
	}
	return execution, lastEventSequence(ctx, journal, id), nil
}

func lastEventSequence(ctx context.Context, journal *store.Journal, id ids.ExecutionID) uint64 {
	var last uint64
	for {
		events, err := journal.ListEvents(ctx, id, contracts.EventQuery{AfterSequence: last, Limit: 1000})
		if err != nil || len(events) == 0 {
			return last
		}
		last = events[len(events)-1].Sequence
	}
}

// findSteerOutcome returns the newest journaled record of one request (a
// queued record is followed by its delivery or rejection), or nil when none is
// recorded. A journal that cannot be read is reported as a problem so
// a caller never mistakes "could not look" for "not delivered".
func (a *app) findSteerOutcome(ctx context.Context, c common, id ids.ExecutionID, after uint64, requestID string) (map[string]any, *output.Error) {
	journal, problem := a.openRead(c)
	if problem != nil {
		return nil, problem
	}
	defer journal.Close()
	var newest map[string]any
	for {
		events, err := journal.ListEvents(context.WithoutCancel(ctx), id, contracts.EventQuery{AfterSequence: after, Limit: 1000, Kinds: []model.EventKind{model.EventProgress}})
		if err != nil {
			return nil, mapStoreError("read steering outcome", err)
		}
		for _, event := range events {
			after = event.Sequence
			steer, ok := event.Payload["steer"].(map[string]any)
			if ok && steer["request_id"] == requestID {
				steer["event_id"] = event.ID.String()
				newest = steer
			}
		}
		if len(events) < 1000 {
			return newest, nil
		}
	}
}

func steerQueued(outcome map[string]any) bool {
	return outcome != nil && outcome["status"] == "queued"
}

func writeSteerOutcome(renderer output.Renderer, execution model.Execution, outcome map[string]any, route map[string]any, replayed bool) *output.Error {
	status, _ := outcome["status"].(string)
	if status != "delivered" && status != "queued" {
		code := output.CodeInvalidState
		diagnostic, _ := outcome["diagnostic_code"].(string)
		switch diagnostic {
		case "steer_capability_unavailable":
			code = output.CodeCapabilityUnavailable
		case "steer_usage":
			code = output.CodeUsage
		case "steer_expired":
			code = output.CodeTimeout
		case "steer_resume_failed", "steer_execution_failed", "steer_failed":
			code = output.CodeExecutionFailed
		}
		reason, _ := outcome["reason"].(string)
		return output.NewError(code, "the steering message was not delivered", false).WithDetail("execution_id", execution.ID.String()).WithDetail("state", execution.State).
			WithDetail("diagnostic_code", diagnostic).WithDetail("reason", reason).WithDetail("request_id", outcome["request_id"])
	}
	// delivered is evidence from the native session itself: it acknowledged
	// the message, or it was relaunched with the message as its prompt. queued
	// is only agentctl's own write to the native input stream.
	guarantee := "the native session was resumed with the message as its prompt; it is not proof the agent acted on it"
	if outcome["delivery"] == string(adapter.SteerLiveInput) {
		guarantee = "the native session acknowledged taking the message; it is not proof the agent acted on it"
	}
	actions := []output.NextAction{{Label: "Wait for execution", Argv: []string{"agentctl", "await", execution.ID.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}}}
	if status == "queued" {
		guarantee = "the owner accepted the message for the native session's input stream and the session has not taken it yet; it takes input at its next turn boundary"
		actions = append([]output.NextAction{{Label: "Watch for the steer_delivered or steer_rejected event of this request", Argv: []string{"agentctl", "events", execution.ID.String()}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}}}, actions...)
	}
	result := map[string]any{"id": execution.ID, "state": execution.State, "adapter": execution.Adapter, "steer": outcome, "replayed": replayed, "guarantee": guarantee}
	if route != nil {
		result["route"] = route
	}
	if err := renderer.Success(output.Success{Result: result, NextActions: actions, Lines: []output.Line{{Lead: execution.ID.String(), Fields: []output.Field{{Name: "steer", Value: status}, {Name: "delivery", Value: outcome["delivery"]}, {Name: "state", Value: execution.State}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}
