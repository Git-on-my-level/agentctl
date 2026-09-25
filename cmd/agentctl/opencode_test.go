package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
)

func writeOpenCodeFixture(t *testing.T, dir string) string {
	t.Helper()
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then
  printf 'opencode 1.17.13\n'
  exit 0
fi
printf '%s\n' '{"type":"step_start","sessionID":"ses_cli","part":{"type":"step-start"}}' '{"type":"tool_use","sessionID":"ses_cli","part":{"type":"tool","tool":"bash","input":{"command":"cat secret-file"},"output":"tool-secret-output"}}' '{"type":"text","sessionID":"ses_cli","part":{"type":"text","text":"fixture answer","time":{"start":1,"end":2}}}' '{"type":"step_finish","sessionID":"ses_cli","part":{"type":"step-finish","reason":"stop"}}'
`
	path := filepath.Join(dir, "opencode")
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestOpenCodeRunInfersAdapterAndResultIsRetrievable(t *testing.T) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	journal := filepath.Join(t.TempDir(), "state", "journal.db")
	script := writeOpenCodeFixture(t, t.TempDir())
	code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--", script, "run", "--format", "json"})
	if code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var runDoc struct {
		OK     bool            `json:"ok"`
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &runDoc); err != nil {
		t.Fatal(err)
	}
	if !runDoc.OK || runDoc.Result.State != model.StateCompleted || runDoc.Result.Adapter != "opencode" {
		t.Fatalf("run doc=%#v", runDoc.Result)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", runDoc.Result.ID.String(), "--require-content"}); code != 0 {
		t.Fatalf("result exit=%d output=%s", code, stdout.String())
	}
	var resultDoc struct {
		Result struct {
			Outcome model.Outcome `json:"outcome"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &resultDoc); err != nil {
		t.Fatal(err)
	}
	if resultDoc.Result.Outcome.Content == nil || resultDoc.Result.Outcome.Content.Text != "fixture answer" {
		t.Fatalf("outcome=%#v", resultDoc.Result.Outcome)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "events", runDoc.Result.ID.String()}); code != 0 {
		t.Fatalf("events exit=%d output=%s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "secret-file") || strings.Contains(stdout.String(), "tool-secret-output") {
		t.Fatalf("tool payload leaked into events: %s", stdout.String())
	}
	raw, err := os.ReadFile(journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("tool-secret-output")) || bytes.Contains(raw, []byte("secret-file")) {
		t.Fatal("tool payload entered journal")
	}
}

