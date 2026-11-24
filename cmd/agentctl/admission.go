package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/admission"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

func (a *app) admissionStore(c common) (*admission.Store, error) {
	path, err := a.journalPath(c)
	if err != nil {
		return nil, err
	}
	return admission.New(path, a.now, admissionProcessStart), nil
}

func (a *app) nativeAdmission(c common, harness string, argv []string) (*model.ProviderAdmission, admission.Bucket, *output.Error) {
	if harness != "opencode" {
		return nil, admission.Bucket{}, nil
	}
	modelID := route.NativeArgvModel(harness, argv)
	if modelID == "" {
		return nil, admission.Bucket{}, nil
	}
	_, profile, problem := a.resolveProfile(c)
	if problem != nil {
		return nil, admission.Bucket{}, nil
	} // Expert run also works without a profile.
	name, bucket := profile.Admission.Match(harness, modelID)
	if name == "" || !bucket.Active() {
		return nil, admission.Bucket{}, nil
	}
	priority := a.getenv(admission.PriorityEnv)
	if priority == "" {
		priority = admission.Background
	}
	if !admission.ValidPriority(priority) {
		return nil, bucket, delegateError(output.CodeUsage, "admission_invalid_priority", "AGENTCTL_ADMISSION_PRIORITY must be interactive or background")
	}
	return &model.ProviderAdmission{Bucket: name, Priority: priority, Phase: "planned"}, bucket, nil
}

func admissionWarnings(meta *model.ProviderAdmission) []output.Warning {
	if meta == nil || meta.Warning == "" {
		return nil
	}
	var message string
	switch meta.Warning {
	case "admission_max_wait":
		message = "Admission reached max_wait. The native launch took a counted over-window slot."
	case "admission_state_unavailable":
		message = "Admission slot state was unavailable. The native launch proceeded without a slot."
	case "admission_lock_timeout":
		message = "Admission used the configured static fallback after a bucket lock timeout."
	case "admission_state_write_failed":
		message = "Admission used the configured static fallback after a controller write failed."
	default:
		message = "Admission used the configured static fallback window."
	}
	return []output.Warning{{Code: meta.Warning, Message: message, Details: map[string]any{"bucket": meta.Bucket}}}
}

func (a *app) waitProviderAdmission(ctx context.Context, c common, execution model.Execution, b admission.Bucket,
	inbox *cancelInbox, interrupt context.CancelFunc, timeout time.Duration, noStore bool) (model.Execution, *admission.Queue, *output.Error) {
	waitCtx, cancel := context.WithTimeout(ctx, b.Wait())
	defer cancel()
	meta := *execution.Admission
	slots, stateErr := a.admissionStore(c)
	var queue *admission.Queue
	if stateErr == nil {
		pid := os.Getpid()
		start, err := admissionProcessStart(pid)
		if err != nil || start == "" {
			return execution, nil, delegateError(output.CodeDependencyUnavailable, "admission_identity_unavailable", "could not establish the native admission owner").
				WithDetail("execution_id", execution.ID.String()).WithDetail("native_launch_started", false)
		}
		owner := admission.Owner{PID: pid, Start: start}
		queue = slots.Queue(meta.Bucket, b, execution.ID.String(), meta.Priority, owner, owner, false)
	}
	keep := false
	defer func() {
		if queue != nil && !keep {
			queue.Close()
		}
	}()
	var adjustment *admission.Adjustment
	stateUnavailable := queue == nil
	for !stateUnavailable {
		a.serviceCancelInbox(inbox, interrupt)
		if ctx.Err() != nil {
			break
		}
		attemptCtx := waitCtx
		afterMaxWait := waitCtx.Err() != nil
		var decision admission.Decision
		var err error
		if afterMaxWait {
			// A busy bucket lock is retried with the owner's context after
			// max_wait. Launch requires a counted ticket in usable state.
			attemptCtx = ctx
			decision, err = queue.TryAfterMaxWait(attemptCtx)
		} else {
			decision, err = queue.Try(attemptCtx, true)
		}
		if err == nil {
			adjustment = decision.Adjustment
			meta.Window = decision.Window
			if decision.Warning != "" {
				meta.Warning = decision.Warning
			}
			meta.Position = decision.Position
			if decision.Granted {
				meta.Phase, meta.GrantedAt = "granted", decision.AcquiredAt
				if afterMaxWait {
					meta.Warning = "admission_max_wait"
				}
				break
			}
		} else {
			switch {
			case attemptCtx.Err() != nil:
				continue
			case errors.Is(err, admission.ErrHolderOwned):
				return execution, nil, delegateError(output.CodeConflict, "admission_holder_conflict", "provider admission is owned by another process").
					WithDetail("execution_id", execution.ID.String()).WithDetail("native_launch_started", false)
			case !errors.Is(err, admission.ErrLockBusy):
				stateUnavailable = true
				continue
			}
		}
		if meta.Position != execution.Admission.Position || a.now().Sub(execution.Observation.ObservedAt) >= nativeRunnerHeartbeatEvery {
			refreshCtx, stop := context.WithTimeout(attemptCtx, admission.PollInterval)
			updated, problem := a.updateProviderAdmission(refreshCtx, c, execution, meta, false, 0)
			expired := refreshCtx.Err() != nil
			stop()
			if problem == nil {
				execution = updated
			} else if !expired && !problem.Retryable {
				return execution, nil, problem
			}
		}
		a.journalAdmissionAdjustment(attemptCtx, c, execution, adjustment)
		adjustment = nil
		select {
		case <-attemptCtx.Done():
		case <-time.After(admission.PollInterval):
		}
	}
	a.serviceCancelInbox(inbox, interrupt)
	if err := ctx.Err(); err != nil {
		queue.Close()
		cancelled, problem := a.cancelProviderAdmission(c, execution, meta, noStore, err)
		return cancelled, nil, problem
	}
	if meta.Phase != "granted" {
		now := a.now().UTC()
		meta.Phase, meta.Position, meta.GrantedAt, meta.Warning = "bypassed", 0, &now, "admission_state_unavailable"
		if queue != nil {
			queue.Close()
			queue = nil
		}
	}
	execution, problem := a.commitProviderAdmission(ctx, c, execution, meta, timeout, inbox, interrupt)
	if err := ctx.Err(); err != nil {
		queue.Close()
		cancelled, problem := a.cancelProviderAdmission(c, execution, meta, noStore, err)
		return cancelled, nil, problem
	}
	if problem != nil {
		return execution, nil, problem.WithDetail("execution_id", execution.ID.String()).WithDetail("native_launch_started", false)
	}
	a.journalAdmissionAdjustment(ctx, c, execution, adjustment)
	keep = true
	return execution, queue, nil
}

