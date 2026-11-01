package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

const (
	cancelRequestSuffix  = ".request"
	cancelClaimedSuffix  = ".claimed"
	cancelDefaultTimeout = time.Minute
)

func cancelSpoolDir(journalPath string, id ids.ExecutionID) string {
	return filepath.Join(filepath.Dir(journalPath), "cancel", id.String())
}

// cancelInbox is the owning process's side of the cancel spool. Only the owner
// holds the native child, so it stops the child and commits the outcome. A
// request is an empty file; its existence is the whole message.
type cancelInbox struct {
	dir      string
	nextPoll time.Time
}

// openCancelInbox creates the spool directory. Its existence is the owner's
// signal that requests will be serviced; it is removed when the owner returns,
// after the terminal outcome is committed.
func (a *app) openCancelInbox(c common, id ids.ExecutionID) *cancelInbox {
	path, err := a.journalPath(c)
	if err != nil {
		return nil
	}
	dir := cancelSpoolDir(path, id)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil
	}
	// MkdirAll honors umask but not an existing wider mode.
	_ = os.Chmod(filepath.Dir(dir), 0o700)
	_ = os.Chmod(dir, 0o700)
	return &cancelInbox{dir: dir}
}

func closeCancelInbox(inbox *cancelInbox) {
	if inbox != nil {
		_ = os.RemoveAll(inbox.dir)
	}
}

// serviceCancelInbox claims every pending request and interrupts the run. The
// owner then takes the same path as a signal: stop the child, drain its events,
// commit one terminal outcome. A result that completed first stays completed.
func (a *app) serviceCancelInbox(inbox *cancelInbox, interrupt context.CancelFunc) {
	if inbox == nil || a.now().Before(inbox.nextPoll) {
		return
	}
	inbox.nextPoll = a.now().Add(steerPollEvery)
	entries, err := os.ReadDir(inbox.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		if !strings.HasSuffix(entry.Name(), cancelRequestSuffix) {
			continue
		}
		pending := filepath.Join(inbox.dir, entry.Name())
		// Withdrawal by the caller is the other rename-or-remove of this file,
		// so exactly one side wins.
		if os.Rename(pending, strings.TrimSuffix(pending, cancelRequestSuffix)+cancelClaimedSuffix) == nil {
			interrupt()
		}
	}
}

// cancelSpoolLive reports whether dir is the owner-only directory an owner made.
func cancelSpoolLive(dir string) bool {
	info, err := os.Lstat(dir)
	return err == nil && info.IsDir() && info.Mode().Perm()&0o077 == 0
}

func orphanIfGoneAction(id ids.ExecutionID) output.NextAction {
	return output.NextAction{Label: "Repair the execution if its owner process is gone", Argv: []string{"agentctl", "cancel", id.String(), "--orphan-if-gone"}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{"the owning agentctl process is gone"}}
}