func TestOpenCodeCapabilitiesStaticAndFixtureProbe(t *testing.T) {
	script := writeOpenCodeFixture(t, t.TempDir())
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "capabilities", "opencode", "--static", "--require", "launch,result_content"}); code != 0 {
		t.Fatalf("static exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"viable":true`) || !strings.Contains(stdout.String(), `"result_content"`) {
		t.Fatalf("static summary=%s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "capabilities", "opencode", "--executable", script, "--require", "launch,result_content"}); code != 0 {
		t.Fatalf("probe exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), `"viable":true`) || !strings.Contains(stdout.String(), `"probed_at"`) {
		t.Fatalf("probe summary=%s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "capabilities", "not-a-real-adapter", "--static"}); code == 0 {
		t.Fatalf("unknown adapter succeeded: %s", stdout.String())
	}
	if !strings.Contains(stdout.String(), `"opencode"`) {
		t.Fatalf("known adapters missing opencode: %s", stdout.String())
	}
}

func TestRouteExplainOpenCodeSelectors(t *testing.T) {
	root := t.TempDir()
	builtinConfig := filepath.Join(root, "builtin.json")
	if err := os.WriteFile(builtinConfig, []byte(`{"schema_version":1,"default_profile":"fleet","profiles":{"fleet":{"route":{"this_host":"m4-air"}}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--config", builtinConfig, "route", "explain", "--output", "json", "--", "opencode"}); code != 0 {
		t.Fatalf("builtin exit=%d output=%s", code, stdout.String())
	}
	var doc struct {
		Result struct {
			Models []struct {
				Adapter string `json:"adapter"`
				Model   string `json:"model"`
			} `json:"models"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Result.Models) == 0 || doc.Result.Models[0].Adapter != "opencode" {
		t.Fatalf("builtin models=%#v", doc.Result.Models)
	}
	configPath := filepath.Join(root, "config.json")
	raw := `{"schema_version":1,"default_profile":"fleet","profiles":{"fleet":{"agent_preferences":{"mode":"advisory","preferred":[{"agent":"opencode","model":"zai/glm-5.3","use_for":"alias:oc"}]},"route":{"this_host":"m4-air"}}}}`
	if err := os.WriteFile(configPath, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--config", configPath, "route", "explain", "--output", "json", "--", "oc"}); code != 0 {
		t.Fatalf("alias exit=%d output=%s", code, stdout.String())
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Result.Models) == 0 || doc.Result.Models[0].Adapter != "opencode" || doc.Result.Models[0].Model != "zai/glm-5.3" {
		t.Fatalf("preferred models=%#v", doc.Result.Models)
	}
}

type openCodeDelegateFixture struct{ root, config, journal, request, count, probe string }

func newOpenCodeDelegateFixture(t *testing.T) openCodeDelegateFixture {
	t.Helper()
	root := t.TempDir()
	f := openCodeDelegateFixture{root: root, config: filepath.Join(root, "config.json"), journal: filepath.Join(root, "state", "journal.db"), request: filepath.Join(root, "request.json"), count: filepath.Join(root, "launches"), probe: filepath.Join(root, "probes")}
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then\n echo probe >> '" + f.probe + "'\n echo 'opencode 1.17.13'\n exit 0\nfi\necho launch >> '" + f.count + "'\nprintf '%s\\n' '{\"type\":\"text\",\"sessionID\":\"ses_delegate\",\"part\":{\"type\":\"text\",\"text\":\"delegate answer\",\"time\":{\"end\":2}}}'\n"
	if err := os.WriteFile(filepath.Join(root, "opencode"), []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	value := map[string]any{"schema_version": 1, "default_profile": "test", "profiles": map[string]any{"test": map[string]any{
		"adapters":          map[string]any{"opencode": map[string]any{"executable": filepath.Join(f.root, "opencode")}},
		"route":             map[string]any{"this_host": "workstation"},
		"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "opencode", "model": "zai/glm-5.3", "use_for": "alias:oc", "default": true}}},
	}}}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-opencode-1","selector":{"family":"oc"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f openCodeDelegateFixture) invoke(prompt string, extra ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader(prompt)
	args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	args = append(args, extra...)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	code := a.run(ctx, args)
	return code, stdout.String()
}

func TestDelegateOpenCodePlanAndCollect(t *testing.T) {
	f := newOpenCodeDelegateFixture(t)
	code, out := f.invoke("private-task-token", "--plan")
	if code != 0 {
		t.Fatalf("plan exit=%d %s", code, out)
	}
	if strings.Contains(out, "private-task-token") || !strings.Contains(out, `"model":"zai/glm-5.3"`) || !strings.Contains(out, `"harness":"opencode"`) || !strings.Contains(out, `"side_effect_class":"read_only"`) {
		t.Fatalf("plan=%s", out)
	}
	for _, p := range []string{f.journal, filepath.Dir(f.journal), f.count} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("plan created %s: %v", p, err)
		}
	}
	code, out = f.invoke("private-task-token", "--wait")
	if code != 0 || !strings.Contains(out, `"text":"delegate answer"`) || !strings.Contains(out, `"harness":"opencode"`) {
		t.Fatalf("wait exit=%d %s", code, out)
	}
	if strings.Contains(out, "private-task-token") {
		t.Fatal("prompt leaked")
	}
	raw, _ := os.ReadFile(f.journal)
	if bytes.Contains(raw, []byte("private-task-token")) {
		t.Fatal("prompt in journal")
	}
}