// The reservation decision ends the capacity wait. Its journal commit has the
// normal journal retry budget, with short lock attempts so the owner continues
// to service cancellation before any native child starts.
func (a *app) commitProviderAdmission(ctx context.Context, c common, execution model.Execution, meta model.ProviderAdmission, timeout time.Duration, inbox *cancelInbox, interrupt context.CancelFunc) (model.Execution, *output.Error) {
	budget := c.journalBusyBudget
	if budget <= 0 {
		budget = journalBusyAttemptLock
	}
	writeCtx, stop := context.WithTimeout(ctx, budget)
	defer stop()
	for {
		attemptCtx, cancel := context.WithTimeout(writeCtx, admission.PollInterval)
		current, problem := a.updateProviderAdmission(attemptCtx, c, execution, meta, true, timeout)
		expired := attemptCtx.Err() != nil
		cancel()
		a.serviceCancelInbox(inbox, interrupt)
		if problem == nil || writeCtx.Err() != nil || !expired && !problem.Retryable {
			return current, problem
		}
	}
}

func (a *app) cancelProviderAdmission(c common, execution model.Execution, meta model.ProviderAdmission, noStore bool, cause error) (model.Execution, *output.Error) {
	// The queue ticket is already released. Preserve the outcome when the
	// journal is writable within its configured budget.
	journal, current, problem := a.openExecutionWrite(context.Background(), c, execution.ID)
	if problem != nil {
		return execution, problem
	}
	meta.Phase, meta.Position = "cancelled", 0
	current.Admission = &meta
	final, err := a.finalizeResult(context.Background(), c, journal, current,
		adapter.Result{State: adapter.StateCancelled, Error: "cancelled during provider admission"}, a.now().UTC(), noStore)
	journal.Close()
	if err != nil {
		return execution, mapStoreError("record cancelled provider admission", err)
	}
	a.anxRegisterTerminal(c, final.ID)
	return final, interruptedRunError(cause, final)
}

