package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/knowledgecmd"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/route"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

var version = "0.1.0-dev"

type app struct {
	stdout, stderr io.Writer
	getenv         func(string) string
	now            func() time.Time
}
type common struct {
	mode                                                        output.Mode
	profile, contextFile, configPath, configBundle, journalPath string
	explain                                                     bool
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	os.Exit(newApp().run(ctx, os.Args[1:]))
}
func newApp() *app {
	return &app{stdout: os.Stdout, stderr: os.Stderr, getenv: os.Getenv, now: time.Now}
}

func (a *app) run(ctx context.Context, args []string) int {
	commonArgs, rest, parseErr := a.parseCommon(args)
	renderer := output.Renderer{Mode: commonArgs.mode, Writer: a.stdout}
	if parseErr != nil {
		return a.fail(renderer, output.NewError(output.CodeUsage, parseErr.Error(), false))
	}
	if len(rest) == 0 {
		return a.help(renderer, "")
	}
	if topic, ok := inlineHelpTopic(rest); ok {
		return a.help(renderer, topic)
	}
	var err *output.Error
	switch rest[0] {
	case "help", "--help", "-h":
		topic := strings.Join(rest[1:], " ")
		return a.help(renderer, topic)
	case "version", "--version":
		_ = renderer.Success(output.Success{Result: map[string]any{"version": version}, Lines: []output.Line{{Lead: "agentctl", Fields: []output.Field{{Name: "version", Value: version}}}}})
		return 0
	case "id":
		err = a.idCommand(renderer, rest[1:])
	case "route":
		err = a.routeCommand(renderer, rest[1:])
	case "doctor":
		err = a.doctor(ctx, renderer, commonArgs, rest[1:])
	case "status":
		err = a.status(ctx, renderer, commonArgs, rest[1:])
	case "events":
		err = a.events(ctx, renderer, commonArgs, rest[1:])
	case "result":
		err = a.result(ctx, renderer, commonArgs, rest[1:])
	case "await":
		err = a.await(ctx, renderer, commonArgs, rest[1:])
	case "run":
		err = a.runNative(ctx, renderer, commonArgs, rest[1:])
	case "attach":
		err = a.attachNative(ctx, renderer, commonArgs, rest[1:])
	case "cancel":
		err = a.cancelNative(ctx, renderer, commonArgs, rest[1:])
	case "subscribe":
		err = a.subscribeCommand(ctx, renderer, commonArgs, rest[1:])
	case "config":
		err = a.configCommand(ctx, renderer, commonArgs, rest[1:])
	case "data":
		err = a.dataCommand(ctx, renderer, commonArgs, rest[1:])
	case "bootstrap":
		err = a.bootstrapCommand(renderer, rest[1:])
	case "supervisor":
		err = a.supervisorCommand(ctx, renderer, commonArgs, rest[1:])
	case "promote":
		err = a.promoteCommand(ctx, renderer, commonArgs, rest[1:])
	case "knowledge", "context":
		err = a.knowledgeCommand(ctx, renderer, rest)
	case "capabilities":
		err = a.capabilitiesCommand(ctx, renderer, commonArgs, rest[1:])
	case "schema":
		err = a.schemaCommand(renderer, rest[1:])
	case "examples":
		err = a.examplesCommand(renderer, rest[1:])
	default:
		err = output.NewError(output.CodeUsage, "unknown command", false).WithDetail("command", rest[0])
	}
	if err != nil {
		if err.Code == output.CodeUsage && len(err.NextActions) == 0 {
			topic := rest[0]
			err.NextActions = append(err.NextActions, output.NextAction{Label: "Discover command usage", Argv: []string{"agentctl", "help", topic}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
		}
		return a.fail(renderer, err)
	}
	return 0
}

func inlineHelpTopic(args []string) (string, bool) {
	if len(args) < 2 || args[len(args)-1] != "--help" && args[len(args)-1] != "-h" {
		return "", false
	}
	for _, arg := range args {
		if arg == "--" {
			return "", false
		}
	}
	return strings.Join(args[:len(args)-1], " "), true
}

func (a *app) parseCommon(args []string) (common, []string, error) {
	// agentctl is an agent-first interface. JSON is the deterministic default;
	// humans can opt into the compact terminal projection with --output text.
	c := common{mode: output.JSON}
	rest := make([]string, 0, len(args))
	command := ""
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if arg == "--" {
			rest = append(rest, args[i:]...)
			break
		}
		take := func() (string, error) {
			if i+1 >= len(args) {
				return "", fmt.Errorf("%s requires a value", arg)
			}
			i++
			return args[i], nil
		}
		switch arg {
		case "--output":
			// `knowledge compile --output <directory>` owns its output flag.
			// Global output selection remains available before the command, which
			// also makes the side-effect boundary obvious to an agent caller.
			if command == "knowledge" {
				rest = append(rest, arg)
				if i+1 >= len(args) {
					return c, nil, fmt.Errorf("%s requires a value", arg)
				}
				i++
				rest = append(rest, args[i])
				continue
			}
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			mode, e := output.ParseMode(v)
			if e != nil {
				return c, nil, e
			}
			c.mode = mode
		case "--profile":
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			c.profile = v
		case "--context-file":
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			c.contextFile = v
		case "--config":
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			c.configPath = v
		case "--config-bundle":
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			c.configBundle = v
		case "--journal":
			v, e := take()
			if e != nil {
				return c, nil, e
			}
			c.journalPath = v
		case "--explain":
			c.explain = true
		default:
			rest = append(rest, arg)
			if command == "" && !strings.HasPrefix(arg, "-") {
				command = arg
			}
		}
	}
	if c.contextFile == "" && a.getenv != nil {
		c.contextFile = strings.TrimSpace(a.getenv("AGENTCTL_CONTEXT_FILE"))
	}
	return c, rest, nil
}

func (a *app) knowledgeCommand(ctx context.Context, renderer output.Renderer, args []string) *output.Error {
	result, textLines, err := knowledgecmd.Run(ctx, args, knowledgecmd.IO{Now: a.now})
	if err != nil {
		return output.Wrap(output.CodeUsage, "knowledge command failed", false, err)
	}
	lines := make([]output.Line, 0, len(textLines))
	for _, line := range textLines {
		lines = append(lines, output.Line{Lead: line})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) idCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, "id requires generate or inspect", false)
	}
	switch args[0] {
	case "generate":
		if len(args) != 2 {
			return output.NewError(output.CodeUsage, "usage: agentctl id generate <type>", false)
		}
		typ := ids.Type(args[1])
		id, err := ids.New(typ)
		if err != nil {
			return output.Wrap(output.CodeUsage, "invalid ID type", false, err).WithDetail("allowed", ids.Types())
		}
		result := map[string]any{"id": id.String(), "type": id.Type(), "payload": id.Payload(), "encoding_version": ids.EncodingVersion, "word_list_digest": ids.WordListDigest()}
		if e := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: id.String(), Fields: []output.Field{{Name: "type", Value: id.Type()}, {Name: "payload", Value: id.Payload()}, {Name: "encoding", Value: ids.EncodingVersion}}}}}); e != nil {
			return output.Wrap(output.CodeInternal, "write output", false, e)
		}
	case "inspect":
		if len(args) != 2 {
			return output.NewError(output.CodeUsage, "usage: agentctl id inspect <id>", false)
		}
		id, err := ids.Parse(args[1])
		if err != nil {
			return output.Wrap(output.CodeUsage, "invalid ID", false, err).WithDetail("ref", args[1])
		}
		result := map[string]any{"id": id.String(), "type": id.Type(), "payload": id.Payload(), "encoding_version": ids.EncodingVersion, "word_list_digest": ids.WordListDigest()}
		if e := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: id.String(), Fields: []output.Field{{Name: "type", Value: id.Type()}, {Name: "payload", Value: id.Payload()}, {Name: "checksum", Value: "valid"}}}}}); e != nil {
			return output.Wrap(output.CodeInternal, "write output", false, e)
		}
	default:
		return output.NewError(output.CodeUsage, "id requires generate or inspect", false)
	}
	return nil
}

