package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

const (
	maxFanoutManifestBytes = 1 << 20
	maxFanoutChildren      = 64
	maxFanoutConcurrency   = 16
)

type fanoutManifest struct {
	SchemaVersion  int           `json:"schema_version"`
	PromptFile     string        `json:"prompt_file"`
	PromptDelivery string        `json:"prompt_delivery,omitempty"`
	Concurrency    int           `json:"concurrency,omitempty"`
	Children       []fanoutChild `json:"children"`
}

type fanoutChild struct {
	ExecutionID           string   `json:"execution_id,omitempty"`
	Adapter               string   `json:"adapter,omitempty"`
	CWD                   string   `json:"cwd,omitempty"`
	Timeout               string   `json:"timeout,omitempty"`
	PromptDelivery        string   `json:"prompt_delivery,omitempty"`
	NoStoreResult         bool     `json:"no_store_result,omitempty"`
	AllowMissingResult    bool     `json:"allow_missing_result,omitempty"`
	AllowUnreliableResult bool     `json:"allow_unreliable_result,omitempty"`
	Argv                  []string `json:"argv"`
}

type fanoutChildResult struct {
	ExecutionID string        `json:"execution_id"`
	Adapter     string        `json:"adapter"`
	State       model.State   `json:"state,omitempty"`
	Error       *output.Error `json:"error,omitempty"`
}