func (a *app) updateProviderAdmission(ctx context.Context, c common, execution model.Execution, meta model.ProviderAdmission, granted bool, timeout time.Duration) (model.Execution, *output.Error) {
	journal, problem := a.openWriteContext(ctx, c)
	if problem != nil {
		return execution, problem
	}
	defer journal.Close()
	current, err := journal.GetExecution(ctx, execution.ID)
	if err != nil {
		return execution, mapStoreError("read provider admission", err)
	}
	if current.State.Terminal() {
		return current, delegateExecutionError(output.CodeInvalidState, "execution settled during provider admission", current)
	}
	current.Admission = &meta
	now := a.now().UTC()
	current.UpdatedAt, current.Observation.ObservedAt = now, now
	if granted {
		current.SourceState = nil
		if timeout > 0 {
			deadline := meta.GrantedAt.Add(timeout)
			current.DeadlineAt = &deadline
		}
	}
	updated, err := journal.UpdateExecution(ctx, current, current.Revision)
	if err != nil {
		return execution, mapStoreError("record provider admission", err)
	}
	return updated, nil
}

func (a *app) journalAdmissionAdjustment(ctx context.Context, c common, execution model.Execution, adjustment *admission.Adjustment) {
	if adjustment == nil {
		return
	}
	writeCtx, cancel := context.WithTimeout(ctx, admission.PollInterval)
	defer cancel()
	journal, problem := a.openWriteContext(writeCtx, c)
	if problem != nil {
		return
	}
	defer journal.Close()
	payload := map[string]any{"bucket": execution.Admission.Bucket, "from": adjustment.From, "to": adjustment.To, "reason": adjustment.Reason, "at": adjustment.At}
	key, canonical, err := callback.SemanticDedupeKey(execution.Adapter, 1, payload)
	if err != nil {
		return
	}
	_, _, _ = journal.AppendEvent(writeCtx, model.Event{
		ExecutionID: execution.ID, OriginHostID: execution.OriginHostID, Authority: execution.Authority, Adapter: execution.Adapter,
		Kind: model.EventHealth, Ordering: model.OrderingObservation, ObservedAt: adjustment.At,
		SourceState: stringPointer("admission.window_adjusted"), Payload: payload, DedupeKey: key, DedupeVersion: 1,
	}, canonical)
}

