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

	"github.com/Git-on-my-level/agentctl/internal/config"
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
	mode                                          output.Mode
	profile, contextFile, configPath, journalPath string
	explain                                       bool
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
	var err *output.Error
	switch rest[0] {
	case "help", "--help", "-h":
		topic := ""
		if len(rest) > 1 {
			topic = rest[1]
		}
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
		return a.fail(renderer, err)
	}
	return 0
}

func (a *app) parseCommon(args []string) (common, []string, error) {
	c := common{mode: output.Text}
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

func (a *app) help(renderer output.Renderer, topic string) int {
	commands := []map[string]any{
		{"name": "run", "side_effect_class": "external_side_effect", "status": "available"},
		{"name": "attach", "side_effect_class": "read_only", "status": "available_for_local_bindings"},
		{"name": "status", "side_effect_class": "read_only", "status": "available"},
		{"name": "events", "side_effect_class": "read_only", "status": "available"},
		{"name": "subscribe", "side_effect_class": "local_operational_write", "status": "available"},
		{"name": "await", "side_effect_class": "read_only", "status": "available"},
		{"name": "result", "side_effect_class": "read_only", "status": "available"},
		{"name": "promote", "side_effect_class": "remote_coordination_write", "status": "available_with_multica_client_key"},
		{"name": "cancel", "side_effect_class": "external_side_effect", "status": "adapter_dependent"},
		{"name": "context", "side_effect_class": "local_operational_write", "status": "read_only_without_render"},
		{"name": "knowledge", "side_effect_class": "local_operational_write", "status": "available"},
		{"name": "config", "side_effect_class": "local_operational_write", "status": "available"},
		{"name": "bootstrap status", "side_effect_class": "read_only", "status": "available"},
		{"name": "supervisor", "side_effect_class": "local_operational_write", "status": "available"},
		{"name": "route explain", "side_effect_class": "read_only", "status": "available"},
		{"name": "doctor", "side_effect_class": "read_only", "status": "available"},
		{"name": "id generate", "side_effect_class": "read_only", "status": "available"},
		{"name": "capabilities", "side_effect_class": "read_only", "status": "available"},
		{"name": "schema", "side_effect_class": "read_only", "status": "available"},
		{"name": "examples", "side_effect_class": "read_only", "status": "available"},
	}
	headerFields := []output.Field{{Name: "version", Value: version}}
	if topic != "" {
		headerFields = append(headerFields, output.Field{Name: "topic", Value: topic})
	}
	lines := []output.Line{{Lead: "agentctl", Fields: headerFields}, {Lead: "usage", Fields: []output.Field{{Name: "argv", Value: "agentctl <command> [flags]"}}}}
	for _, item := range commands {
		lines = append(lines, output.Line{Lead: "command", Fields: []output.Field{{Name: "name", Value: item["name"]}, {Name: "side_effect_class", Value: item["side_effect_class"]}, {Name: "status", Value: item["status"]}}})
	}
	_ = renderer.Success(output.Success{Result: map[string]any{"name": "agentctl", "version": version, "topic": topic, "commands": commands, "global_flags": []string{"--output text|json", "--profile <name>", "--context-file <path>", "--explain", "--config <path>", "--journal <path>"}}, Lines: lines})
	return 0
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
	if len(args) != 0 {
		return output.NewError(output.CodeUsage, "doctor takes no positional arguments", false)
	}
	journalPath, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	result := map[string]any{"journal_path": journalPath, "journal_status": "absent", "config_status": "absent"}
	lines := []output.Line{{Lead: "doctor", Fields: []output.Field{{Name: "journal", Value: journalPath}, {Name: "journal_status", Value: "absent"}}}}
	if journal, jErr := store.Open(journalPath, store.Options{ReadOnly: true}); jErr == nil {
		host, hErr := journal.HostID(ctx)
		_ = journal.Close()
		if hErr != nil {
			return mapStoreError("read journal host", hErr)
		}
		result["journal_status"] = "ready"
		result["origin_host_id"] = host
		lines[0].Fields[1].Value = "ready"
		lines[0].Fields = append(lines[0].Fields, output.Field{Name: "origin_host_id", Value: host})
	} else if !errors.Is(jErr, store.ErrNotFound) {
		return mapStoreError("inspect journal", jErr)
	}
	configPath := c.configPath
	if configPath == "" {
		configPath, err = config.DefaultPath()
		if err != nil {
			return output.Wrap(output.CodeInternal, "resolve config path", false, err)
		}
	}
	result["config_path"] = configPath
	if cfg, cErr := config.Load(configPath); cErr == nil {
		name, _, resolveErr := cfg.ResolveProfile(c.profile)
		if resolveErr == nil {
			result["config_status"] = "ready"
			result["profile"] = name
			lines = append(lines, output.Line{Lead: "profile", Fields: []output.Field{{Name: "name", Value: name}, {Name: "status", Value: "ready"}}})
		} else {
			return mapConfigError("profile is unavailable", resolveErr)
		}
	} else if !errors.Is(cErr, config.ErrNotFound) {
		return output.Wrap(output.CodeUsage, "invalid config", false, cErr)
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
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
	if len(args) != 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl result <execution-id>", false)
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
	return writeExecution(renderer, execution, "result")
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
		return output.NewError(output.CodeUsage, "usage: agentctl await <execution-id> [--timeout duration] [--stop-on-attention]", false)
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	timeout := 30 * time.Second
	stopAttention := false
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
