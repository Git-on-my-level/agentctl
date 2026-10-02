package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func TestPromptOutsideRootRepairsMatchCommandParsers(t *testing.T) {
	root, outside := t.TempDir(), t.TempDir()
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
		{"delegate", "delegate", "", []string{"--cwd", root, "--request-file", request, "--prompt-file", path, "--authority", "native", "--plan"}},
		{"continue", "continue", "", []string{ref, "--request-key", "repair-test", "--prompt-file", path, "--plan"}},
		{"dispatch", "dispatch", "", []string{"--route", "example", "--title", "Repair test", "--idempotency-key", "repair-test", "--prompt-file", path, "--plan"}},
		{"steer", "steer", "", []string{ref, "--prompt-file", path, "--plan"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			input := strings.NewReader("unread-input")
			a.stdin = input
			journal := filepath.Join(t.TempDir(), "state", "journal.db")
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
			if len(problem.NextActions) != 1 || !reflect.DeepEqual(problem.NextActions[0].Argv, []string{"agentctl", "help", tt.command}) || problem.NextActions[0].Mutates || problem.NextActions[0].SideEffectClass != output.ReadOnly {
				t.Fatalf("next_actions=%#v", problem.NextActions)
			}
			repaired := applyPromptRepairFlags(t, tt.args, repair)
			var parseProblem *output.Error
			switch tt.command {
			case "run":
				opts, problem := parseRun(repaired)
				parseProblem = problem
				if opts.promptDelivery != tt.delivery || !reflect.DeepEqual(opts.argv, []string{"/bin/echo"}) {
					t.Fatalf("repair changed native arguments or delivery: %#v", opts)
				}
			case "delegate":
				_, parseProblem = parseDelegate(repaired)
			case "continue":
				_, _, _, parseProblem = parseContinue(repaired)
			case "dispatch":
				_, parseProblem = parseDispatch(repaired)
			case "steer":
				_, _, parseProblem = parseSteer(repaired)
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

func TestFanoutOutsideRootRepairUsesManifestGuidance(t *testing.T) {
	for _, childPrompt := range []bool{false, true} {
		t.Run(map[bool]string{false: "shared", true: "child"}[childPrompt], func(t *testing.T) {
			root, outside := t.TempDir(), t.TempDir()
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