func (a *app) admissionCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	usage := func() *output.Error {
		return delegateError(output.CodeUsage, "admission_invalid_arguments", "see agentctl help admission for command arguments")
	}
	if len(args) == 0 {
		return usage()
	}
	command := args[0]
	fs := flag.NewFlagSet(command, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var name, harness, modelID, holder, priority, format, at string
	var wait time.Duration
	var noWait bool
	var pid int
	var attempts, throttled int64
	if command == "match" {
		fs.StringVar(&harness, "harness", "", "")
		fs.StringVar(&modelID, "model", "", "")
	} else {
		fs.StringVar(&name, "bucket", "", "")
	}
	switch command {
	case "match":
	case "status":
		fs.StringVar(&format, "format", "", "")
	case "acquire":
		fs.StringVar(&holder, "holder", "", "")
		fs.StringVar(&priority, "priority", admission.Background, "")
		fs.IntVar(&pid, "pid", os.Getppid(), "")
		fs.DurationVar(&wait, "wait", 0, "")
		fs.BoolVar(&noWait, "no-wait", false, "")
	case "release":
		fs.StringVar(&holder, "holder", "", "")
	case "observe":
		fs.Int64Var(&attempts, "attempts", -1, "")
		fs.Int64Var(&throttled, "throttled", -1, "")
		fs.StringVar(&at, "at", "", "")
	default:
		return usage()
	}
	if err := fs.Parse(args[1:]); err != nil || fs.NArg() != 0 {
		return usage()
	}
	if command == "match" && (harness == "" || modelID == "") ||
		command != "match" && command != "status" && name == "" ||
		name != "" && !admission.ValidBucket(name) ||
		command == "status" && format != "" && format != "prom" ||
		(command == "acquire" || command == "release") && !admission.ValidHolder(holder) {
		return usage()
	}
	_, profile, problem := a.resolveProfile(c)
	if problem != nil {
		return problem
	}
	write := func(result any, fields ...output.Field) *output.Error {
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "admission." + command, Fields: fields}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write provider admission result", false, err)
		}
		return nil
	}
	if command == "match" {
		name, bucket := profile.Admission.Match(harness, modelID)
		return write(map[string]any{"matched": name != "", "bucket": name, "enabled": name != "" && bucket.Active()}, output.Field{Name: "bucket", Value: name}, output.Field{Name: "enabled", Value: name != "" && bucket.Active()})
	}
	buckets := map[string]admission.Bucket{}
	if profile.Admission != nil {
		buckets = profile.Admission.Buckets
	}
	bucket, present := buckets[name]
	if name != "" && !present && command != "release" {
		return delegateError(output.CodeNotFound, "admission_unknown_bucket", "admission bucket is not configured")
	}
	slots, stateErr := a.admissionStore(c)
	stateProblem := func(err error) *output.Error {
		code, diagnostic := output.CodeDependencyUnavailable, "admission_state_unavailable"
		if errors.Is(err, admission.ErrHolderOwned) {
			code, diagnostic = output.CodeConflict, "admission_holder_conflict"
		}
		if ctx.Err() != nil {
			code, diagnostic = output.CodeExecutionCancelled, "admission_cancelled"
		}
		problem := delegateError(code, diagnostic, "provider admission operation could not be completed").WithDetail("bucket", name)
		problem.Retryable = code == output.CodeDependencyUnavailable
		return problem
	}
	if stateErr != nil {
		return stateProblem(stateErr)
	}
	switch command {
	case "status":
		names := []string{}
		for key := range buckets {
			if name == "" || key == name {
				names = append(names, key)
			}
		}
		sort.Strings(names)
		views := make([]admission.Snapshot, 0, len(names))
		lines := []output.Line{}
		for _, key := range names {
			status, err := slots.Status(ctx, key, buckets[key])
			if err != nil {
				if format == "prom" {
					return stateProblem(err)
				}
				status.Warning = "admission_state_unavailable"
			}
			if status.Warning != "" && status.Enabled && format == "" {
				message := "Admission status is using the configured fallback window."
				if status.Warning == "admission_state_missing" {
					message = "This bucket has no controller state yet. The configured fallback window applies."
				}
				renderer = renderer.WithWarnings(output.Warning{Code: status.Warning, Message: message, Details: map[string]any{"bucket": key}})
			}
			views = append(views, status)
			lines = append(lines, output.Line{Lead: "admission.bucket", Fields: []output.Field{
				{Name: "bucket", Value: key}, {Name: "enabled", Value: status.Enabled}, {Name: "window", Value: status.Window}, {Name: "in_use", Value: status.InUse}, {Name: "over_window", Value: status.OverWindow}, {Name: "queued", Value: status.Queued[admission.Interactive] + status.Queued[admission.Background]},
			}})
		}
		if format == "prom" {
			if err := writeAdmissionMetrics(renderer.Writer, views); err != nil {
				return output.Wrap(output.CodeInternal, "write admission metrics", false, err)
			}
			return nil
		}
		if err := renderer.Success(output.Success{Result: map[string]any{"buckets": views}, Lines: lines}); err != nil {
			return output.Wrap(output.CodeInternal, "write admission status", false, err)
		}
		return nil
	case "release":
		released, err := slots.Release(ctx, name, holder)
		if err != nil {
			return stateProblem(err)
		}
		return write(map[string]any{"bucket": name, "released": released}, output.Field{Name: "released", Value: released})
	case "observe":
		now := a.now().UTC()
		observedAt := now
		if at != "" {
			parsed, err := time.Parse(time.RFC3339Nano, at)
			if err != nil {
				return usage()
			}
			observedAt = parsed.UTC()
		}
		sample := admission.Sample{At: observedAt, Attempts: attempts, Throttled: throttled}
		if !bucket.ValidSample(now, sample) {
			return usage()
		}
		adjustment, warning, err := slots.Observe(ctx, name, bucket, sample)
		if err != nil {
			return stateProblem(err)
		}
		if warning != "" {
			renderer = renderer.WithWarnings(output.Warning{Code: warning, Message: "Admission used the configured static fallback window."})
		}
		return write(map[string]any{"bucket": name, "recorded": bucket.Active(), "adjustment": adjustment}, output.Field{Name: "recorded", Value: bucket.Active()})
	case "acquire":
		waitSet := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "wait" {
				waitSet = true
			}
		})
		if !admission.ValidPriority(priority) || pid <= 0 || pid > model.MaxLaunchPID || waitSet && (wait <= 0 || noWait) {
			return usage()
		}
		if !bucket.Active() {
			return write(admission.Decision{Bucket: name, Priority: priority, Granted: true, Reason: "disabled"}, output.Field{Name: "granted", Value: true}, output.Field{Name: "reason", Value: "disabled"})
		}
		if !noWait && !waitSet {
			wait = bucket.Wait()
		}
		wait = min(wait, bucket.Wait())
		start, err := admissionProcessStart(pid)
		self, selfErr := admissionProcessStart(os.Getpid())
		if err != nil || selfErr != nil || start == "" || self == "" {
			return stateProblem(admission.ErrHolderOwned)
		}
		queue := slots.Queue(name, bucket, holder, priority, admission.Owner{PID: os.Getpid(), Start: self}, admission.Owner{PID: pid, Start: start}, true)
		defer queue.Close()
		waitCtx := ctx
		if wait > 0 {
			var cancel context.CancelFunc
			waitCtx, cancel = context.WithTimeout(ctx, wait)
			defer cancel()
		}
		var decision admission.Decision
		for {
			decision, err = queue.Try(waitCtx, wait > 0)
			if ctx.Err() != nil && !decision.Granted {
				return stateProblem(ctx.Err())
			}
			if waitCtx.Err() != nil && !decision.Granted {
				decision.Reason = "wait_timeout"
				break
			}
			if err != nil && (wait == 0 || !errors.Is(err, admission.ErrLockBusy)) {
				return stateProblem(err)
			}
			if decision.Granted || wait == 0 {
				break
			}
			select {
			case <-waitCtx.Done():
			case <-time.After(admission.PollInterval):
			}
		}
		if decision.Warning != "" {
			renderer = renderer.WithWarnings(output.Warning{Code: decision.Warning, Message: "Admission used the configured static fallback window."})
		}
		return write(decision, output.Field{Name: "bucket", Value: name}, output.Field{Name: "granted", Value: decision.Granted}, output.Field{Name: "reason", Value: decision.Reason})
	}
	return usage()
}

