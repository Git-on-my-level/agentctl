package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestPromptOutsideRootRepairsMatchCommandParsers(t *testing.T) {
	root, outside := testutil.TempDir(t), testutil.TempDir(t)
	path := filepath.Join(outside, "private-prompt.md")
	const secret = "prompt-repair-private-task-token"
	if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	request := filepath.Join(root, "request.json")
	if err := os.WriteFile(request, []byte(`{"schema_version":1,"request_key":"repair-test","selector":{"family":"sol"}}`), 0600); err != nil {
		t.Fatal(err)
	}
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	ref := rawID.String()
	tests := []struct {
		name, command, delivery string
		args                    []string
	}{
		{"run_default", "run", "argv", []string{"--cwd", root, "--prompt-file", path, "--plan", "--", "/bin/echo"}},
		{"run_argv", "run", "argv", []string{"--cwd", root, "--prompt-file", path, "--prompt-delivery", "argv", "--plan", "--", "/bin/echo"}},
		{"run_stdin", "run", "stdin", []string{"--cwd", root, "--prompt-file", path, "--prompt-delivery", "stdin", "--plan", "--", "/bin/echo"}},
		{"run_stream", "run", "stream", []string{"--cwd", root, "--prompt-file", path, "--prompt-delivery", "stream", "--plan", "--", "/bin/echo"}},
		{"run_launch", "run", "argv", []string{"--cwd", root, "--label", "review", "--timeout", "30s", "--prompt-file", path, "--", "/bin/echo"}},
		{"delegate", "delegate", "", []string{"--cwd", root, "--request-file", request, "--prompt-file", path, "--authority", "native", "--plan"}},
		{"delegate_launch", "delegate", "", []string{"--cwd", root, "--request-file", request, "--prompt-file", path, "--authority", "native", "--wait", "--label", "review"}},
		{"continue", "continue", "", []string{ref, "--request-key", "repair-test", "--prompt-file", path, "--plan"}},
		{"dispatch", "dispatch", "", []string{"--route", "example", "--title", "Repair test", "--idempotency-key", "repair-test", "--prompt-file", path, "--plan"}},
		{"dispatch_launch", "dispatch", "", []string{"--route", "example", "--title", "Repair test", "--idempotency-key", "repair-test", "--prompt-file", path, "--label", "review"}},
		{"steer", "steer", "", []string{ref, "--prompt-file", path, "--plan"}},
		{"steer_launch", "steer", "", []string{ref, "--prompt-file", path, "--allow-interrupt", "--idempotency-key", "repair-test"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			input := strings.NewReader("unread-input")
			a.stdin = input
			journal := filepath.Join(testutil.TempDir(t), "state", "journal.db")
			argv := append([]string{"--output", "json", "--journal", journal, tt.command}, tt.args...)
			if code := a.run(context.Background(), argv); code != output.ExitCodeFor(output.CodeAuthorizationDenied) {
				t.Fatalf("exit=%d output=%s", code, stdout.String())
			}
			if strings.Contains(stdout.String()+stderr.String(), secret) || input.Len() != len("unread-input") {
				t.Fatal("boundary rejection read or leaked prompt input")
			}
			if _, err := os.Stat(filepath.Dir(journal)); !os.IsNotExist(err) {
				t.Fatalf("boundary rejection created journal state: %v", err)
			}
			var document output.ErrorDocument
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			problem := document.Error
			if problem == nil || problem.Details["recommended_prompt_source"] != "stdin" {
				t.Fatalf("missing stdin repair: %#v", problem)
			}
			repair, ok := problem.Details["repair"].(map[string]any)
			if !ok || repair["stdin_file"] != path || repair["preserve_other_arguments"] != true {
				t.Fatalf("repair=%#v", repair)
			}
			flags, ok := repair["set_flags"].(map[string]any)
			if !ok || flags["--prompt-stdin"] != "" {
				t.Fatalf("set_flags=%#v", flags)
			}
			if tt.command == "run" {
				if len(flags) != 2 || flags["--prompt-delivery"] != tt.delivery {
					t.Fatalf("run changed delivery: %#v", flags)
				}
			} else if len(flags) != 1 {
				t.Fatalf("repair added unsupported command flags: %#v", flags)
			}
			wantMutates, wantClass := false, output.ReadOnly
			if !argsPlan(tt.args) {
				wantMutates = true
				wantClass = output.ExternalSideEffect
				if tt.command == "dispatch" {
					wantClass = output.RemoteCoordinationWrite
				}
			}
			if len(problem.NextActions) != 2 || problem.NextActions[0].Mutates != wantMutates || problem.NextActions[0].SideEffectClass != wantClass {
				t.Fatalf("next_actions=%#v", problem.NextActions)
			}
			stdinAction := problem.NextActions[0]
			if !stdinRepairPrecedesDelimiter(stdinAction.Argv, path) {
				t.Fatalf("stdin repair argv=%#v", stdinAction.Argv)
			}
			if problem := preservedRepairArgs(tt.args, stdinAction.Argv); problem != "" {
				t.Fatal(problem)
			}
			if !reflect.DeepEqual(problem.NextActions[1].Argv, []string{"agentctl", "help", tt.command}) {
				t.Fatalf("help action=%#v", problem.NextActions[1])
			}
			repaired := applyPromptRepairFlags(t, tt.args, repair)
			actionArgs := shellCommandArgs(stdinAction.Argv, path)
			var parseProblem *output.Error
			switch tt.command {
			case "run":
				opts, problem := parseRun(repaired)
				parseProblem = problem
				if opts.promptDelivery != tt.delivery || !reflect.DeepEqual(opts.argv, []string{"/bin/echo"}) {
					t.Fatalf("repair changed native arguments or delivery: %#v", opts)
				}
				actionOpts, actionProblem := parseRun(actionArgs)
				if actionProblem != nil || !actionOpts.promptStdin || actionOpts.plan != opts.plan || actionOpts.timeout != opts.timeout || !reflect.DeepEqual(actionOpts.argv, opts.argv) || !reflect.DeepEqual(actionOpts.labels, opts.labels) {
					t.Fatalf("next action=%#v problem=%v repaired=%#v", actionOpts, actionProblem, opts)
				}
			case "delegate":
				opts, problem := parseDelegate(repaired)
				parseProblem = problem
				actionOpts, actionProblem := parseDelegate(actionArgs)
				if actionProblem != nil || !actionOpts.promptStdin || actionOpts.plan != opts.plan || actionOpts.wait != opts.wait || actionOpts.authority != opts.authority || !reflect.DeepEqual(actionOpts.labels, opts.labels) {
					t.Fatalf("next action=%#v problem=%v repaired=%#v", actionOpts, actionProblem, opts)
				}
			case "continue":
				_, _, opts, problem := parseContinue(repaired)
				parseProblem = problem
				_, _, actionOpts, actionProblem := parseContinue(actionArgs)
				if actionProblem != nil || !actionOpts.promptStdin || actionOpts.plan != opts.plan || actionOpts.wait != opts.wait || actionOpts.continuationMode != opts.continuationMode || !reflect.DeepEqual(actionOpts.labels, opts.labels) {
					t.Fatalf("next action=%#v problem=%v repaired=%#v", actionOpts, actionProblem, opts)
				}
			case "dispatch":
				opts, problem := parseDispatch(repaired)
				parseProblem = problem
				actionOpts, actionProblem := parseDispatch(actionArgs)
				if actionProblem != nil || !actionOpts.promptStdin || actionOpts.plan != opts.plan || actionOpts.routeSelector != opts.routeSelector || !reflect.DeepEqual(actionOpts.labels, opts.labels) {
					t.Fatalf("next action=%#v problem=%v repaired=%#v", actionOpts, actionProblem, opts)
				}
			case "steer":
				_, opts, problem := parseSteer(repaired)
				parseProblem = problem
				_, actionOpts, actionProblem := parseSteer(actionArgs)
				if actionProblem != nil || !actionOpts.promptStdin || actionOpts.plan != opts.plan || actionOpts.allowInterr != opts.allowInterr || actionOpts.idempotencyKey != opts.idempotencyKey {
					t.Fatalf("next action=%#v problem=%v repaired=%#v", actionOpts, actionProblem, opts)
				}
			}
			if parseProblem != nil {
				t.Fatalf("originating parser rejected emitted repair: %v; argv=%q", parseProblem, repaired)
			}
		})
	}
}