type fanoutResult struct {
	SchemaVersion int                 `json:"schema_version"`
	Manifest      string              `json:"manifest"`
	Prompt        map[string]any      `json:"prompt"`
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
			if err != nil || value <= 0 {
				return output.NewError(output.CodeUsage, "--concurrency must be a positive integer", false)
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
	if manifest.Concurrency < 1 || manifest.Concurrency > maxFanoutConcurrency {
		return output.NewError(output.CodeUsage, "fanout concurrency must be between 1 and 16", false)
	}
	manifestRoot := filepath.Dir(absoluteManifest)
	prompt, problem := a.loadPrompt(runOptions{cwd: manifestRoot, promptFile: manifest.PromptFile, promptDelivery: normalizedPromptDelivery(manifest.PromptDelivery)})
	if problem != nil {
		return problem
	}

	results := make([]fanoutChildResult, len(manifest.Children))
	childArgs := make([][]string, len(manifest.Children))
	for i, child := range manifest.Children {
		if child.CWD != "" && !filepath.IsAbs(child.CWD) {
			child.CWD = filepath.Join(manifestRoot, child.CWD)
			manifest.Children[i].CWD = child.CWD
		}
		id, err := fanoutExecutionID(child.ExecutionID)
		if err != nil {
			return output.Wrap(output.CodeUsage, "invalid fanout child execution ID", false, err).WithDetail("child", i)
		}
		adapterName := child.Adapter
		if adapterName == "" {
			adapterName = inferAdapter(child.Argv[0])
		}
		results[i] = fanoutChildResult{ExecutionID: id.String(), Adapter: adapterName}
		childArgs[i] = fanoutRunArgs(child, id, prompt)
	}

	if plan {
		for i := range manifest.Children {
			var childOut, childErr bytes.Buffer
			childApp := *a
			childApp.stdin = bytes.NewReader(prompt.Bytes)
			childApp.stdinIsTerminal = func() bool { return false }
			childApp.stdout, childApp.stderr = &childOut, &childErr
			args := append([]string{"--plan"}, childArgs[i]...)
			if problem := childApp.runNative(ctx, output.Renderer{Mode: output.JSON, Writer: &childOut}, c, args); problem != nil {
				results[i].Error = problem
			}
		}
		result := fanoutResult{SchemaVersion: 1, Manifest: absoluteManifest, Prompt: promptSummary(prompt), Concurrency: manifest.Concurrency, Planned: true, Success: fanoutResultsSuccessful(results, true), Children: results}
		if !result.Success {
			return output.NewError(output.CodeUsage, "one or more fanout children failed preflight", false).WithDetail("fanout", result)
		}
		return writeFanout(renderer, result)
	}

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	sem := make(chan struct{}, manifest.Concurrency)
	var wg sync.WaitGroup
	for i := range manifest.Children {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-runCtx.Done():
				results[i].Error = output.Wrap(output.CodeExecutionCancelled, "fanout child cancelled before launch", false, runCtx.Err())
				return
			}
			if err := runCtx.Err(); err != nil {
				results[i].Error = output.Wrap(output.CodeExecutionCancelled, "fanout child cancelled before launch", false, err)
				return
			}
			var childOut, childErr bytes.Buffer
			childApp := *a
			childApp.stdin = bytes.NewReader(prompt.Bytes)
			childApp.stdinIsTerminal = func() bool { return false }
			childApp.stdout, childApp.stderr = &childOut, &childErr
			problem := childApp.runNative(runCtx, output.Renderer{Mode: output.JSON, Writer: &childOut}, c, childArgs[i])
			if problem != nil {
				results[i].Error = problem
				if failFast {
					cancel()
				}
				return
			}
			journal, openProblem := childApp.openRead(c)
			if openProblem != nil {
				results[i].Error = openProblem
				if failFast {
					cancel()
				}
				return
			}
			executionID, _ := ids.ParseExecutionID(results[i].ExecutionID)
			execution, err := journal.GetExecution(context.Background(), executionID)
			_ = journal.Close()
			if err != nil {
				results[i].Error = mapStoreError("read fanout child", err)
			} else {
				results[i].State = execution.State
				if execution.State != model.StateCompleted && failFast {
					cancel()
				}
			}
		}()
	}
	wg.Wait()
	result := fanoutResult{SchemaVersion: 1, Manifest: absoluteManifest, Prompt: promptSummary(prompt), Concurrency: manifest.Concurrency, Planned: false, Success: fanoutResultsSuccessful(results, false), Children: results}
	if !result.Success {
		return output.NewError(output.CodeExecutionFailed, "one or more fanout children did not complete", false).WithDetail("fanout", result)
	}
	return writeFanout(renderer, result)
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
	body, err := io.ReadAll(io.LimitReader(file, maxFanoutManifestBytes+1))
	if err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeUsage, "read fanout manifest", false, err)
	}
	if len(body) > maxFanoutManifestBytes {
		return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout manifest exceeds 1 MiB limit", false)
	}
	var manifest fanoutManifest
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return fanoutManifest{}, "", output.Wrap(output.CodeUsage, "decode fanout manifest", false, err)
	}
	if manifest.SchemaVersion != 1 {
		return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout manifest schema_version must be 1", false)
	}
	if strings.TrimSpace(manifest.PromptFile) == "" {
		return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout manifest requires prompt_file", false)
	}
	if len(manifest.Children) == 0 || len(manifest.Children) > maxFanoutChildren {
		return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout manifest must contain between 1 and 64 children", false)
	}
	if manifest.PromptDelivery != "" && normalizedPromptDelivery(manifest.PromptDelivery) != "argv" && normalizedPromptDelivery(manifest.PromptDelivery) != "stdin" {
		return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout prompt_delivery must be argv or stdin", false)
	}
	seenExecutionIDs := make(map[string]struct{})
	for i, child := range manifest.Children {
		if len(child.Argv) == 0 || strings.TrimSpace(child.Argv[0]) == "" {
			return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout child requires non-empty argv", false).WithDetail("child", i)
		}
		if len(child.Argv) > 256 {
			return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout child argv exceeds 256 entries", false).WithDetail("child", i)
		}
		if child.ExecutionID != "" {
			if _, exists := seenExecutionIDs[child.ExecutionID]; exists {
				return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout child execution_id values must be unique", false).WithDetail("child", i)
			}
			seenExecutionIDs[child.ExecutionID] = struct{}{}
		}
		if child.Timeout != "" {
			value, err := time.ParseDuration(child.Timeout)
			if err != nil || value <= 0 {
				return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout child timeout must be a positive Go duration", false).WithDetail("child", i)
			}
		}
		delivery := child.PromptDelivery
		if delivery == "" {
			delivery = manifest.PromptDelivery
		}
		if normalizedPromptDelivery(delivery) != "argv" && normalizedPromptDelivery(delivery) != "stdin" {
			return fanoutManifest{}, "", output.NewError(output.CodeUsage, "fanout prompt_delivery must be argv or stdin", false).WithDetail("child", i)
		}
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
		lines = append(lines, output.Line{Lead: child.ExecutionID, Fields: []output.Field{{Name: "adapter", Value: child.Adapter}, {Name: "state", Value: child.State}}})
	}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write fanout result", false, err)
	}
	return nil
}
