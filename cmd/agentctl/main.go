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
	"github.com/Git-on-my-level/agentctl/internal/updatecheck"
)

var version = "0.1.0-dev"

type app struct {
	stdout, stderr  io.Writer
	stdin           io.Reader
	stdinIsTerminal func() bool
	getenv          func(string) string
	now             func() time.Time
	updateNotice    func(context.Context, string, common) *output.Warning
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
	a := &app{stdout: os.Stdout, stderr: os.Stderr, stdin: os.Stdin, stdinIsTerminal: func() bool {
		info, err := os.Stdin.Stat()
		return err == nil && info.Mode()&os.ModeCharDevice != 0
	}, getenv: os.Getenv, now: time.Now}
	a.updateNotice = func(ctx context.Context, currentVersion string, c common) *output.Warning {
		statePath, policyPath, err := updatecheck.DefaultPaths(a.getenv)
		if err != nil {
			return nil
		}
		mode, err := updatecheck.ResolveMode(policyPath, a.getenv)
		if err != nil {
			return nil
		}
		options := updatecheck.Options{CurrentVersion: currentVersion, StatePath: statePath, Getenv: a.getenv}
		if mode == updatecheck.ModeAuto || a.skillsAutoDue(c) {
			if (mode == updatecheck.ModeAuto && updatecheck.Due(options)) || a.skillsAutoDue(c) {
				_ = startUpdateWorker()
			}
			if mode == updatecheck.ModeAuto || mode == updatecheck.ModeOff {
				return nil
			}
		}
		if mode == updatecheck.ModeOff {
			return nil
		}
		notice, _ := updatecheck.Check(ctx, options)
		if notice == nil {
			return nil
		}
		return &output.Warning{Code: "agentctl_update_available", Message: "a newer agentctl release is available; verify its published checksum before installing", Details: map[string]any{"current_version": notice.CurrentVersion, "latest_version": notice.LatestVersion, "release_url": notice.ReleaseURL}}
	}
	return a
}

func updateCheckStatePath(_ common) (string, error) {
	statePath, _, err := updatecheck.DefaultPaths(os.Getenv)
	return statePath, err
}