// Apply the documented repair shape without assumptions about a command's
// accepted prompt flags, then let that command's real parser verify it.
func applyPromptRepairFlags(t *testing.T, args []string, repair map[string]any) []string {
	t.Helper()
	removed, ok := repair["remove_flags"].([]any)
	if !ok {
		t.Fatalf("remove_flags=%#v", repair["remove_flags"])
	}
	flags := repair["set_flags"].(map[string]any)
	replace := map[string]bool{}
	for _, flag := range removed {
		replace[flag.(string)] = true
	}
	keys := make([]string, 0, len(flags))
	for flag := range flags {
		replace[flag] = true
		keys = append(keys, flag)
	}
	sort.Strings(keys)
	var result, native []string
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			native = args[i:]
			break
		}
		if replace[args[i]] {
			if args[i] != "--prompt-stdin" {
				i++
			}
			continue
		}
		result = append(result, args[i])
	}
	for _, flag := range keys {
		result = append(result, flag)
		if value := flags[flag].(string); value != "" {
			result = append(result, value)
		}
	}
	return append(result, native...)
}

func argsPlan(args []string) bool {
	for _, arg := range args {
		if arg == "--" {
			return false
		}
		if arg == "--plan" {
			return true
		}
	}
	return false
}

// stdinRepairPrecedesDelimiter requires the shell redirect to be an agentctl
// flag. A "--" earlier in the argv would hand it to the native command.
func stdinRepairPrecedesDelimiter(argv []string, path string) bool {
	limit := len(argv)
	for i, arg := range argv {
		if arg == "--" {
			limit = i
			break
		}
	}
	return limit >= 3 && argv[limit-3] == "--prompt-stdin" && argv[limit-2] == "<" && argv[limit-1] == path
}

