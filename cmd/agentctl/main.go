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
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
	"github.com/Git-on-my-level/agentctl/internal/updatecheck"
)

var version = "0.1.0-dev"

const (
	awaitMulticaReprobeInterval      = 2 * time.Second
	awaitMulticaMaxConsecutiveErrors = 3
)

type app struct {
	stdout, stderr  io.Writer
	stdin           io.Reader
	stdinIsTerminal func() bool
	getenv          func(string) string
	now             func() time.Time
	updateNotice    func(context.Context, string, common) *output.Warning
	// supervisorHealthProbe overrides the owner-only supervisor status RPC that
	// doctor folds into its readiness report. Production leaves it nil and
	// dials the socket; tests inject a bounded fake health response.
	supervisorHealthProbe func(context.Context) (supervisor.Status, error)
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
				_ = startUpdateWorker(c)
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
		return a.updateWorker(ctx, commonArgs)
	case "update":
		err = a.updateCommand(ctx, renderer, commonArgs, rest[1:])
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
	case "dispatch":
		err = a.dispatchCommand(ctx, renderer, commonArgs, rest[1:])
	case "delegate":
		err = a.delegateCommand(ctx, renderer, commonArgs, rest[1:])
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
		err = a.bootstrapCommand(renderer, commonArgs, rest[1:])
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
	case "run", "fanout", "promote", "dispatch":
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
		actions = append(actions, output.NextAction{Label: "Plan tracked Multica dispatch", Argv: []string{"agentctl", "dispatch", "--route", result.Placement.Host + " <model>", "--title", "<title>", "--prompt-file", "<path>", "--idempotency-key", "<stable-key>", "--plan"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"replace placeholders with one reviewed concrete model, title, prompt file, and stable task key"}})
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
	return route.NewCatalog(thisHost, hosts, preferredRecords(profile), kind)
}

func preferredRecords(profile config.Profile) []route.ModelRecord {
	if profile.AgentPreferences == nil {
		return nil
	}
	preferred := make([]route.ModelRecord, 0, len(profile.AgentPreferences.Preferred))
	for _, item := range profile.AgentPreferences.Preferred {
		preferred = append(preferred, route.ModelRecord{
			Adapter: item.Agent,
			Model:   item.Model,
			Speed:   item.Speed,
			Aliases: route.ParseUseForAliases(item.UseFor),
		})
	}
	return preferred
}