func (a *app) run(ctx context.Context, args []string) int {
	commonArgs, rest, parseErr := a.parseCommon(args)
	renderer := output.Renderer{Mode: commonArgs.mode, Writer: a.stdout}
	if parseErr != nil {
		return a.fail(renderer, output.NewError(output.CodeUsage, parseErr.Error(), false))
	}
	internalUpdateWorker := len(rest) > 0 && rest[0] == "_update-worker"
	updateCommand := len(rest) > 0 && rest[0] == "update"
	skillsCommand := len(rest) > 0 && rest[0] == "skills"
	if a.updateNotice != nil && !internalUpdateWorker && !updateCommand && !skillsCommand && invocationAllowsAutomaticMaintenance(rest) {
		if warning := a.updateNotice(ctx, version, commonArgs); warning != nil {
			renderer = renderer.WithWarnings(*warning)
		}
	}
	if len(rest) == 0 {
		return a.help(renderer, "")
	}
	if topic, ok := inlineHelpTopic(rest); ok {
		return a.help(renderer, topic)
	}
	var err *output.Error
	switch rest[0] {
	case "_update-worker":
		return a.updateWorker(ctx)
	case "update":
		err = a.updateCommand(ctx, renderer, rest[1:])
	case "help", "--help", "-h":
		topic := strings.Join(rest[1:], " ")
		return a.help(renderer, topic)
	case "version", "--version":
		_ = renderer.Success(output.Success{Result: map[string]any{"version": version}, Lines: []output.Line{{Lead: "agentctl", Fields: []output.Field{{Name: "version", Value: version}}}}})
		return 0
	case "id":
		err = a.idCommand(renderer, rest[1:])
	case "route":
		err = a.routeCommand(renderer, commonArgs, rest[1:])
	case "doctor":
		err = a.doctor(ctx, renderer, commonArgs, rest[1:])
	case "orient":
		err = a.orient(ctx, renderer, commonArgs, rest[1:])
	case "status":
		err = a.status(ctx, renderer, commonArgs, rest[1:])
	case "recent", "list":
		err = a.recent(ctx, renderer, commonArgs, rest[1:])
	case "inbox":
		err = a.inbox(ctx, renderer, commonArgs, rest[1:])
	case "workspace":
		err = a.workspaceCommand(ctx, renderer, commonArgs, rest[1:])
	case "events":
		err = a.events(ctx, renderer, commonArgs, rest[1:])
	case "result":
		err = a.result(ctx, renderer, commonArgs, rest[1:])
	case "await":
		return a.await(ctx, renderer, commonArgs, rest[1:])
	case "run":
		err = a.runNative(ctx, renderer, commonArgs, rest[1:])
	case "fanout":
		err = a.fanout(ctx, renderer, commonArgs, rest[1:])
	case "attach":
		err = a.attachNative(ctx, renderer, commonArgs, rest[1:])
	case "cancel":
		err = a.cancelNative(ctx, renderer, commonArgs, rest[1:])
	case "subscribe":
		err = a.subscribeCommand(ctx, renderer, commonArgs, rest[1:])
	case "config":
		err = a.configCommand(ctx, renderer, commonArgs, rest[1:])
	case "skills":
		err = a.skillsCommand(ctx, renderer, commonArgs, rest[1:])
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

// Automatic release and managed-skill maintenance is separately authorized by
// operator policy, but it still must not make a command advertised as read-only
// perform hidden network or filesystem writes. Run it only alongside commands
// that are about to create external work. Explicit maintenance commands own
// their own side effects and therefore do not need this hook.
func invocationAllowsAutomaticMaintenance(args []string) bool {
	if len(args) == 0 {
		return false
	}
	for _, arg := range args[1:] {
		if arg == "--plan" || arg == "--help" || arg == "-h" {
			return false
		}
	}
	switch args[0] {
	case "run", "fanout", "promote":
		return true
	default:
		return false
	}
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

func (a *app) routeCommand(renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 || args[0] != "explain" {
		return output.NewError(output.CodeUsage, "usage: agentctl route explain <selector...>", false)
	}
	var queryParts []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--":
			queryParts = append(queryParts, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(args[i], "-") {
				return output.NewError(output.CodeUsage, "unknown route flag", false).WithDetail("flag", args[i])
			}
			queryParts = append(queryParts, args[i])
		}
	}
	query := strings.TrimSpace(strings.Join(queryParts, " "))
	if query == "" {
		return output.NewError(output.CodeUsage, "route explain requires a host or model selector", false)
	}
	return a.routeExplainQuery(renderer, c, query)
}

func (a *app) routeExplainQuery(renderer output.Renderer, c common, query string) *output.Error {
	catalog := route.NewCatalog("", nil, nil, "")
	path, err := configPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve route config path", false, err)
	}
	resolution, configErr := config.Resolve(path, c.configBundle)
	if configErr == nil {
		_, profile, resolveErr := resolution.Config.ResolveProfile(c.profile)
		if resolveErr != nil {
			return mapConfigError("resolve route profile", resolveErr)
		}
		catalog = catalogFromProfile(profile)
	} else if !errors.Is(configErr, config.ErrNotFound) || c.configPath != "" || c.configBundle != "" || c.profile != "" {
		return mapConfigError("read route config", configErr)
	}
	result := route.Match(query, catalog)
	fields := []output.Field{{Name: "placement", Value: result.Placement.Mode}}
	if result.Placement.Kind != "" {
		fields = append(fields, output.Field{Name: "kind", Value: result.Placement.Kind})
	}
	if result.Placement.Host != "" {
		fields = append(fields, output.Field{Name: "host", Value: result.Placement.Host})
	}
	lines := []output.Line{{Lead: "route", Fields: fields}}
	for _, host := range result.Hosts {
		lines = append(lines, output.Line{Lead: "host", Fields: []output.Field{{Name: "id", Value: host.ID}}})
	}
	for _, modelHit := range result.Models {
		modelFields := []output.Field{{Name: "adapter", Value: modelHit.Adapter}}
		if modelHit.Model != "" {
			modelFields = append(modelFields, output.Field{Name: "model", Value: modelHit.Model})
		}
		if modelHit.Speed != "" {
			modelFields = append(modelFields, output.Field{Name: "speed", Value: modelHit.Speed})
		}
		lines = append(lines, output.Line{Lead: "model", Fields: modelFields})
	}
	if len(result.Unmatched) != 0 {
		lines = append(lines, output.Line{Lead: "unmatched", Fields: []output.Field{{Name: "tokens", Value: result.Unmatched}}})
	}
	var warnings []output.Warning
	var actions []output.NextAction
	if len(result.Unmatched) != 0 {
		warnings = append(warnings, output.Warning{Code: "route_unmatched_tokens", Message: "unmatched tokens do not influence routing; pass only a short reviewed host/model selector", Details: map[string]any{"tokens": result.Unmatched}})
	}
	switch result.Placement.Mode {
	case "remote":
		warnings = append(warnings, output.Warning{Code: "route_not_dispatched", Message: "route advice is read-only: no Multica issue, remote task, or agentctl execution handle was created", Details: map[string]any{"runtime_verified": false, "tracked_execution": false}})
		actions = append(actions, output.NextAction{Label: "Review routing and remote-authority boundaries", Argv: []string{"agentctl", "help", "route"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	case "need_this_host":
		warnings = append(warnings, output.Warning{Code: "route_this_host_unset", Message: "the configured route catalog does not identify this machine; local versus remote placement is unknown"})
		actions = append(actions, output.NextAction{Label: "Inspect route configuration", Argv: []string{"agentctl", "config", "show"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	case "local":
		actions = append(actions, output.NextAction{Label: "Discover direct run", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, Warnings: warnings, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func catalogFromProfile(profile config.Profile) route.Catalog {
	thisHost, hosts, kind := "", map[string]string(nil), ""
	if profile.Route != nil {
		thisHost = profile.Route.ThisHost
		hosts = profile.Route.Hosts
		if profile.Route.Placement != nil {
			kind = profile.Route.Placement.Kind
		}
	}
	var preferred []route.ModelRecord
	if profile.AgentPreferences != nil {
		for _, item := range profile.AgentPreferences.Preferred {
			preferred = append(preferred, route.ModelRecord{
				Adapter: item.Agent,
				Model:   item.Model,
				Speed:   item.Speed,
				Aliases: route.ParseUseForAliases(item.UseFor),
			})
		}
	}
	return route.NewCatalog(thisHost, hosts, preferred, kind)
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
		return output.NewError(output.CodeUsage, "usage: agentctl result <execution-id> [--content|--summary] [--allow-empty] [--require-result-source source] [--min-result-bytes n]", false)
	}
	summary, contentOnly, requireContent := false, false, true
	requireSource := ""
	minResultBytes := 0
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--summary":
			summary = true
		case "--content":
			contentOnly = true
		case "--require-content":
			requireContent = true
		case "--allow-empty":
			requireContent = false
		case "--require-result-source":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--require-result-source requires a value", false)
			}
			i++
			requireSource = strings.TrimSpace(args[i])
			if requireSource == "" {
				return output.NewError(output.CodeUsage, "--require-result-source cannot be empty", false)
			}
		case "--min-result-bytes":
			if i+1 >= len(args) {
				return output.NewError(output.CodeUsage, "--min-result-bytes requires a value", false)
			}
			i++
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 0 {
				return output.NewError(output.CodeUsage, "--min-result-bytes must be a non-negative integer", false)
			}
			minResultBytes = value
		default:
			return output.NewError(output.CodeUsage, "unknown result flag", false).WithDetail("flag", args[i])
		}
	}
	if summary && contentOnly {
		return output.NewError(output.CodeUsage, "--content and --summary are mutually exclusive", false)
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
	if requireContent && outcome.Content == nil && outcome.Failure == nil {
		return output.NewError(output.CodeNotFound, "execution has no stored result content", false).WithDetail("execution_id", id.String()).WithDetail("availability", outcome.Availability)
	}
	if requireSource != "" {
		if outcome.Content == nil || !resultSourceSatisfies(outcome.Content.Source, requireSource) {
			actual := ""
			if outcome.Content != nil {
				actual = outcome.Content.Source
			}
			return output.NewError(output.CodeInvalidState, "result content source does not satisfy requirement", false).WithDetail("execution_id", id.String()).WithDetail("required_source", requireSource).WithDetail("actual_source", actual)
		}
	}
	if minResultBytes > 0 && (outcome.Content == nil || outcome.Content.Bytes < minResultBytes) {
		actual := 0
		if outcome.Content != nil {
			actual = outcome.Content.Bytes
		}
		return output.NewError(output.CodeInvalidState, "result content is shorter than required", false).WithDetail("execution_id", id.String()).WithDetail("minimum_bytes", minResultBytes).WithDetail("actual_bytes", actual)
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
	_ = journal.Close()
	if contentOnly {
		if outcome.Content == nil {
			return output.NewError(output.CodeNotFound, "execution has no stored text content", false).WithDetail("execution_id", id.String()).WithDetail("availability", outcome.Availability)
		}
		if _, err := io.WriteString(a.stdout, outcome.Content.Text); err != nil {
			return output.Wrap(output.CodeInternal, "write result content", false, err)
		}
		if problem := a.acknowledgeExecution(ctx, c, id, store.AcknowledgementResult); problem != nil {
			_, _ = fmt.Fprintln(a.stderr, "agentctl: result delivered but acknowledgement failed:", problem)
		}
		return nil
	}
	if problem := writeExecutionOutcome(renderer, execution, outcome); problem != nil {
		return problem
	}
	if problem := a.acknowledgeExecution(ctx, c, id, store.AcknowledgementResult); problem != nil {
		_, _ = fmt.Fprintln(a.stderr, "agentctl: result delivered but acknowledgement failed:", problem)
	}
	return nil
}

func resultSourceSatisfies(actual, required string) bool {
	actual = strings.ToLower(strings.TrimSpace(actual))
	required = strings.ToLower(strings.TrimSpace(required))
	if actual == required {
		return true
	}
	return required == "assistant" && (actual == "assistant_message_fallback" || actual == "assistant_terminal_result")
}

func (a *app) events(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, "usage: agentctl events <execution-id> [--after-sequence n] [--limit n] [--kind kind]", false)
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
		flag := args[i]
		value := args[i+1]
		i++
		switch flag {
		case "--after-sequence":
			number, err := strconv.ParseUint(value, 10, 64)
			if err != nil {
				return output.Wrap(output.CodeUsage, "event pagination value must be a non-negative integer", false, err)
			}
			query.AfterSequence = number
		case "--limit":
			number, err := strconv.Atoi(value)
			if err != nil || number < 0 {
				return output.Wrap(output.CodeUsage, "event pagination value must be a non-negative integer", false, err)
			}
			query.Limit = number
		case "--kind":
			kind := model.EventKind(strings.ToLower(strings.TrimSpace(value)))
			switch kind {
			case model.EventStarted, model.EventProgress, model.EventAttention, model.EventArtifact, model.EventHealth, model.EventTerminal, model.EventPromoted, model.EventSuperseded:
				query.Kinds = append(query.Kinds, kind)
			default:
				return output.NewError(output.CodeUsage, "unknown event kind", false).WithDetail("kind", value)
			}
		default:
			return output.NewError(output.CodeUsage, "unknown events flag", false).WithDetail("flag", flag)
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

func (a *app) await(ctx context.Context, renderer output.Renderer, c common, args []string) int {
	if len(args) < 1 {
		return a.fail(renderer, output.NewError(output.CodeUsage, "usage: agentctl await <execution-id> [--timeout duration | --no-timeout | --through-execution-deadline] [--ignore-attention]", false))
	}
	id, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return a.fail(renderer, problem)
	}
	timeout := 10 * time.Minute
	noTimeout := false
	timeoutSet := false
	throughExecutionDeadline := false
	stopAttention := true
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--timeout":
			if i+1 >= len(args) {
				return a.fail(renderer, output.NewError(output.CodeUsage, "--timeout requires a duration", false))
			}
			i++
			value, err := time.ParseDuration(args[i])
			if err != nil || value <= 0 {
				return a.fail(renderer, output.NewError(output.CodeUsage, "timeout must be a positive Go duration", false))
			}
			timeout = value
			timeoutSet = true
		case "--no-timeout":
			noTimeout = true
		case "--through-execution-deadline":
			throughExecutionDeadline = true
		case "--stop-on-attention":
			stopAttention = true
		case "--ignore-attention":
			stopAttention = false
		default:
			return a.fail(renderer, output.NewError(output.CodeUsage, "unknown await flag", false).WithDetail("flag", args[i]))
		}
	}
	if boolCount(timeoutSet, noTimeout, throughExecutionDeadline) > 1 {
		return a.fail(renderer, output.NewError(output.CodeUsage, "--timeout, --no-timeout, and --through-execution-deadline are mutually exclusive", false))
	}
	deadline := time.Time{}
	if !noTimeout && !throughExecutionDeadline {
		deadline = a.now().Add(timeout)
	}
	for {
		journal, openErr := a.openRead(c)
		if openErr != nil {
			return a.fail(renderer, openErr)
		}
		execution, err := journal.GetExecution(ctx, id)
		_ = journal.Close()
		if err != nil {
			return a.fail(renderer, mapStoreError("read awaited execution", err))
		}
		if throughExecutionDeadline && deadline.IsZero() {
			if execution.DeadlineAt == nil {
				return a.fail(renderer, output.NewError(output.CodeCapabilityUnavailable, "execution has no recorded deadline", false).WithDetail("execution_id", id.String()).WithDetail("diagnostic_code", "execution_deadline_unavailable"))
			}
			// Allow bounded cancellation and terminal journaling after the native
			// execution deadline itself elapses.
			deadline = execution.DeadlineAt.Add(10 * time.Second)
		}
		if execution.Observation.Integrity == model.IntegrityConflicted {
			return a.fail(renderer, outcomeError(output.CodeUnknownState, "execution evidence is conflicted", execution))
		}
		switch execution.State {
		case model.StateCompleted:
			if problem := writeExecution(renderer, execution, "await"); problem != nil {
				return a.fail(renderer, problem)
			}
			if problem := a.acknowledgeExecution(ctx, c, id, store.AcknowledgementAwait); problem != nil {
				_, _ = fmt.Fprintln(a.stderr, "agentctl: terminal state delivered but acknowledgement failed:", problem)
			}
			return 0
		case model.StateFailed:
			return a.deliverAwaitError(ctx, renderer, c, id, store.AcknowledgementAwait, outcomeError(output.CodeExecutionFailed, "execution failed", execution))
		case model.StateCancelled:
			return a.deliverAwaitError(ctx, renderer, c, id, store.AcknowledgementAwait, outcomeError(output.CodeExecutionCancelled, "execution was cancelled", execution))
		case model.StateOrphaned:
			return a.deliverAwaitError(ctx, renderer, c, id, store.AcknowledgementAwait, outcomeError(output.CodeExecutionUnknown, "execution is orphaned", execution))
		case model.StateAttention:
			if stopAttention {
				return a.fail(renderer, outcomeError(output.CodeAttentionRequired, "execution requires attention", execution))
			}
		}
		if !deadline.IsZero() && !a.now().Before(deadline) {
			return a.fail(renderer, outcomeError(output.CodeTimeout, "await deadline elapsed", execution))
		}
		timer := time.NewTimer(500 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return a.fail(renderer, output.Wrap(output.CodeExecutionCancelled, "await cancelled", false, ctx.Err()).WithDetail("execution_id", id.String()))
		case <-timer.C:
		}
	}
}

func boolCount(values ...bool) int {
	count := 0
	for _, value := range values {
		if value {
			count++
		}
	}
	return count
}

func (a *app) deliverAwaitError(ctx context.Context, renderer output.Renderer, c common, id ids.ExecutionID, source string, problem *output.Error) int {
	exitCode := a.fail(renderer, problem)
	if exitCode != problem.ExitCode {
		return exitCode
	}
	if acknowledgement := a.acknowledgeExecution(ctx, c, id, source); acknowledgement != nil {
		_, _ = fmt.Fprintln(a.stderr, "agentctl: terminal state delivered but acknowledgement failed:", acknowledgement)
	}
	return exitCode
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
	journal, err := openJournalWithRetry(path, store.Options{ReadOnly: true})
	if err != nil {
		return nil, mapStoreError("open journal", err)
	}
	return journal, nil
}

func (a *app) acknowledgeExecution(ctx context.Context, c common, id ids.ExecutionID, source string) *output.Error {
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	if _, _, err := journal.AcknowledgeExecution(ctx, id, source); err != nil {
		return mapStoreError("acknowledge terminal execution", err)
	}
	return nil
}

func openJournalWithRetry(path string, options store.Options) (*store.Journal, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		journal, openErr := store.Open(path, options)
		if openErr == nil {
			return journal, nil
		}
		err = openErr
		if !errors.Is(openErr, store.ErrBusy) || attempt == 1 {
			break
		}
		time.Sleep(time.Duration(100+time.Now().UnixNano()%250) * time.Millisecond)
	}
	return nil, err
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
	if len(e.Labels) != 0 {
		fields = append(fields, output.Field{Name: "labels", Value: e.Labels})
	}
	actions := []output.NextAction{}
	warnings := []output.Warning{}
	if e.TaskContract != nil {
		fields = append(fields, output.Field{Name: "task_contract", Value: "retained"}, output.Field{Name: "acceptance", Value: "external_required"})
		warnings = append(warnings, taskContractAcceptanceWarning())
	}
	if operation == "run" {
		warnings = append(warnings, output.Warning{Code: "foreground_execution_owned", Message: "foreground run is owned by this process and has no default wall-clock timeout; use --background with recent, await, and result for work that must outlive this shell"})
		actions = append(actions, output.NextAction{Label: "Review run ownership and lifecycle", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if !e.State.Terminal() {
		label := "Wait up to 10 minutes"
		argv := []string{"agentctl", "await", e.ID.String(), "--output", string(renderer.Mode)}
		if e.DeadlineAt != nil {
			label = "Wait through execution deadline"
			argv = append(argv, "--through-execution-deadline")
		}
		actions = append(actions, output.NextAction{Label: label, Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}})
		if operation == "background" {
			actions = append(actions, output.NextAction{Label: "Discover durable callback setup", Argv: []string{"agentctl", "help", "subscribe"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"choose an explicit callback destination and target"}})
		}
	}
	if e.State.Terminal() && operation != "result" {
		actions = append(actions, output.NextAction{Label: "Read terminal result", Argv: []string{"agentctl", "result", e.ID.String(), "--output", string(renderer.Mode)}, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}})
	}
	redacted := e
	redacted.CWD = nil
	redacted.Repository = nil
	redacted.Workspace = nil
	redacted.SourceBindings = append([]model.SourceBinding{}, e.SourceBindings...)
	for i := range redacted.SourceBindings {
		redacted.SourceBindings[i].OpaqueID = nil
	}
	if err := renderer.Success(output.Success{Result: redacted, Lines: []output.Line{{Lead: e.ID.String(), Fields: fields}}, Warnings: warnings, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func writeExecutionOutcome(renderer output.Renderer, e model.Execution, outcome model.Outcome) *output.Error {
	redacted := e
	redacted.CWD = nil
	redacted.Repository = nil
	redacted.Workspace = nil
	redacted.SourceBindings = append([]model.SourceBinding{}, e.SourceBindings...)
	for i := range redacted.SourceBindings {
		redacted.SourceBindings[i].OpaqueID = nil
	}
	value := struct {
		model.Execution
		Outcome model.Outcome `json:"outcome"`
	}{Execution: redacted, Outcome: outcome}
	fields := []output.Field{{Name: "state", Value: e.State}, {Name: "availability", Value: outcome.Availability}, {Name: "content", Value: outcome.Content != nil}, {Name: "result_ref", Value: outcome.ResultRef}}
	warnings := []output.Warning{}
	if e.TaskContract != nil {
		fields = append(fields, output.Field{Name: "task_contract", Value: "retained"}, output.Field{Name: "acceptance", Value: "external_required"})
		warnings = append(warnings, taskContractAcceptanceWarning())
	}
	if err := renderer.Success(output.Success{Result: value, Lines: []output.Line{{Lead: e.ID.String(), Fields: fields}}, Warnings: warnings}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func taskContractAcceptanceWarning() output.Warning {
	return output.Warning{Code: "acceptance_external_required", Message: "the execution state does not prove the task contract's expected artifacts or acceptance; verify them through their named authority"}
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
	case errors.Is(err, store.ErrBusy):
		return output.Wrap(output.CodeDependencyUnavailable, message, true, err).WithDetail("diagnostic_code", "journal_busy")
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