func preservedRepairArgs(args, action []string) string {
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			suffix := args[i:]
			if !reflect.DeepEqual(action[len(action)-len(suffix):], suffix) {
				return fmt.Sprintf("native argv = %#v, want suffix %#v", action, suffix)
			}
			return ""
		}
		if args[i] == "--prompt-file" {
			i++
			continue
		}
		if !containsArg(action, args[i]) {
			return fmt.Sprintf("dropped %s from %#v", args[i], action)
		}
		if i+1 < len(args) && args[i+1] != "--" && !strings.HasPrefix(args[i+1], "-") && !containsArg(action, args[i+1]) {
			return fmt.Sprintf("dropped %s value %s from %#v", args[i], args[i+1], action)
		}
	}
	return ""
}

func shellCommandArgs(argv []string, path string) []string {
	if len(argv) >= 2 && argv[0] == "agentctl" {
		argv = argv[2:]
	}
	out := make([]string, 0, len(argv))
	for i := 0; i < len(argv); i++ {
		if argv[i] == "<" && i+1 < len(argv) && argv[i+1] == path {
			i++
			continue
		}
		out = append(out, argv[i])
	}
	return out
}

func TestFanoutOutsideRootRepairUsesManifestGuidance(t *testing.T) {
	for _, childPrompt := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "child"}[childPrompt], func(t *testing.T) {
			root, outside := testutil.TempDir(t), testutil.TempDir(t)
			path := filepath.Join(outside, "private-prompt.md")
			const secret = "fanout-repair-private-task-token"
			if err := os.WriteFile(path, []byte(secret), 0600); err != nil {
				t.Fatal(err)
			}
			manifest := fanoutManifest{SchemaVersion: 1, Children: []fanoutChild{{Argv: []string{"/bin/echo"}}}}
			if childPrompt {
				manifest.Children[0].PromptFile = path
			} else {
				manifest.PromptFile = path
			}
			body, err := json.Marshal(manifest)
			if err != nil {
				t.Fatal(err)
			}
			manifestPath := filepath.Join(root, "fanout.json")
			if err := os.WriteFile(manifestPath, body, 0600); err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			journal := filepath.Join(root, "state", "journal.db")
			code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "fanout", "--manifest", manifestPath, "--plan"})
			if code != output.ExitCodeFor(output.CodeAuthorizationDenied) {
				t.Fatalf("exit=%d output=%s", code, stdout.String())
			}
			var document output.ErrorDocument
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
			problem := document.Error
			repair := problem.Details["repair"].(map[string]any)
			if problem.Details["recommended_prompt_source"] != "file" || repair["manual_action"] == nil || repair["set_flags"] != nil || repair["remove_flags"] != nil {
				t.Fatalf("fanout advertised unsupported flag repair: %#v", problem)
			}
			if len(problem.NextActions) != 1 || !reflect.DeepEqual(problem.NextActions[0].Argv, []string{"agentctl", "help", "fanout"}) || problem.NextActions[0].Mutates {
				t.Fatalf("next_actions=%#v", problem.NextActions)
			}
			if strings.Contains(stdout.String()+stderr.String(), secret) {
				t.Fatal("prompt leaked")
			}
			if _, err := os.Stat(filepath.Dir(journal)); !os.IsNotExist(err) {
				t.Fatalf("boundary rejection created journal state: %v", err)
			}
			// Follow the manual repair and confirm that both shared and child
			// manifest fields now load successfully without launching work.
			if err := os.WriteFile(filepath.Join(root, "task.md"), []byte(secret), 0600); err != nil {
				t.Fatal(err)
			}
			if childPrompt {
				manifest.Children[0].PromptFile = "task.md"
			} else {
				manifest.PromptFile = "task.md"
			}
			if _, _, problem := a.loadFanoutPrompts(manifest, root); problem != nil {
				t.Fatalf("manual manifest repair failed: %v", problem)
			}
		})
	}
}