func offPolicyRunWarning(c common, adapter string, argv []string) *output.Warning {
	path, err := configPath(c)
	if err != nil {
		return nil
	}
	resolution, err := config.Resolve(path, c.configBundle)
	if err != nil {
		return nil
	}
	_, profile, err := resolution.Config.ResolveProfile(c.profile)
	if err != nil {
		return nil
	}
	records := preferredRecords(profile)
	if len(records) == 0 {
		return nil
	}
	model := route.NativeArgvModel(adapter, argv)
	if model == "" {
		return nil
	}
	if route.InPreferredTable(records, adapter, model) {
		return nil
	}
	return &output.Warning{
		Code:    "off_policy_run",
		Message: "native argv is not in the active profile's agent_preferences table; launching anyway because run remains caller-authoritative",
		Details: map[string]any{"adapter": adapter, "model": model},
	}
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

const (
	defaultUnreconciledResultLimit = 50
	maximumUnreconciledResultLimit = 200
)

type resultOptions struct {
	summary        bool
	contentOnly    bool
	requireContent bool
	requireSource  string
	minResultBytes int
	unreconciled   bool
	labels         []string
	limit          int
}

const resultUsage = "usage: agentctl result <execution-id> [--content|--summary] [--allow-empty] [--require-result-source source] [--min-result-bytes n] | agentctl result --unreconciled [--label name ...] [--limit n] [--summary] [--allow-empty]"

func parseResult(args []string) (resultOptions, string, *output.Error) {
	opts := resultOptions{requireContent: true, limit: defaultUnreconciledResultLimit}
	reference := ""
	limitGiven := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--summary":
			opts.summary = true
		case "--content":
			opts.contentOnly = true
		case "--require-content":
			opts.requireContent = true
		case "--allow-empty":
			opts.requireContent = false
		case "--unreconciled":
			opts.unreconciled = true
		case "--require-result-source":
			if i+1 >= len(args) {
				return opts, "", output.NewError(output.CodeUsage, "--require-result-source requires a value", false)
			}
			i++
			opts.requireSource = strings.TrimSpace(args[i])
			if opts.requireSource == "" {
				return opts, "", output.NewError(output.CodeUsage, "--require-result-source cannot be empty", false)
			}
		case "--min-result-bytes":
			if i+1 >= len(args) {
				return opts, "", output.NewError(output.CodeUsage, "--min-result-bytes requires a value", false)
			}
			i++
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 0 {
				return opts, "", output.NewError(output.CodeUsage, "--min-result-bytes must be a non-negative integer", false)
			}
			opts.minResultBytes = value
		case "--label":
			if i+1 >= len(args) {
				return opts, "", output.NewError(output.CodeUsage, "--label requires a value", false)
			}
			i++
			value := strings.TrimSpace(args[i])
			if !validRunLabel(value) {
				return opts, "", output.NewError(output.CodeUsage, "--label must be an exact valid label", false).WithDetail("label", value)
			}
			opts.labels = append(opts.labels, value)
		case "--limit":
			if i+1 >= len(args) {
				return opts, "", output.NewError(output.CodeUsage, "--limit requires a value", false)
			}
			i++
			value, err := strconv.Atoi(strings.TrimSpace(args[i]))
			if err != nil || value < 1 || value > maximumUnreconciledResultLimit {
				return opts, "", output.NewError(output.CodeUsage, "--limit must be between 1 and 200", false)
			}
			opts.limit = value
			limitGiven = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return opts, "", output.NewError(output.CodeUsage, "unknown result flag", false).WithDetail("flag", args[i])
			}
			if reference != "" {
				return opts, "", output.NewError(output.CodeUsage, "result accepts exactly one execution reference", false).WithDetail("reference", args[i])
			}
			reference = args[i]
		}
	}
	if opts.summary && opts.contentOnly {
		return opts, "", output.NewError(output.CodeUsage, "--content and --summary are mutually exclusive", false)
	}
	if opts.unreconciled {
		// Batch collection is a set operation. A positional reference, raw
		// content streaming, and single-result assertions all describe exactly
		// one execution and would silently change meaning across a set.
		if reference != "" {
			return opts, "", output.NewError(output.CodeUsage, "--unreconciled selects the uncollected set and cannot take an execution reference", false).WithDetail("reference", reference)
		}
		if opts.contentOnly {
			return opts, "", output.NewError(output.CodeUsage, "--content writes one exact result body and cannot be combined with --unreconciled", false)
		}
		if opts.requireSource != "" || opts.minResultBytes > 0 {
			return opts, "", output.NewError(output.CodeUsage, "--require-result-source and --min-result-bytes assert one result and cannot be combined with --unreconciled", false)
		}
		return opts, "", nil
	}
	if len(opts.labels) != 0 || limitGiven {
		return opts, "", output.NewError(output.CodeUsage, "--label and --limit are only valid with --unreconciled", false)
	}
	if reference == "" {
		return opts, "", output.NewError(output.CodeUsage, resultUsage, false)
	}
	return opts, reference, nil
}

