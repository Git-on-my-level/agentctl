package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/fanout"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const maxFanoutPromptBytes = 64 << 20

type fanoutManifest = fanout.Manifest
type fanoutChild = fanout.Child

type fanoutChildResult struct {
	Name            string         `json:"name,omitempty"`
	ExecutionID     string         `json:"execution_id"`
	Adapter         string         `json:"adapter"`
	Labels          []string       `json:"labels,omitempty"`
	Prompt          map[string]any `json:"prompt,omitempty"`
	LaunchAttempted bool           `json:"launch_attempted"`
	Recorded        bool           `json:"recorded"`
	State           model.State    `json:"state,omitempty"`
	Error           *output.Error  `json:"error,omitempty"`
}

type fanoutResult struct {
	SchemaVersion int                 `json:"schema_version"`
	Manifest      string              `json:"manifest"`
	Prompt        map[string]any      `json:"prompt,omitempty"`
	Concurrency   int                 `json:"concurrency"`
	Planned       bool                `json:"planned"`
	Success       bool                `json:"success"`
	Children      []fanoutChildResult `json:"children"`
}

func (a *app) fanout(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	var manifestPath string
	plan, failFast := false, false
	concurrencyOverride := 0
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--manifest":
			i++
			if i >= len(args) {
				return output.NewError(output.CodeUsage, "--manifest requires a path", false)
			}
			manifestPath = args[i]
		case "--concurrency":
			i++
			if i >= len(args) {
				return output.NewError(output.CodeUsage, "--concurrency requires an integer", false)
			}
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 1 || value > fanout.MaxConcurrency {
				return output.NewError(output.CodeUsage, "--concurrency must be between 1 and 16", false)
			}
			concurrencyOverride = value
		case "--plan":
			plan = true
		case "--fail-fast":
			failFast = true
		default:
			return output.NewError(output.CodeUsage, "unknown fanout flag", false).WithDetail("flag", args[i])
		}
	}
	if manifestPath == "" {
		return output.NewError(output.CodeUsage, "fanout requires --manifest", false)
	}
	manifest, absoluteManifest, problem := readFanoutManifest(manifestPath)
	if problem != nil {
		return problem
	}
	if concurrencyOverride > 0 {
		manifest.Concurrency = concurrencyOverride
	}
	if manifest.Concurrency == 0 {
		manifest.Concurrency = 2
	}
	manifestRoot := filepath.Dir(absoluteManifest)
	prompts, sharedPrompt, problem := a.loadFanoutPrompts(manifest, manifestRoot)
	if problem != nil {
		return problem
	}
	results := make([]fanoutChildResult, len(manifest.Children))
	childArgs := make([][]string, len(manifest.Children))
	for i, child := range manifest.Children {
		if child.CWD != "" && !filepath.IsAbs(child.CWD) {
			child.CWD = filepath.Join(manifestRoot, child.CWD)
		}
		child.Labels = fanout.MergeLabels(manifest.Labels, child.Labels)
		id, err := fanoutExecutionID(child.ExecutionID)
		if err != nil {
			return output.Wrap(output.CodeUsage, "invalid fanout child execution ID", false, err).WithDetail("child", i)
		}
		adapterName := child.Adapter
		if adapterName == "" {
			adapterName = inferAdapter(child.Argv[0])
		}
		results[i] = fanoutChildResult{Name: child.Name, ExecutionID: id.String(), Adapter: adapterName, Labels: child.Labels, Prompt: promptSummary(prompts[i])}
		childArgs[i] = fanoutRunArgs(child, id, prompts[i])
	}
	result := fanoutResult{SchemaVersion: 1, Manifest: absoluteManifest, Concurrency: manifest.Concurrency, Planned: plan, Children: results}
	if sharedPrompt != nil {
		result.Prompt = promptSummary(sharedPrompt)
	}

	// Validate the entire batch before launching any task, not only for --plan.
	// Probes may run declared read-only native discovery commands. The actual
	// launch rechecks capabilities; preflight is not an atomic reservation.
	for i := range manifest.Children {
		if ctx.Err() != nil {
			results[i].Error = output.Wrap(output.CodeExecutionCancelled, "fanout preflight cancelled", false, ctx.Err())
			continue
		}
		childApp := a.fanoutChildApp(prompts[i])
		results[i].Error = childApp.runNative(ctx, output.Renderer{Mode: output.JSON, Writer: io.Discard}, c, append([]string{"--plan"}, childArgs[i]...))
	}
	if ctx.Err() == nil {
		if problem := a.observeFanout(ctx, c, results, true); problem != nil {
			return problem.WithDetail("fanout", result)
		}
	}
	if !fanoutResultsSuccessful(results, true) {
		code, message := output.CodeUsage, "one or more fanout children failed preflight; no tasks were launched"
		if ctx.Err() != nil {
			code, message = output.CodeExecutionCancelled, "fanout preflight cancelled; no tasks were launched"
		}
		return output.NewError(code, message, false).WithDetail("fanout", result)
	}
	if plan {
		result.Success = true
		return writeFanout(renderer, result)
	}

	attempted, err := fanout.Run(ctx, len(results), manifest.Concurrency, failFast, func(runCtx context.Context, i int) bool {
		childApp := a.fanoutChildApp(prompts[i])
		problem := childApp.runNative(runCtx, output.Renderer{Mode: output.JSON, Writer: io.Discard}, c, childArgs[i])
		results[i].Error = problem
		// run returns a successful command envelope even for a failed native
		// outcome. Read the journal to make --fail-fast respect work failure.
		if observation := childApp.observeFanout(context.Background(), c, results[i:i+1], false); observation != nil {
			if results[i].Error == nil {
				results[i].Error = observation
			} else {
				results[i].Error.WithDetail("observation_error", observation)
			}
		}
		if results[i].Error == nil && !results[i].Recorded {
			results[i].Error = output.NewError(output.CodeInternal, "fanout child returned without a journal record", false)
		}
		return results[i].Error == nil && results[i].State == model.StateCompleted
	})
	if err != nil {
		return output.Wrap(output.CodeInternal, "execute fanout", false, err).WithDetail("fanout", result)
	}
	for i, wasAttempted := range attempted {
		results[i].LaunchAttempted = wasAttempted
		if !wasAttempted {
			results[i].Error = output.NewError(output.CodeExecutionCancelled, "fanout child skipped before launch", false)
		}
	}
	result.Success = fanoutResultsSuccessful(results, false)
	if !result.Success {
		return output.NewError(output.CodeExecutionFailed, "one or more fanout children did not complete", false).WithDetail("fanout", result)
	}
	return writeFanout(renderer, result)
}