func TestPromptFileNotFoundIsPreciseAndRepairable(t *testing.T) {
	root := testutil.TempDir(t)
	missing := filepath.Join(root, "missing.md")
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	_, problem := a.loadPromptForCommand("delegate", runOptions{cwd: root, promptFile: "missing.md", promptDelivery: "argv",
		promptRepairArgv: []string{"agentctl", "delegate", "--request-file", "request.json"}})
	if problem == nil || problem.Code != output.CodeNotFound {
		t.Fatalf("problem=%#v", problem)
	}
	if problem.Details["path"] != missing || problem.Details["requested_path"] != "missing.md" {
		t.Fatalf("details=%#v", problem.Details)
	}
	if len(problem.NextActions) == 0 {
		t.Fatal("missing repair action")
	}
	want := []string{"agentctl", "delegate", "--request-file", "request.json", "--prompt-stdin", "<", missing}
	if !reflect.DeepEqual(problem.NextActions[0].Argv, want) {
		t.Fatalf("argv=%#v", problem.NextActions[0].Argv)
	}
	if !problem.NextActions[0].Mutates || problem.NextActions[0].SideEffectClass != output.ExternalSideEffect {
		t.Fatalf("missing-file repair was marked read-only: %#v", problem.NextActions[0])
	}
}

func TestPromptFileLiteralTildeReportsUnexpandedShellPath(t *testing.T) {
	root := testutil.TempDir(t)
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	_, problem := a.loadPromptForCommand("delegate", runOptions{cwd: root, promptFile: "~/workspace/omi/prompt-a.md", promptDelivery: "argv",
		promptRepairArgv: []string{"agentctl", "delegate", "--request-file", "request.json"}})
	if problem == nil || problem.Code != output.CodeNotFound {
		t.Fatalf("problem=%#v", problem)
	}
	if problem.Details["tilde_unexpanded"] != true || problem.Details["suggested_path"] != "$HOME/workspace/omi/prompt-a.md" {
		t.Fatalf("details=%#v", problem.Details)
	}
	if !strings.Contains(problem.Message, "shell did not expand ~") {
		t.Fatalf("message=%q", problem.Message)
	}
	if len(problem.NextActions) == 0 || problem.NextActions[0].Argv[len(problem.NextActions[0].Argv)-1] != "$HOME/workspace/omi/prompt-a.md" {
		t.Fatalf("actions=%#v", problem.NextActions)
	}
}

