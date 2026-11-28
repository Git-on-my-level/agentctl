package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
)

func TestCodexFreshHomeSessionCanContinue(t *testing.T) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(root, "repo")
	if err := os.Mkdir(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.Command("git", "init", "--quiet", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	launcher := filepath.Join(root, "codex")
	resumeMarker := filepath.Join(root, "resume-read-rollout")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'codex-cli 0.170.0\n'
  exit 0
fi
thread_id='550e8400-e29b-41d4-a716-446655440005'
rollout="$CODEX_HOME/sessions/2026/11/25/rollout-2026-11-25T00-00-00-00000000-$thread_id.jsonl"
case " $* " in
  *" exec resume "*)
    [ -f "$rollout" ] || exit 75
    grep -Fq 'first-turn-rollout' "$rollout" || exit 76
    printf resumed > "$SYNTHETIC_RESUME_MARKER"
    answer='resumed answer'; rollout_text='resumed answer';;
  *)
    mkdir -p "$(dirname "$rollout")"
    printf '%s\n' "{\"type\":\"session_meta\",\"payload\":{\"id\":\"$thread_id\"}}" > "$rollout"
    answer='first answer'; rollout_text='first-turn-rollout';;
esac
printf '%s\n' "{\"type\":\"thread.started\",\"thread_id\":\"$thread_id\"}"
sleep 0.05
printf '%s\n' "{\"type\":\"response_item\",\"payload\":{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"$rollout_text\"}]}}" >> "$rollout"
printf '%s\n' "{\"type\":\"item.completed\",\"thread_id\":\"$thread_id\",\"item\":{\"type\":\"agent_message\",\"text\":\"$answer\"}}"
printf '%s\n' "{\"type\":\"turn.completed\",\"thread_id\":\"$thread_id\"}"
`
	if err := os.WriteFile(launcher, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	baseHome := filepath.Join(root, "fresh-codex-home")
	t.Setenv("CODEX_HOME", baseHome)
	t.Setenv("SYNTHETIC_RESUME_MARKER", resumeMarker)
	configPath := filepath.Join(root, "config.json")
	value := config.Config{
		SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
			Adapters:         map[string]config.Adapter{"codex": {Executable: launcher}},
			Delegation:       &config.DelegationPolicy{UnattendedCodingPermissions: true},
			AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "codex", Family: "synthetic", Model: "synthetic-model", Default: true}}},
		}},
	}
	configBytes, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	requestPath := filepath.Join(root, "request.json")
	if err := os.WriteFile(requestPath, []byte(`{"schema_version":1,"request_key":"first-turn","selector":{"model":"synthetic-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	firstCode, firstOutput := runCodexFixtureCommand(t, configPath, journalPath, "first task", []string{"delegate", "--cwd", repo, "--request-file", requestPath, "--prompt-stdin", "--wait"})
	if firstCode != 0 || !strings.Contains(firstOutput, "first answer") {
		t.Fatalf("first Codex delegate exit=%d output=%s", firstCode, firstOutput)
	}
	rollout := filepath.Join(baseHome, "sessions", "2026", "11", "25", "rollout-2026-11-25T00-00-00-00000000-550e8400-e29b-41d4-a716-446655440005.jsonl")
	if got, err := os.ReadFile(rollout); err != nil || !strings.Contains(string(got), "first-turn-rollout") {
		t.Fatalf("first-turn rollout was not retained in fresh home: %q err=%v", got, err)
	}
	firstID := resultID(t, firstOutput)
	resumeCode, resumeOutput := runCodexFixtureCommand(t, configPath, journalPath, "review feedback", []string{"continue", firstID, "--request-key", "second-turn", "--prompt-stdin", "--wait"})
	if resumeCode != 0 || !strings.Contains(resumeOutput, "resumed answer") {
		t.Fatalf("Codex continue exit=%d output=%s", resumeCode, resumeOutput)
	}
	if _, err := os.Stat(resumeMarker); err != nil {
		t.Fatalf("Codex resume did not read the persisted rollout: %v", err)
	}
}

func runCodexFixtureCommand(t *testing.T, configPath, journalPath, stdin string, args []string) (int, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader(stdin)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	command := append([]string{"--output", "json", "--config", configPath, "--journal", journalPath}, args...)
	code := a.run(ctx, command)
	if code != 0 && stdout.Len() == 0 {
		t.Fatalf("Codex fixture command had no output: %s", stderr.String())
	}
	return code, stdout.String()
}