// cancelThroughOwner asks a live owner in another process to cancel, then waits
// for the outcome that owner commits. It returns handled=false when no owner is
// servicing a spool, so the caller can use the adapter route.
func (a *app) cancelThroughOwner(ctx context.Context, renderer output.Renderer, c common, execution model.Execution, timeout time.Duration) (bool, *output.Error) {
	id := execution.ID
	path, err := a.journalPath(c)
	if err != nil {
		return true, output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	dir := cancelSpoolDir(path, id)
	withID := func(problem *output.Error) *output.Error {
		return problem.WithDetail("execution_id", id.String()).WithDetail("state", execution.State)
	}
	// An owner opens its spool after admission, while the execution is starting.
	deadline := a.now().Add(timeout)
	for !cancelSpoolLive(dir) && execution.State == model.StateStarting {
		if ctx.Err() != nil || a.now().After(deadline) {
			return true, withID(output.NewError(output.CodeTimeout, "the execution is still being admitted; no cancellation was requested", true).
				WithDetail("diagnostic_code", "cancel_owner_not_ready").WithActions(orphanIfGoneAction(id)))
		}
		select {
		case <-ctx.Done():
		case <-time.After(steerPollEvery):
		}
		if latest, problem := a.readExecutionForCancel(ctx, c, id); problem == nil {
			execution = latest
		}
	}
	if execution.State == model.StateCancelled {
		return true, writeExecution(renderer, execution, "cancel")
	}
	if execution.State.Terminal() {
		return true, withID(output.NewError(output.CodeInvalidState, fmt.Sprintf("execution reached %s before the cancellation took effect", execution.State), false))
	}
	if !cancelSpoolLive(dir) {
		return false, nil
	}
	leaseStale := func() bool {
		fresh := execution.Observation.FreshForSeconds
		return fresh != nil && a.now().After(execution.Observation.ObservedAt.Add(time.Duration(*fresh)*time.Second+10*time.Second))
	}
	staleOwner := func() *output.Error {
		return withID(output.NewError(output.CodeExecutionUnknown, "the owning agentctl process has not renewed its lease; cancellation is unconfirmed", false).
			WithDetail("diagnostic_code", "cancel_owner_stale").WithActions(orphanIfGoneAction(id)))
	}
	if leaseStale() {
		return true, staleOwner()
	}
	pending := ""
	file, err := os.CreateTemp(dir, "*"+cancelRequestSuffix)
	switch {
	case err == nil:
		pending = file.Name()
		_ = file.Close()
	case !errors.Is(err, os.ErrNotExist):
		return true, withID(output.Wrap(output.CodeInternal, "write cancellation request", false, err))
	}
	// A missing spool means the owner returned after the checks above; the
	// journal says what it recorded.
	for {
		latest, problem := a.readExecutionForCancel(ctx, c, id)
		if problem == nil {
			execution = latest
			if execution.State == model.StateCancelled {
				return true, writeExecution(renderer, execution, "cancel")
			}
			if execution.State.Terminal() {
				if pending != "" {
					_ = os.Remove(pending)
				}
				return true, withID(output.NewError(output.CodeInvalidState, fmt.Sprintf("execution reached %s before the cancellation took effect", execution.State), false))
			}
			// The owner commits before it removes the spool, so reading the
			// journal first makes a missing spool here a real loss.
			if !cancelSpoolLive(dir) {
				return true, withID(output.NewError(output.CodeExecutionUnknown, "the owning agentctl process exited without recording an outcome", false).
					WithDetail("diagnostic_code", "cancel_owner_gone").WithActions(orphanIfGoneAction(id)))
			}
			// A killed owner leaves its spool behind and stops renewing its lease.
			// One that claimed the request may just be waiting for the child to
			// exit without renewing, so only an unclaimed request proves staleness.
			if leaseStale() && pending != "" && os.Remove(pending) == nil {
				return true, staleOwner()
			}
		}
		if ctx.Err() != nil || a.now().After(deadline) {
			if pending != "" && os.Remove(pending) == nil {
				return true, withID(output.NewError(output.CodeTimeout, "the owner did not take the cancellation request in time; it was withdrawn", true).
					WithDetail("diagnostic_code", "cancel_unclaimed").WithActions(orphanIfGoneAction(id)))
			}
			return true, withID(output.NewError(output.CodeTimeout, "the owner took the cancellation request and has not finished; it is still stopping the native process", true).
				WithDetail("diagnostic_code", "cancel_pending").
				WithActions(output.NextAction{Label: "Wait for the outcome", Argv: []string{"agentctl", "await", id.String()}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}}, orphanIfGoneAction(id)))
		}
		select {
		case <-ctx.Done():
		case <-time.After(steerPollEvery):
		}
	}
}

func (a *app) readExecutionForCancel(ctx context.Context, c common, id ids.ExecutionID) (model.Execution, *output.Error) {
	journal, problem := a.openRead(c)
	if problem != nil {
		return model.Execution{}, problem
	}
	defer journal.Close()
	execution, err := journal.GetExecution(context.WithoutCancel(ctx), id)
	if err != nil {
		return model.Execution{}, mapStoreError("read execution", err)
	}
	return execution, nil
}

func parseCancel(args []string) (ref string, orphanIfGone bool, timeout time.Duration, problem *output.Error) {
	usage := output.NewError(output.CodeUsage, "usage: agentctl cancel <execution-id> [--timeout duration | --orphan-if-gone]", false)
	timeout = cancelDefaultTimeout
	timeoutSet := false
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--orphan-if-gone" && !orphanIfGone:
			orphanIfGone = true
		case args[i] == "--timeout" && !timeoutSet && i+1 < len(args):
			i++
			parsed, err := time.ParseDuration(args[i])
			if err != nil || parsed <= 0 {
				return "", false, 0, output.NewError(output.CodeUsage, "--timeout must be a positive duration", false)
			}
			timeout, timeoutSet = parsed, true
		case ref == "" && !strings.HasPrefix(args[i], "-"):
			ref = args[i]
		default:
			return "", false, 0, usage
		}
	}
	if ref == "" || (orphanIfGone && timeoutSet) {
		return "", false, 0, usage
	}
	return ref, orphanIfGone, timeout, nil
}
