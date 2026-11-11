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
	"github.com/Git-on-my-level/agentctl/internal/testutil"
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
	journal := filepath.Join(testutil.TempDir(t), "state", "journal.db")
	script := writeOpenCodeFixture(t, testutil.TempDir(t))
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
	script := writeOpenCodeFixture(t, testutil.TempDir(t))
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
	root := testutil.TempDir(t)
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
	root := testutil.TempDir(t)
	f := openCodeDelegateFixture{root: root, config: filepath.Join(root, "config.json"), journal: filepath.Join(root, "state", "journal.db"), request: filepath.Join(root, "request.json"), count: filepath.Join(root, "launches"), probe: filepath.Join(root, "probes")}
	script := "#!/bin/sh\nif [ \"$1\" = '--version' ]; then\n echo probe >> '" + f.probe + "'\n echo 'opencode 1.17.13'\n exit 0\nfi\necho launch >> '" + f.count + "'\nprintf '%s\\n' '{\"type\":\"text\",\"sessionID\":\"ses_delegate\",\"part\":{\"type\":\"text\",\"text\":\"delegate answer\",\"time\":{\"end\":2}}}' '{\"type\":\"step_finish\",\"sessionID\":\"ses_delegate\",\"part\":{\"type\":\"step-finish\",\"reason\":\"stop\",\"tokens\":{\"input\":3685,\"output\":3,\"reasoning\":0,\"cache\":{\"read\":7168,\"write\":0}}}}'\n"
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
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
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
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", f.journal, "recent", "--summary"}); code != 0 {
		t.Fatalf("summary exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var summary recentExportDocument
	if err := json.Unmarshal(stdout.Bytes(), &summary); err != nil {
		t.Fatal(err)
	}
	usage := summary.Result.Summary.TokenUsageByModel["zai/glm-5.3"]
	if usage.Input != 3685 || usage.Output != 3 || usage.CacheRead != 7168 || usage.CacheWrite != 0 || usage.Reasoning != 0 {
		t.Fatalf("model token usage = %#v; summary=%s", usage, stdout.String())
	}
	assertResultMatchesSchemaShape(t, stdout.String(), "recent-result.schema.json", "summary")
}

func TestDelegateOpenCodeUnattendedCodingAddsAuto(t *testing.T) {
	f := newOpenCodeDelegateFixture(t)
	var config map[string]any
	data, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &config); err != nil {
		t.Fatal(err)
	}
	profile := config["profiles"].(map[string]any)["test"].(map[string]any)
	profile["delegation"] = map[string]any{"unattended_coding_permissions": true}
	data, err = json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, data, 0o600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("private-task-token", "--plan")
	if code != 0 {
		t.Fatalf("plan exit=%d %s", code, out)
	}
	if !strings.Contains(out, `"--auto"`) || !strings.Contains(out, `"permissions":"constrained"`) {
		t.Fatalf("OpenCode unattended plan = %s", out)
	}
	if err := os.WriteFile(f.request, []byte(`{"schema_version":1,"request_key":"test-opencode-read-only","selector":{"family":"oc","settings":{"access":"read_only"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out = f.invoke("private-task-token", "--plan")
	if code == 0 || !strings.Contains(out, "access_unavailable") || strings.Contains(out, `"--auto"`) {
		t.Fatalf("OpenCode read_only plan exit=%d output=%s", code, out)
	}
}

func TestOpenCodeRepeatedStepClearsRetryProjection(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "opencode")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode 2.2.6'; exit 0; fi
printf '%s\n' '{"type":"step_start","sessionID":"ses_retry","part":{"type":"step-start"}}' '{"type":"retry","sessionID":"ses_retry","attempt":1,"error":{"type":"provider.rate-limit","status":429}}' '{"type":"step_start","sessionID":"ses_retry","part":{"type":"step-start"}}' 'not-json'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--", path, "run", "--format", "json"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	id := executionIDFromOutput(t, stdout.String())
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "result", id}); code != 0 {
		t.Fatalf("result exit=%d %s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), `"rate_limit"`) || strings.Contains(stdout.String(), `"code":"provider_capacity"`) {
		t.Fatalf("repeated step left stale capacity/retry metadata: %s", stdout.String())
	}
}

func TestOpenCodeIncompleteRunAfterRetryKeepsLocalDiagnostic(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "opencode")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode 2.2.6'; exit 0; fi
printf '%s\n' '{"type":"retry","timestamp":1794251075486,"sessionID":"ses_incomplete","attempt":2,"nextRetryAt":1794251101486,"error":{"type":"sk-live-9f8e7d6c5b4a","message":"Rate limit exceeded; private-request-details","status":429,"retryAfterMs":26000}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--", path, "run", "--format", "json"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	id := executionIDFromOutput(t, stdout.String())
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "status", id}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"rate_limit"`, `"attempt":2`, `"retry_after_ms":26000`, `"provider_error_type":"unclassified"`, `"code":"native_execution_failed"`, `"retryable":false`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "provider_capacity") || strings.Contains(stdout.String(), "sk-live-9f8e7d6c5b4a") || strings.Contains(stdout.String(), "private-request-details") {
		t.Fatalf("incomplete run inherited capacity/secret evidence: %s", stdout.String())
	}
}