func (a *app) fanoutChildApp(prompt *promptPayload) *app {
	child := *a
	child.stdin = bytes.NewReader(prompt.Bytes)
	child.stdinIsTerminal = func() bool { return false }
	child.stdout, child.stderr = io.Discard, io.Discard
	return &child
}

// observeFanout does not acknowledge results or create a missing journal. It
// preserves a run error alongside the actual recorded state after cancellation.
func (a *app) observeFanout(ctx context.Context, c common, results []fanoutChildResult, rejectExisting bool) *output.Error {
	path, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve fanout journal", false, err)
	}
	journal, err := openJournalWithRetryContext(ctx, path, store.Options{ReadOnly: true})
	if errors.Is(err, store.ErrNotFound) {
		return nil
	}
	if err != nil {
		return mapStoreError("open fanout journal", err)
	}
	defer journal.Close()
	for i := range results {
		id, _ := ids.ParseExecutionID(results[i].ExecutionID)
		execution, err := journal.GetExecution(ctx, id)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return mapStoreError("read fanout child", err)
		}
		results[i].Recorded, results[i].State = true, execution.State
		if rejectExisting && results[i].Error == nil {
			results[i].Error = output.NewError(output.CodeConflict, "fanout execution ID already exists; fanout does not replay or resume work", false)
		}
	}
	return nil
}

func (a *app) loadFanoutPrompts(manifest fanoutManifest, root string) ([]*promptPayload, *promptPayload, *output.Error) {
	cache := map[string]*promptPayload{}
	total := 0
	load := func(path, delivery string) (*promptPayload, *output.Error) {
		key, confined := fanoutPromptCacheKey(root, path)
		payload := cache[key]
		if payload == nil || !confined {
			var problem *output.Error
			payload, problem = a.loadPrompt(runOptions{cwd: root, promptFile: path, promptDelivery: normalizedPromptDelivery(delivery)})
			if problem != nil {
				return nil, problem
			}
			total += len(payload.Bytes)
			if total > maxFanoutPromptBytes {
				return nil, output.NewError(output.CodeUsage, "fanout unique prompt bytes exceed 64 MiB limit", false)
			}
			if confined {
				cache[key] = payload
			}
		}
		// Byte storage is shared, but delivery metadata is per child. Never
		// mutate a cached prompt while concurrent argv/stdin consumers use it.
		copy := *payload
		copy.Delivery = normalizedPromptDelivery(delivery)
		return &copy, nil
	}
	var shared *promptPayload
	if manifest.PromptFile != "" {
		var problem *output.Error
		shared, problem = load(manifest.PromptFile, manifest.PromptDelivery)
		if problem != nil {
			return nil, nil, problem
		}
	}
	prompts := make([]*promptPayload, len(manifest.Children))
	for i, child := range manifest.Children {
		path, delivery := child.PromptFile, child.PromptDelivery
		if path == "" {
			path = manifest.PromptFile
		}
		if delivery == "" {
			delivery = manifest.PromptDelivery
		}
		payload, problem := load(path, delivery)
		if problem != nil {
			return nil, nil, problem.WithDetail("child", i)
		}
		prompts[i] = payload
	}
	return prompts, shared, nil
}