func writeAdmissionMetrics(w io.Writer, views []admission.Snapshot) error {
	host, err := os.Hostname()
	if err != nil {
		return err
	}
	type metric struct {
		name, kind string
		value      func(admission.Snapshot) string
	}
	number := func(v float64) string { return strconv.FormatFloat(v, 'g', -1, 64) }
	metrics := []metric{
		{"window", "gauge", func(v admission.Snapshot) string { return strconv.Itoa(v.Window) }},
		{"in_use", "gauge", func(v admission.Snapshot) string { return strconv.Itoa(v.InUse) }},
		{"queued", "gauge", func(v admission.Snapshot) string {
			return strconv.Itoa(v.Queued[admission.Interactive] + v.Queued[admission.Background])
		}},
		{"throttle_rate", "gauge", func(v admission.Snapshot) string { return number(v.ThrottleRate) }},
		{"goodput_rpm", "gauge", func(v admission.Snapshot) string { return number(v.GoodputRPM) }},
		{"admitted_total", "counter", func(v admission.Snapshot) string { return strconv.FormatInt(v.Admitted, 10) }},
		{"throttled_attempts_total", "counter", func(v admission.Snapshot) string { return strconv.FormatInt(v.ThrottledAttempts, 10) }},
		{"attempts_total", "counter", func(v admission.Snapshot) string { return strconv.FormatInt(v.TotalAttempts, 10) }},
	}
	for _, m := range metrics {
		if _, err := fmt.Fprintf(w, "# TYPE agentctl_admission_%s %s\n", m.name, m.kind); err != nil {
			return err
		}
		for _, v := range views {
			if !v.Enabled {
				continue
			}
			if _, err := fmt.Fprintf(w, "agentctl_admission_%s{bucket=%q,host=%q} %s\n", m.name, v.Bucket, host, m.value(v)); err != nil {
				return err
			}
		}
	}
	return nil
}

func countAdmissionAttempt(event adapter.Event, sample *admission.Sample) {
	switch event.SourceState {
	case "opencode.step_start":
		sample.Attempts++
	case "opencode.retry", "opencode.error":
		if status, ok := nativeMetadataInt(event.Payload["http_status"]); ok && status == 429 {
			sample.Attempts++
			sample.Throttled++
		}
	}
}

func projectAdmissionPlan(result map[string]any, native any) {
	fields, ok := native.(map[string]any)
	if !ok {
		return
	}
	raw, ok := fields["preflight"].(json.RawMessage)
	if !ok {
		return
	}
	var preflight struct {
		Admission *model.ProviderAdmission `json:"admission"`
	}
	if json.Unmarshal(raw, &preflight) == nil && preflight.Admission != nil {
		result["admission"] = preflight.Admission
	}
}

func admissionTextFields(meta *model.ProviderAdmission) []output.Field {
	fields := []output.Field{{Name: "admission", Value: meta.Phase}, {Name: "bucket", Value: meta.Bucket}, {Name: "priority", Value: meta.Priority}}
	if meta.Position != 0 {
		fields = append(fields, output.Field{Name: "position", Value: meta.Position})
	}
	return fields
}