func (a *app) result(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) < 1 {
		return output.NewError(output.CodeUsage, resultUsage, false)
	}
	opts, reference, problem := parseResult(args)
	if problem != nil {
		return problem
	}
	if opts.unreconciled {
		return a.resultUnreconciled(ctx, renderer, c, opts)
	}
	id, problem := parseExecutionRef(reference, c)
	if problem != nil {
		return problem
	}
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	execution, outcome, problem := dereferenceResult(ctx, journal, id, opts)
	if problem != nil {
		return problem
	}
	_ = journal.Close()
	if opts.contentOnly {
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

// dereferenceResult is the single-execution result contract. Batch collection
// routes through it unchanged so conflicted evidence, nonterminal state, and
// --allow-empty behave identically whether one id or a set was requested.
func dereferenceResult(ctx context.Context, journal *store.Journal, id ids.ExecutionID, opts resultOptions) (model.Execution, model.Outcome, *output.Error) {
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		return model.Execution{}, model.Outcome{}, mapStoreError("read execution result", err)
	}
	if execution.Observation.Integrity == model.IntegrityConflicted {
		return execution, model.Outcome{}, outcomeError(output.CodeUnknownState, "execution evidence is conflicted", execution)
	}
	if !execution.State.Terminal() {
		return execution, model.Outcome{}, output.NewError(output.CodeInvalidState, "execution is not terminal", false).WithDetail("execution_id", id.String()).WithDetail("state", execution.State)
	}
	outcome, err := journal.GetOutcome(ctx, id)
	if errors.Is(err, store.ErrNotFound) {
		recordedAt := execution.UpdatedAt
		if execution.TerminalAt != nil {
			recordedAt = *execution.TerminalAt
		}
		availability := model.OutcomeLegacyNotRecorded
		if execution.Authority == model.AuthorityMultica {
			availability = model.OutcomeUnavailableAtSource
		}
		outcome = model.Outcome{SchemaVersion: model.SchemaVersion, ExecutionID: id, Revision: 1, State: execution.State, Availability: availability, RecordedAt: recordedAt, Source: execution.Adapter, ResultRef: fmt.Sprintf("agentctl://%s/%s", execution.OriginHostID, execution.ID)}
	} else if err != nil {
		return execution, model.Outcome{}, mapStoreError("read execution outcome", err)
	}
	if opts.requireContent && outcome.Content == nil && outcome.Failure == nil {
		return execution, outcome, output.NewError(output.CodeNotFound, "execution has no stored result content", false).WithDetail("execution_id", id.String()).WithDetail("availability", outcome.Availability)
	}
	if opts.requireSource != "" {
		if outcome.Content == nil || !resultSourceSatisfies(outcome.Content.Source, opts.requireSource) {
			actual := ""
			if outcome.Content != nil {
				actual = outcome.Content.Source
			}
			return execution, outcome, output.NewError(output.CodeInvalidState, "result content source does not satisfy requirement", false).WithDetail("execution_id", id.String()).WithDetail("required_source", opts.requireSource).WithDetail("actual_source", actual)
		}
	}
	if opts.minResultBytes > 0 && (outcome.Content == nil || outcome.Content.Bytes < opts.minResultBytes) {
		actual := 0
		if outcome.Content != nil {
			actual = outcome.Content.Bytes
		}
		return execution, outcome, output.NewError(output.CodeInvalidState, "result content is shorter than required", false).WithDetail("execution_id", id.String()).WithDetail("minimum_bytes", opts.minResultBytes).WithDetail("actual_bytes", actual)
	}
	if opts.summary && outcome.Content != nil {
		bounded := *outcome.Content
		if bounded.Text != bounded.Preview {
			bounded.Text = bounded.Preview
			bounded.Truncated = true
			bounded.SHA256 = ""
		}
		outcome.Content = &bounded
	}
	return execution, outcome, nil
}

type unreconciledResultItem struct {
	ExecutionID  string          `json:"execution_id"`
	Labels       []string        `json:"labels"`
	Adapter      string          `json:"adapter"`
	Authority    model.Authority `json:"authority"`
	State        model.State     `json:"state"`
	TerminalAt   *time.Time      `json:"terminal_at,omitempty"`
	Status       string          `json:"status"`
	Acknowledged bool            `json:"acknowledged"`
	Outcome      *model.Outcome  `json:"outcome,omitempty"`
	SkipReason   string          `json:"skip_reason,omitempty"`
	SkipMessage  string          `json:"skip_message,omitempty"`
}