func TestRunPromptStdinRepairStaysAheadOfTheDelimiter(t *testing.T) {
	path := "/tmp/task.md"
	_, planned := promptStdinRepair("run", runOptions{promptRepairArgv: []string{"agentctl", "run", "--plan", "--label", "review", "--", "cursor-agent", "--plan"}}, path)
	wantPlan := []string{"agentctl", "run", "--plan", "--label", "review", "--prompt-stdin", "<", path, "--", "cursor-agent", "--plan"}
	if !reflect.DeepEqual(planned.Argv, wantPlan) || planned.Mutates || planned.SideEffectClass != output.ReadOnly {
		t.Fatalf("planned repair=%#v", planned)
	}
	_, live := promptStdinRepair("run", runOptions{promptRepairArgv: []string{"agentctl", "run", "--label", "review", "--", "cursor-agent", "--plan"}}, path)
	wantLive := []string{"agentctl", "run", "--label", "review", "--prompt-stdin", "<", path, "--", "cursor-agent", "--plan"}
	if !reflect.DeepEqual(live.Argv, wantLive) || !live.Mutates || live.SideEffectClass != output.ExternalSideEffect {
		t.Fatalf("live repair=%#v", live)
	}
}

func TestFanoutMissingAndUnresolvedPromptsStayOnTheManifest(t *testing.T) {
	root := testutil.TempDir(t)
	a := testApp(&bytes.Buffer{}, &bytes.Buffer{})
	missing := fanoutManifest{SchemaVersion: 1, PromptFile: "missing.md", Children: []fanoutChild{{Argv: []string{"/bin/echo"}}}}
	if _, _, problem := a.loadFanoutPrompts(missing, root); !fanoutManifestRepair(problem, output.CodeNotFound) {
		t.Fatalf("missing=%#v", problem)
	}
	child := fanoutManifest{SchemaVersion: 1, Children: []fanoutChild{{PromptFile: "missing.md", Argv: []string{"/bin/echo"}}}}
	if _, _, problem := a.loadFanoutPrompts(child, root); !fanoutManifestRepair(problem, output.CodeNotFound) {
		t.Fatalf("child=%#v", problem)
	}
	if err := os.WriteFile(filepath.Join(root, "notdir"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	blocked := fanoutManifest{SchemaVersion: 1, PromptFile: filepath.Join("notdir", "missing.md"), Children: []fanoutChild{{Argv: []string{"/bin/echo"}}}}
	if _, _, problem := a.loadFanoutPrompts(blocked, root); !fanoutManifestRepair(problem, output.CodeAuthorizationDenied) {
		t.Fatalf("unresolved=%#v", problem)
	}
	tilde, problem := a.loadPromptForCommand("fanout", runOptions{cwd: root, promptFile: "~/workspace/omi/prompt-a.md", promptDelivery: "argv"})
	if tilde != nil || !fanoutManifestRepair(problem, output.CodeNotFound) || problem.Details["tilde_unexpanded"] != true {
		t.Fatalf("tilde payload=%v problem=%#v", tilde, problem)
	}
}

func fanoutManifestRepair(problem *output.Error, code output.Code) bool {
	if problem == nil || problem.Code != code || problem.Details["recommended_prompt_source"] != "file" {
		return false
	}
	repair, ok := problem.Details["repair"].(map[string]any)
	if !ok || repair["manual_action"] == nil || repair["set_flags"] != nil || repair["remove_flags"] != nil {
		return false
	}
	if !strings.Contains(fmt.Sprint(repair["explanation"]), "does not accept --prompt-stdin") {
		return false
	}
	return len(problem.NextActions) == 1 && reflect.DeepEqual(problem.NextActions[0].Argv, []string{"agentctl", "help", "fanout"}) && !problem.NextActions[0].Mutates
}