func TestOpenCodeRateLimitSurfacesMetadataAndCapacity(t *testing.T) {
	root := testutil.TempDir(t)
	path := filepath.Join(root, "opencode")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 'opencode 2.2.6'; exit 0; fi
printf '%s\n' '{"type":"retry","timestamp":1794251075486,"sessionID":"ses_rate_limit","attempt":2,"nextRetryAt":1794251101486,"error":{"type":"provider.rate-limit","message":"Rate limit exceeded; private-request-details","status":429,"retryAfterMs":26000}}' '{"type":"error","sessionID":"ses_rate_limit","error":{"type":"provider.rate-limit","message":"Rate limit exceeded; private-request-details","status":429}}'
`
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	journal := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "run", "--", path, "run", "--format", "json"}); code != 0 {
		t.Fatalf("run exit=%d output=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var document struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	execution := document.Result
	if execution.State != model.StateFailed || execution.RateLimit == nil {
		t.Fatalf("terminal execution = %#v", execution)
	}
	rateLimit := execution.RateLimit
	if rateLimit.Attempt != 2 || rateLimit.RetryAfterMS != 26000 || rateLimit.ProviderErrorType != "provider.rate-limit" || rateLimit.HTTPStatus != 429 || rateLimit.NextRetryAt == nil {
		t.Fatalf("rate limit = %#v", rateLimit)
	}
	if strings.Contains(stdout.String(), "private-request-details") {
		t.Fatalf("provider message leaked into run: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "status", execution.ID.String()}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"rate_limit"`, `"attempt":2`, `"retry_after_ms":26000`, `"http_status":429`, `"provider_error_type":"provider.rate-limit"`, `"code":"provider_capacity"`, `"category":"capacity"`, `"retryable":true`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status missing %s: %s", want, stdout.String())
		}
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "events", execution.ID.String()}); code != 0 {
		t.Fatalf("events exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"source_state":"opencode.retry"`, `"attempt":2`, `"retry_after_ms":26000`, `"http_status":429`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("events missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "private-request-details") || strings.Contains(stdout.String(), "Rate limit exceeded") {
		t.Fatalf("provider message leaked into events: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(context.Background(), []string{"--output", "json", "--journal", journal, "await", execution.ID.String()}); code != 11 {
		t.Fatalf("await exit=%d output=%s", code, stdout.String())
	}
	if !strings.Contains(stdout.String(), `"rate_limit"`) || !strings.Contains(stdout.String(), `"retry_after_ms":26000`) || strings.Contains(stdout.String(), "private-request-details") {
		t.Fatalf("await rate-limit metadata/privacy = %s", stdout.String())
	}
}

func TestOpenCodeDelegatedResumeCapabilityIsSeparateFromSteer(t *testing.T) {
	f := opencodeRecoveryFixture(t, "", true)
	code, out := f.invoke("original-private-task", "--wait")
	if code != 0 {
		t.Fatalf("delegate %d: %s", code, out)
	}
	_, out = f.command(t, "", "status", resultID(t, out))
	var doc struct {
		Result model.Execution `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	resume, steer := false, false
	for _, item := range doc.Result.Capabilities.Items {
		if item.Name == "resume" {
			resume = item.Status == model.CapabilitySupported
		}
		if item.Name == "steer" {
			steer = item.Status == model.CapabilityUnavailable
		}
	}
	if !resume || !steer {
		t.Fatalf("capabilities: %#v", doc.Result.Capabilities)
	}
}