// resultUnreconciled collects the terminal executions whose result nobody has
// dereferenced. It is a bounded loop over the single-id path, not a second
// result contract: one unreadable execution is reported and skipped so a single
// conflicted item cannot strand the rest of a backlog.
func (a *app) resultUnreconciled(ctx context.Context, renderer output.Renderer, c common, opts resultOptions) *output.Error {
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	executions, err := journal.ListExecutions(ctx, false)
	if err != nil {
		_ = journal.Close()
		return mapStoreError("list unreconciled executions", err)
	}
	acks, err := journal.AcknowledgementIndex(ctx)
	if err != nil {
		_ = journal.Close()
		return mapStoreError("list execution acknowledgements", err)
	}
	selected := make([]model.Execution, 0, opts.limit)
	total := 0
	for i := len(executions) - 1; i >= 0; i-- {
		execution := executions[i]
		if !acks.Unreconciled(execution) {
			continue
		}
		matched := true
		for _, wanted := range opts.labels {
			if !containsArg(execution.Labels, wanted) {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		total++
		if len(selected) < opts.limit {
			selected = append(selected, execution)
		}
	}
	items := make([]unreconciledResultItem, 0, len(selected))
	collect := make([]ids.ExecutionID, 0, len(selected))
	for _, execution := range selected {
		labels := append([]string(nil), execution.Labels...)
		if labels == nil {
			labels = []string{}
		}
		item := unreconciledResultItem{ExecutionID: execution.ID.String(), Labels: labels, Adapter: execution.Adapter, Authority: execution.Authority, State: execution.State, TerminalAt: execution.TerminalAt}
		_, outcome, problem := dereferenceResult(ctx, journal, execution.ID, opts)
		if problem != nil {
			item.Status = "skipped"
			item.SkipReason = string(problem.Code)
			item.SkipMessage = problem.Message
			items = append(items, item)
			continue
		}
		bounded := outcome
		item.Status = "collected"
		item.Outcome = &bounded
		collect = append(collect, execution.ID)
		items = append(items, item)
	}
	_ = journal.Close()

	// Stamps are written after every result is materialized so a mid-batch
	// journal error cannot acknowledge work whose outcome was never produced.
	stamped, ackProblem := a.acknowledgeExecutions(ctx, c, collect, store.AcknowledgementResult)
	acknowledged := 0
	collected, skipped := 0, 0
	for i := range items {
		switch items[i].Status {
		case "collected":
			collected++
			if stamped[items[i].ExecutionID] {
				items[i].Acknowledged = true
				acknowledged++
			}
		default:
			skipped++
		}
	}
	warnings := []output.Warning{}
	if ackProblem != nil {
		warnings = append(warnings, output.Warning{
			Code:    "acknowledgement_incomplete",
			Message: "results were delivered but some acknowledgement stamps were not written; re-run to stamp the remainder",
			Details: map[string]any{"error": ackProblem.Message},
		})
	}
	if skipped != 0 {
		warnings = append(warnings, output.Warning{
			Code:    "unreconciled_items_skipped",
			Message: "some uncollected executions could not be dereferenced and remain unreconciled",
			Details: map[string]any{"skipped": skipped},
		})
	}
	lines := make([]output.Line, 0, len(items))
	for _, item := range items {
		fields := []output.Field{{Name: "status", Value: item.Status}, {Name: "state", Value: item.State}, {Name: "adapter", Value: item.Adapter}, {Name: "acknowledged", Value: item.Acknowledged}}
		if item.Outcome != nil {
			fields = append(fields, output.Field{Name: "availability", Value: item.Outcome.Availability}, output.Field{Name: "content", Value: item.Outcome.Content != nil})
		}
		if item.SkipReason != "" {
			fields = append(fields, output.Field{Name: "skip_reason", Value: item.SkipReason})
		}
		lines = append(lines, output.Line{Lead: item.ExecutionID, Fields: fields})
	}
	result := map[string]any{
		"executions":   items,
		"count":        len(items),
		"total":        total,
		"has_more":     total > len(items),
		"collected":    collected,
		"skipped":      skipped,
		"acknowledged": acknowledged,
		"host_local":   true,
	}
	actions := []output.NextAction{}
	if total > len(items) {
		actions = append(actions, output.NextAction{
			Label:   "Collect the remaining uncollected results",
			Argv:    []string{"agentctl", "result", "--unreconciled", "--limit", strconv.Itoa(opts.limit)},
			Mutates: true, SideEffectClass: output.LocalOperationalWrite,
			Preconditions: []string{"already collected executions drop out of the set"},
		})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines, Warnings: warnings, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write unreconciled results", false, err)
	}
	return nil
}

// acknowledgeExecutions stamps a bounded set under one write lease. Stamping is
// first-write-wins, so re-running a partially stamped batch is a no-op for the
// executions already collected.
func (a *app) acknowledgeExecutions(ctx context.Context, c common, executions []ids.ExecutionID, source string) (map[string]bool, *output.Error) {
	stamped := map[string]bool{}
	if len(executions) == 0 {
		return stamped, nil
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return stamped, problem
	}
	defer journal.Close()
	for _, id := range executions {
		if _, _, err := journal.AcknowledgeExecution(ctx, id, source); err != nil {
			return stamped, mapStoreError("acknowledge terminal execution", err)
		}
		stamped[id.String()] = true
	}
	return stamped, nil
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
	var timeout time.Duration
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
	lastMulticaReprobe := time.Time{}
	consecutiveMulticaReprobeErrors := 0
	if timeoutSet {
		deadline = a.now().Add(timeout)
	}
	for {
		if ctx.Err() != nil {
			return a.fail(renderer, output.Wrap(output.CodeExecutionCancelled, "await cancelled", false, ctx.Err()).WithDetail("execution_id", id.String()))
		}
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
				// The stop is only useful if the caller can escape it: point at
				// the authority evidence, the read-only re-check, and the
				// explicit wait that continues through attention.
				return a.fail(renderer, outcomeError(output.CodeAttentionRequired, "execution requires attention", execution).
					WithDetail("authority", string(execution.Authority)).
					WithActions(attentionNextActions(renderer.Mode, execution)...))
			}
		}
		if !deadline.IsZero() && !a.now().Before(deadline) {
			return a.fail(renderer, outcomeError(output.CodeTimeout, "await deadline elapsed", execution))
		}
		if execution.Authority == model.AuthorityMultica && !execution.State.Terminal() && (lastMulticaReprobe.IsZero() || a.now().Sub(lastMulticaReprobe) >= awaitMulticaReprobeInterval) {
			probeCtx := ctx
			cancelProbe := func() {}
			if !deadline.IsZero() {
				probeCtx, cancelProbe = context.WithDeadline(ctx, deadline)
			}
			problem := a.reprobeAwaitedMultica(probeCtx, c, execution)
			probeContextError := probeCtx.Err()
			cancelProbe()
			if ctx.Err() != nil {
				return a.fail(renderer, output.Wrap(output.CodeExecutionCancelled, "await cancelled", false, ctx.Err()).WithDetail("execution_id", id.String()))
			}
			if errors.Is(probeContextError, context.DeadlineExceeded) {
				return a.fail(renderer, outcomeError(output.CodeTimeout, "await deadline elapsed", execution))
			}
			if problem != nil {
				consecutiveMulticaReprobeErrors++
				lastMulticaReprobe = a.now()
				if !problem.Retryable || consecutiveMulticaReprobeErrors >= awaitMulticaMaxConsecutiveErrors {
					problem.WithDetail("consecutive_failures", consecutiveMulticaReprobeErrors)
					return a.fail(renderer, problem)
				}
				continue
			}
			consecutiveMulticaReprobeErrors = 0
			lastMulticaReprobe = a.now()
			continue
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
	return openJournalWithRetryLoop(context.Background(), path, options, false)
}

func openJournalWithRetryContext(ctx context.Context, path string, options store.Options) (*store.Journal, error) {
	return openJournalWithRetryLoop(ctx, path, options, true)
}

func openJournalWithRetryLoop(ctx context.Context, path string, options store.Options, contextBound bool) (*store.Journal, error) {
	var err error
	for attempt := 0; attempt < 2; attempt++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		openOptions := options
		if contextBound {
			lockTimeout := 50 * time.Millisecond
			if deadline, ok := ctx.Deadline(); ok {
				remaining := time.Until(deadline)
				if remaining <= 0 {
					return nil, context.DeadlineExceeded
				}
				if remaining < lockTimeout {
					lockTimeout = remaining
				}
			}
			if openOptions.LockTimeout == 0 || openOptions.LockTimeout > lockTimeout {
				openOptions.LockTimeout = lockTimeout
			}
		}
		journal, openErr := store.Open(path, openOptions)
		if openErr == nil {
			if ctx.Err() != nil {
				_ = journal.Close()
				return nil, ctx.Err()
			}
			return journal, nil
		}
		err = openErr
		if !errors.Is(openErr, store.ErrBusy) || attempt == 1 {
			break
		}
		timer := time.NewTimer(time.Duration(100+time.Now().UnixNano()%250) * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
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
func writeExecution(renderer output.Renderer, e model.Execution, operation string, extraWarnings ...output.Warning) *output.Error {
	fields := []output.Field{{Name: "state", Value: e.State}, {Name: "authority", Value: e.Authority}, {Name: "adapter", Value: e.Adapter}, {Name: "liveness", Value: e.Liveness}, {Name: "revision", Value: e.Revision}}
	if len(e.Labels) != 0 {
		fields = append(fields, output.Field{Name: "labels", Value: e.Labels})
	}
	actions := []output.NextAction{}
	warnings := append([]output.Warning{}, extraWarnings...)
	if e.TaskContract != nil {
		fields = append(fields, output.Field{Name: "task_contract", Value: "retained"}, output.Field{Name: "acceptance", Value: "external_required"})
		warnings = append(warnings, taskContractAcceptanceWarning())
	}
	if operation == "run" {
		warnings = append(warnings, output.Warning{Code: "foreground_execution_owned", Message: "foreground run is owned by this process and has no default wall-clock timeout; use --background with recent, await, and result for work that must outlive this shell"})
		actions = append(actions, output.NextAction{Label: "Review run ownership and lifecycle", Argv: []string{"agentctl", "help", "run"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}})
	}
	if !e.State.Terminal() {
		label := "Wait without timeout"
		argv := []string{"agentctl", "await", e.ID.String(), "--output", string(renderer.Mode), "--no-timeout"}
		if e.DeadlineAt != nil {
			label = "Wait through execution deadline"
			argv = []string{"agentctl", "await", e.ID.String(), "--output", string(renderer.Mode), "--through-execution-deadline"}
		}
		if e.State == model.StateAttention {
			// A plain wait stops on attention, so recommending it here would
			// return attention_required immediately and loop the caller. The
			// decision belongs to the execution authority; agentctl offers the
			// read-only evidence, a read-only re-check, and the explicit wait
			// that continues through attention once the authority has decided.
			warnings = append(warnings, attentionAuthorityWarning(e))
			actions = append(actions, attentionNextActions(renderer.Mode, e)...)
		} else {
			actions = append(actions, output.NextAction{Label: label, Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{}})
		}
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

// attentionAuthorityWarning is the non-executable pointer at the authority that
// must decide. agentctl records the attention state but never resolves it: a
// Multica-authority execution is decided on its bound issue, and a native
// execution is decided in its own session or provider surface.
func attentionAuthorityWarning(e model.Execution) output.Warning {
	details := map[string]any{"execution_id": e.ID.String(), "authority": string(e.Authority), "adapter": e.Adapter}
	if e.SourceState != nil && *e.SourceState != "" {
		details["source_state"] = *e.SourceState
	}
	message := "the native session for this execution requires a human decision; agentctl cannot answer a permission, approval, authentication, or input prompt on its behalf"
	if e.Authority == model.AuthorityMultica {
		message = "the bound Multica issue requires a human decision; agentctl observes the attention but never decides it"
		for _, binding := range e.SourceBindings {
			if binding.Kind == "issue" {
				details["issue_alias"] = binding.AliasID.String()
				break
			}
		}
	}
	return output.Warning{Code: "attention_requires_authority_decision", Message: message, Details: details}
}

// attentionDecisionPrecondition keeps the continued wait honest: it is only
// useful after the authority named by attentionAuthorityWarning has decided.
func attentionDecisionPrecondition(e model.Execution) string {
	if e.Authority == model.AuthorityMultica {
		return "resolve the attention on the bound Multica issue; this wait does not decide it"
	}
	return "resolve the attention in the native session; this wait does not decide it"
}

// attentionNextActions are the escapes from an attention stop: read-only
// evidence, a read-only re-check, and the explicit wait that continues through
// attention. None of them returns attention_required again.
func attentionNextActions(mode output.Mode, e model.Execution) []output.NextAction {
	argv := []string{"agentctl", "await", e.ID.String(), "--output", string(mode), "--no-timeout", "--ignore-attention"}
	if e.DeadlineAt != nil {
		argv = []string{"agentctl", "await", e.ID.String(), "--output", string(mode), "--through-execution-deadline", "--ignore-attention"}
	}
	return []output.NextAction{
		{Label: "Read the attention evidence", Argv: []string{"agentctl", "events", e.ID.String(), "--output", string(mode), "--kind", "attention"}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
		{Label: "Re-check state after the authority decision", Argv: []string{"agentctl", "status", e.ID.String(), "--output", string(mode)}, Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{}},
		{Label: "Wait through attention after the authority decides", Argv: argv, Mutates: true, SideEffectClass: output.LocalOperationalWrite, Preconditions: []string{attentionDecisionPrecondition(e)}},
	}
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