func (a *app) routeCommand(renderer output.Renderer, args []string) *output.Error {
	if len(args) == 0 || args[0] != "explain" {
		return output.NewError(output.CodeUsage, "usage: agentctl route explain [flags]", false)
	}
	req := route.Request{}
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--lifecycle":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--lifecycle requires a value", false)
			}
			i++
			req.ExplicitLifecycle = route.Lifecycle(args[i])
		case "--model-family":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--model-family requires a value", false)
			}
			i++
			req.ModelFamily = args[i]
		case "--needs-pr":
			req.NeedsPR = true
		case "--multiple-owners":
			req.MultipleOwners = true
		case "--cross-host-handoff":
			req.CrossHostHandoff = true
		case "--parent-may-exit":
			req.ParentMayExit = true
		case "--review-visibility":
			req.ReviewVisibility = true
		case "--multi-stage":
			req.MultiStage = true
		case "--long-lived-fix-loop":
			req.LongLivedFixLoop = true
		default:
			return output.NewError(output.CodeUsage, "unknown route flag", false).WithDetail("flag", args[i])
		}
	}
	decision, err := route.Explain(req)
	if err != nil {
		return output.Wrap(output.CodeUsage, "cannot explain route", false, err)
	}
	if err := renderer.Success(output.Success{Result: decision, Lines: []output.Line{{Lead: "route", Fields: []output.Field{{Name: "lifecycle", Value: decision.Lifecycle}, {Name: "adapter", Value: decision.Adapter}, {Name: "explicit", Value: decision.Explicit}, {Name: "reasons", Value: decision.Reasons}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) doctor(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	return a.doctorReadiness(ctx, renderer, c, args)
}

func (a *app) status(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl status <execution-id>", false)
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		return mapStoreError("read execution", err)
	}
	return writeExecution(renderer, execution, "status")
}
func (a *app) result(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl result <execution-id> [--summary] [--allow-empty]", false)
	}
	summary, requireContent := false, true
	for _, arg := range args[1:] {
		switch arg {
		case "--summary":
			summary = true
		case "--require-content":
			requireContent = true
		case "--allow-empty":
			requireContent = false
		default:
			return output.NewError(output.CodeUsage, "unknown result flag", false).WithDetail("flag", arg)
		}
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		return mapStoreError("read execution result", err)
	}
	if execution.Observation.Integrity == model.IntegrityConflicted {
		return outcomeError(output.CodeUnknownState, "execution evidence is conflicted", execution)
	}
	if !execution.State.Terminal() {
		return output.NewError(output.CodeInvalidState, "execution is not terminal", false).WithDetail("execution_id", id.String()).WithDetail("state", execution.State)
	}
	outcome, err := journal.GetOutcome(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		recordedAt := execution.UpdatedAt
		if execution.TerminalAt != nil {
			recordedAt = *execution.TerminalAt
		}
		outcome = model.Outcome{SchemaVersion: model.SchemaVersion, ExecutionID: id, Revision: 1, State: execution.State, Availability: model.OutcomeLegacyNotRecorded, RecordedAt: recordedAt, Source: execution.Adapter, ResultRef: fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID)}
	} else if err != nil {
		return mapStoreError("read execution outcome", err)
	}
	if requireContent && outcome.Content == nil {
		return output.NewError(output.CodeNotFound, "execution has no stored result content", false).WithDetail("execution_id", id.String()).WithDetail("availability", outcome.Availability)
	}
	if summary && outcome.Content != nil {
		copy := *outcome.Content
		if copy.Text != copy.Preview {
			copy.Text = copy.Preview
			copy.Truncated = true
			copy.SHA256 = ""
		}
		outcome.Content = &copy
	}
	return writeExecutionOutcome(renderer, execution, outcome)
}

func (a *app) events(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl events <execution-id> [--after-sequence n] [--limit n]", false)
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	query := contracts.EventQuery{}
	for i := 1; i < len(args); i++ {
		if i+1 >= len(args) {
			return output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		value := args[i+1]
		i++
		number, err := strconv.ParseUint(value, 10, 64)
		if err != nil {
			return output.Wrap(output.CodeUsage, "event pagination value must be a non-negative integer", false, err)
		}
		switch args[i-1] {
		case "--after-sequence":
			query.AfterSequence = number
		case "--limit":
			query.Limit = int(number)
		default:
			return output.NewError(output.CodeUsage, "unknown events flag", false).WithDetail("flag", args[i-1])
		}
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	events, err := journal.ListEvents(ctx, id, query)
	if err != nil {
		return mapStoreError("read events", err)
	}
	lines := make([]output.Line, 0, len(events))
	for _, event := range events {
		lines = append(lines, eventLine(event))
	}
	if err := renderer.Success(output.Success{Result: map[string]any{"events": events, "count": len(events)}, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) await(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl await <execution-id> [--timeout duration] [--ignore-attention]", false)
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	timeout := 10 * time.Minute
	stopAttention := true
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--timeout requires a duration", false)
			}
			i++
			value, err := time.ParseDuration(args[i])
			if err != nil || value <= 0 {
				return output.NewError(output.CodeUsage, "timeout must be a positive Go duration", false)
			}
			timeout = value
		case "--stop-on-attention":
			stopAttention = true
		case "--ignore-attention":
			stopAttention = false
		default:
			return output.NewError(output.CodeUsage, "unknown await flag", false).WithDetail("flag", args[i])
		}
	}
	deadline := a.now().Add(timeout)
	for {
		journal, openErr := a.openRead(c)
		if openErr != nil {
			return openErr
		}
		execution, err := journal.GetExecution(ctx, id)
		_ = journal.Close()
		if err != nil {
			return mapStoreError("read awaited execution", err)
		}
		if execution.Observation.Integrity == model.IntegrityConflicted {
			return outcomeError(output.CodeUnknownState, "execution evidence is conflicted", execution)
		}
		switch execution.State {
		case model.StateCompleted:
			return writeExecution(renderer, execution, "await")
		case model.StateFailed:
			return outcomeError(output.CodeExecutionFailed, "execution failed", execution)
		case model.StateCancelled:
			return outcomeError(output.CodeExecutionCancelled, "execution was cancelled", execution)
		case model.StateOrphaned:
			return outcomeError(output.CodeExecutionUnknown, "execution is orphaned", execution)
		case model.StateAttention:
			if stopAttention {
				return outcomeError(output.CodeAttentionRequired, "execution requires attention", execution)
			}
		}
		if !a.now().Before(deadline) {
			return outcomeError(output.CodeTimeout, "await deadline elapsed", execution)
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return output.Wrap(output.CodeTimeout, "await cancelled", true, ctx.Err()).WithDetail("execution_id", id.String())
		case <-timer.C:
		}
	}
}

func (a *app) journalPath(c common) (string, error) {
	if c.journalPath != "" {
		return c.journalPath, nil
	}
	return store.DefaultPath()
}
func (a *app) openRead(c common) (*store.Journal, *output.Error) {
	path, err := a.journalPath(c)
	if err != nil {
		return nil, output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	journal, err := store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		return nil, mapStoreError("open journal", err)
	}
	return journal, nil
}
func parseExecutionRef(value string, c common) (ids.ExecutionID, *output.Error) {
	if strings.HasPrefix(value, "@") {
		if c.contextFile == "" {
			return "", output.NewError(output.CodeNotFound, "contextual reference requires --context-file or AGENTCTL_CONTEXT_FILE", false).WithDetail("ref", value)
		}
		return "", output.NewError(output.CodeCapabilityUnavailable, "context document resolution is not installed", false).WithDetail("ref", value)
	}
	id, err := ids.ParseExecutionID(value)
	if err != nil {
		return "", output.Wrap(output.CodeUsage, "invalid execution reference", false, err).WithDetail("ref", value)
	}
	return id, nil
}
func writeExecution(renderer output.Renderer, e model.Execution, operation string) *output.Error {
	fields := []output.Field{{Name: "state", Value: e.State}, {Name: "authority", Value: e.Authority}, {Name: "adapter", Value: e.Adapter}, {Name: "liveness", Value: e.Liveness}, {Name: "revision", Value: e.Revision}}
	actions := []output.NextAction{}
	if !e.State.Terminal() {
		actions = append(actions, output.NextAction{Label: "Wait for terminal state", Argv: []string{"agentctl", "await", e.ID.String(), "--output", string(renderer.Mode)}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if e.State.Terminal() && operation != "result" {
		actions = append(actions, output.NextAction{Label: "Read terminal result", Argv: []string{"agentctl", "result", e.ID.String(), "--output", string(renderer.Mode)}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	redacted := e
	redacted.CWD = nil
	redacted.Repository = nil
	redacted.SourceBindings = append([]model.SourceBinding(nil), e.SourceBindings...)
	for i := range redacted.SourceBindings {
		redacted.SourceBindings[i].OpaqueID = nil
	}
	if err := renderer.Success(output.Success{Result: redacted, Lines: []output.Line{{Lead: e.ID.String(), Fields: fields}}, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	_ = operation
	return nil
}

func writeExecutionOutcome(renderer output.Renderer, e model.Execution, outcome model.Outcome) *output.Error {
	redacted := e
	redacted.CWD = nil
	redacted.Repository = nil
	redacted.SourceBindings = append([]model.SourceBinding(nil), e.SourceBindings...)
	for i := range redacted.SourceBindings {
		redacted.SourceBindings[i].OpaqueID = nil
	}
	value := struct {
		model.Execution
		Outcome model.Outcome `json:"outcome"`
	}{Execution: redacted, Outcome: outcome}
	fields := []output.Field{{Name: "state", Value: e.State}, {Name: "availability", Value: outcome.Availability}, {Name: "content", Value: outcome.Content != nil}, {Name: "result_ref", Value: outcome.ResultRef}}
	if err := renderer.Success(output.Success{Result: value, Lines: []output.Line{{Lead: e.ID.String(), Fields: fields}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}
func eventLine(e model.Event) output.Line {
	return output.Line{Lead: e.ID.String(), Fields: []output.Field{{Name: "execution", Value: e.ExecutionID}, {Name: "sequence", Value: e.Sequence}, {Name: "kind", Value: e.Kind}, {Name: "state", Value: e.State}, {Name: "ordering", Value: e.Ordering}}}
}
func outcomeError(code output.Code, message string, e model.Execution) *output.Error {
	return output.NewError(code, message, code == output.CodeTimeout || code == output.CodeExecutionUnknown).WithDetail("execution_id", e.ID.String()).WithDetail("state", e.State).WithDetail("revision", e.Revision)
}
func mapStoreError(message string, err error) *output.Error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return output.Wrap(output.CodeNotFound, message, false, err)
	case errors.Is(err, store.ErrConflict), errors.Is(err, store.ErrTerminalConflict):
		return output.Wrap(output.CodeConflict, message, false, err)
	case errors.Is(err, store.ErrPermission):
		return output.Wrap(output.CodeAuthorizationDenied, message, false, err)
	case errors.Is(err, store.ErrReadOnly):
		return output.Wrap(output.CodeInvalidState, message, false, err)
	case errors.Is(err, store.ErrCorrupt):
		return output.Wrap(output.CodeInternal, message, false, err)
	default:
		return output.Wrap(output.CodeInternal, message, false, err)
	}
}
func (a *app) fail(renderer output.Renderer, err *output.Error) int {
	if writeErr := renderer.Failure(err); writeErr != nil {
		_, _ = fmt.Fprintln(a.stderr, "agentctl: failed to write primary error:", writeErr)
		return 70
	}
	return err.ExitCode
}