func readFanoutManifest(path string) (fanoutManifest, string, *output.Error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeUsage, "resolve fanout manifest", false, err)
	}
	file, err := openRegularNoFollow(absolute)
	if err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeAuthorizationDenied, "open fanout manifest", false, err)
	}
	defer file.Close()
	body, err := io.ReadAll(io.LimitReader(file, fanout.MaxManifestBytes+1))
	if err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeUsage, "read fanout manifest", false, err)
	}
	manifest, err := fanout.Decode(body)
	if err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeUsage, "invalid fanout manifest", false, err)
	}
	return manifest, absolute, nil
}

func fanoutExecutionID(value string) (ids.ExecutionID, error) {
	if strings.TrimSpace(value) != "" {
		return ids.ParseExecutionID(value)
	}
	raw, err := ids.New(ids.TypeExecution)
	if err != nil {
		return "", err
	}
	return ids.ParseExecutionID(raw.String())
}

func fanoutRunArgs(child fanoutChild, id ids.ExecutionID, prompt *promptPayload) []string {
	delivery := normalizedPromptDelivery(child.PromptDelivery)
	if child.PromptDelivery == "" {
		delivery = prompt.Delivery
	}
	args := []string{"--execution-id", id.String(), "--prompt-stdin", "--prompt-delivery", delivery}
	for _, label := range child.Labels {
		args = append(args, "--label", label)
	}
	if child.Adapter != "" {
		args = append(args, "--adapter", child.Adapter)
	}
	if child.CWD != "" {
		args = append(args, "--cwd", child.CWD)
	}
	if child.Timeout != "" {
		args = append(args, "--timeout", child.Timeout)
	}
	if child.NoStoreResult {
		args = append(args, "--no-store-result")
	}
	if child.AllowMissingResult {
		args = append(args, "--allow-missing-result")
	}
	if child.AllowUnreliableResult {
		args = append(args, "--allow-unreliable-result")
	}
	return append(append(args, "--"), child.Argv...)
}

func fanoutPromptCacheKey(root, path string) (string, bool) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return path, false
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(absRoot, path)
	}
	path, err = filepath.Abs(filepath.Clean(path))
	if err != nil {
		return path, false
	}
	rel, err := filepath.Rel(absRoot, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path, false
	}
	resolvedRoot, err := filepath.EvalSymlinks(absRoot)
	if err != nil {
		return filepath.Join(absRoot, rel), true
	}
	return filepath.Join(resolvedRoot, rel), true
}

func normalizedPromptDelivery(value string) string {
	if value == "" {
		return "argv"
	}
	return value
}

func promptSummary(prompt *promptPayload) map[string]any {
	return map[string]any{"source": prompt.Source, "delivery": prompt.Delivery, "bytes": len(prompt.Bytes), "sha256": prompt.Digest}
}

func fanoutResultsSuccessful(results []fanoutChildResult, plan bool) bool {
	for _, result := range results {
		if result.Error != nil || !plan && result.State != model.StateCompleted {
			return false
		}
	}
	return true
}

func writeFanout(renderer output.Renderer, result fanoutResult) *output.Error {
	lines := []output.Line{{Lead: "fanout", Fields: []output.Field{{Name: "planned", Value: result.Planned}, {Name: "success", Value: result.Success}, {Name: "children", Value: len(result.Children)}, {Name: "concurrency", Value: result.Concurrency}}}}
	for _, child := range result.Children {
		lines = append(lines, output.Line{Lead: child.ExecutionID, Fields: []output.Field{{Name: "name", Value: child.Name}, {Name: "adapter", Value: child.Adapter}, {Name: "attempted", Value: child.LaunchAttempted}, {Name: "recorded", Value: child.Recorded}, {Name: "state", Value: child.State}}})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write fanout result", false, err)
	}
	return nil
}
