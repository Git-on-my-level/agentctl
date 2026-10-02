package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const recentSecretPrompt = "recent-export-private-prompt-token"
const recentSecretResult = "recent-export-private-result-token"

// This is a local shell output fixture, not an agent invocation. Run admission
// and normal terminal persistence establish a real uncollected result without
// dereferencing it through the result command (which would acknowledge it).
func seedRecentSecretRun(t *testing.T, path, caller string) model.Execution {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	baseGetenv := a.getenv
	a.getenv = func(key string) string {
		if key == adapter.CallerHarnessEnv {
			return caller
		}
		return baseGetenv(key)
	}
	a.stdin = strings.NewReader(recentSecretPrompt)
	args := []string{"--journal", path, "run", "--adapter", "generic-process", "--label", "export-privacy", "--prompt-stdin", "--prompt-delivery", "stdin", "--", "/bin/sh", "-c", `cat >/dev/null; printf '%s\n' "$1"`, "_", `{"type":"result","status":"completed","result":"` + recentSecretResult + `"}`}
	if code := a.run(context.Background(), args); code != 0 {
		t.Fatalf("seed exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var document struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if document.Result.State != model.StateCompleted {
		t.Fatalf("fixture is not terminal: %s", stdout.String())
	}
	return document.Result
}

type recentFixtureFile struct {
	Mode   fs.FileMode
	SHA256 [sha256.Size]byte
}

func recentFixtureSnapshot(t *testing.T, root string) map[string]recentFixtureFile {
	t.Helper()
	result := map[string]recentFixtureFile{}
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		item := recentFixtureFile{Mode: info.Mode()}
		if !entry.IsDir() {
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			item.SHA256 = sha256.Sum256(data)
		}
		result[path] = item
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func recentAcknowledgements(t *testing.T, path string) store.AcknowledgementIndex {
	t.Helper()
	journal, err := store.Open(path, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	index, err := journal.AcknowledgementIndex(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return index
}

func TestRecentExportTerminalPrivacyAndNoWritesAcrossCallerFilters(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "state", "journal.db")
	callers := []string{"hermes", "claude-code", "codex", "cursor", "omp", "zcode", "devin", "other", "unknown"}
	expected := map[string]int{}
	for _, caller := range callers {
		declaration := caller
		if caller == "unknown" {
			declaration = ""
		}
		seedRecentSecretRun(t, path, declaration)
		expected[caller]++
	}
	// A caller-specific filter must really page, and an existing ack must be
	// preserved alongside the terminals that remain uncollected.
	uncollected := seedRecentSecretRun(t, path, "hermes")
	expected["hermes"]++
	acknowledged := seedRecentSecretRun(t, path, "hermes")
	expected["hermes"]++
	journal, err := store.Open(path, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := journal.AcknowledgeExecution(context.Background(), acknowledged.ID, store.AcknowledgementResult); err != nil {
		t.Fatal(err)
	}
	outcome, err := journal.GetOutcome(context.Background(), uncollected.ID)
	if err != nil || outcome.Content == nil || outcome.Content.Text != recentSecretResult {
		t.Fatalf("fixture lacks stored secret result: outcome=%#v err=%v", outcome, err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}
	beforeAcks := recentAcknowledgements(t, path)
	if len(beforeAcks.ByID) != 1 || !beforeAcks.Unreconciled(uncollected) {
		t.Fatalf("fixture lacks acknowledged and uncollected terminals: %#v", beforeAcks)
	}
	raw, err := os.ReadFile(path)
	if err != nil || !bytes.Contains(raw, []byte(recentSecretResult)) || bytes.Contains(raw, []byte(recentSecretPrompt)) {
		t.Fatalf("fixture result retention or prompt privacy failed: %v", err)
	}
	beforeFiles := recentFixtureSnapshot(t, root)
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	maintenanceCalled := false
	a.updateNotice = func(context.Context, string, common) *output.Warning {
		maintenanceCalled = true
		return nil
	}
	// Invalid ambient identity must neither alter historical attribution nor
	// reject this read-only export or disclose environment text.
	baseGetenv := a.getenv
	a.getenv = func(key string) string {
		if key == adapter.CallerHarnessEnv {
			return "private-ambient-caller-token"
		}
		return baseGetenv(key)
	}
	invoke := func(mode string, args []string) recentExportDocument {
		t.Helper()
		stdout.Reset()
		stderr.Reset()
		argv := append([]string{"--output", mode, "--journal", path, "recent"}, args...)
		if code := a.run(context.Background(), argv); code != 0 {
			t.Fatalf("export exit=%d args=%q output=%s stderr=%s", code, args, stdout.String(), stderr.String())
		}
		for _, secret := range []string{recentSecretPrompt, recentSecretResult, "private-ambient-caller-token"} {
			if strings.Contains(stdout.String()+stderr.String(), secret) {
				t.Fatalf("export disclosed %q", secret)
			}
		}
		if maintenanceCalled {
			t.Fatal("read-only export invoked automatic maintenance")
		}
		var document recentExportDocument
		if mode == "json" {
			if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
				t.Fatal(err)
			}
		}
		return document
	}
	for _, onlyUncollected := range []bool{false, true} {
		for _, caller := range append([]string{""}, callers...) {
			args := []string{"--state", "terminal", "--label", "export-privacy", "--limit", "1"}
			wantByCaller := map[string]int{}
			for key, count := range expected {
				if onlyUncollected && key == "hermes" {
					count--
				}
				if caller == "" || caller == key {
					wantByCaller[key] = count
				}
			}
			wantTotal := 0
			for _, count := range wantByCaller {
				wantTotal += count
			}
			if onlyUncollected {
				args = append(args, "--unreconciled")
			}
			if caller != "" {
				args = append(args, "--caller", caller)
			}
			seen := map[string]bool{}
			cursor := ""
			for page := 0; page < wantTotal; page++ {
				current := append([]string{}, args...)
				if cursor != "" {
					current = append(current, "--cursor", cursor)
				}
				doc := invoke("json", current)
				if doc.Result.Total != wantTotal || doc.Result.Count != 1 {
					t.Fatalf("caller=%q total=%d count=%d want=%d", caller, doc.Result.Total, doc.Result.Count, wantTotal)
				}
				item := doc.Result.Executions[0]
				if seen[item.ID.String()] || (onlyUncollected && !item.Unreconciled) {
					t.Fatalf("duplicate or collected export item: %#v", item)
				}
				seen[item.ID.String()] = true
				gotCaller := "unknown"
				if item.Caller != nil {
					gotCaller = string(item.Caller.Harness)
					if item.Caller.Provenance != model.CallerDeclared {
						t.Fatalf("caller provenance lost: %#v", item.Caller)
					}
				}
				if caller != "" && gotCaller != caller {
					t.Fatalf("caller=%q emitted %q", caller, gotCaller)
				}
				cursor = doc.Result.Next
				if doc.Result.HasMore != (page+1 < wantTotal) || (cursor != "") != doc.Result.HasMore {
					t.Fatal("pagination failed to cover the selected terminal set")
				}
			}
			summaryArgs := append(append([]string{}, args...), "--summary")
			summary := invoke("json", summaryArgs)
			if summary.Result.Total != wantTotal || summary.Result.Count != 0 || summary.Result.HasMore || !reflect.DeepEqual(summary.Result.Summary.ByCaller, wantByCaller) {
				t.Fatalf("summary truncated or changed callers: %#v want=%#v", summary.Result, wantByCaller)
			}
			invoke("text", args)
			invoke("text", summaryArgs)
		}
	}
	if after := recentAcknowledgements(t, path); !reflect.DeepEqual(beforeAcks, after) {
		t.Fatalf("export changed acknowledgement index: before=%#v after=%#v", beforeAcks, after)
	}
	if after := recentFixtureSnapshot(t, root); !reflect.DeepEqual(beforeFiles, after) {
		t.Fatal("read-only export changed journal bytes, file modes, or directory contents")
	}
}
