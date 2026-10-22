package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestClassifyCapacityIsRetryableAndGenericProviderIsNot(t *testing.T) {
	code, kind, category, retryable := classifyOutcomeFailure("Selected model is at capacity. Please try a different model.", "", "", "codex", "")
	if code != "provider_capacity" || kind != "capacity" || category != "capacity" || !retryable {
		t.Fatalf("capacity=%s %s %s %v", code, kind, category, retryable)
	}
	code, kind, category, retryable = classifyOutcomeFailure("the turn stopped", "", "other", "codex", "")
	if code != "native_execution_failed" || kind != "provider" || category != "provider" || retryable {
		t.Fatalf("generic=%s %s %s %v", code, kind, category, retryable)
	}
	code, _, category, retryable = classifyOutcomeFailure("native process exited unsuccessfully", "", "", "codex", "")
	if code != "native_execution_failed" || category != "" || retryable {
		t.Fatalf("unstructured=%s %s %v", code, category, retryable)
	}
	code, kind, category, retryable = classifyOutcomeFailure("ZCode provider usage limit reached", "", "1308", "zcode", "")
	if code != "provider_capacity" || kind != "capacity" || category != "capacity" || !retryable || !model.ValidNativeFailureCode("1308") {
		t.Fatalf("ZCode usage limit=%s %s %s %v", code, kind, category, retryable)
	}
	code, kind, category, retryable = classifyOutcomeFailure("provider returned a rate limit response", "", "", "codex", "")
	if code != "quota_exceeded" || kind != "quota" || category != "" || !retryable {
		t.Fatalf("generic ZCode rate limit=%s %s %s %v", code, kind, category, retryable)
	}
}

func TestCodexFailureReasonSurfacesWithoutAnswerOrSecret(t *testing.T) {
	root := testutil.TempDir(t)
	bin := filepath.Join(root, "codex")
	script := `#!/bin/sh
if [ "$1" = "--version" ]; then echo 0.0.0-test; exit 0; fi
printf '%s\n' '{"type":"item","item":{"type":"agent_message","text":"do-not-store-this-answer"}}'
printf '%s\n' '{"type":"turn.failed","thread_id":"thread-1","error":{"message":"request rejected: {\"api_key\":\"private-value\",\"prompt\":\"confidential task\"} Selected model is at capacity.","codex_error_info":"server_overloaded"}}'
`
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	journalPath := filepath.Join(root, "state", "journal.db")
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "run", "--adapter", "codex", "--", bin, "exec", "--json", "task"}); code != 0 {
		t.Fatalf("run exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") {
		t.Fatalf("run leaked answer or secret: %s", stdout.String())
	}
	id := executionIDFromOutput(t, stdout.String())
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "status", id}); code != 0 {
		t.Fatalf("status exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"category":"capacity"`, `"native_code":"server_overloaded"`, `"retryable":true`, failureMessageCapacity} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("status missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") {
		t.Fatalf("status leaked: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "result", id}); code != 0 {
		t.Fatalf("result exit=%d %s", code, stdout.String())
	}
	for _, want := range []string{`"native_code":"server_overloaded"`, `"category":"capacity"`, `"retryable":true`} {
		if !strings.Contains(stdout.String(), want) {
			t.Fatalf("result missing %s: %s", want, stdout.String())
		}
	}
	if strings.Contains(stdout.String(), "do-not-store-this-answer") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential task") || !strings.Contains(stdout.String(), failureMessageCapacity) {
		t.Fatalf("result leaked or dropped the capacity reason: %s", stdout.String())
	}
	stdout.Reset()
	if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, "events", id}); code != 0 {
		t.Fatalf("events exit=%d %s", code, stdout.String())
	}
	if strings.Contains(stdout.String(), "at capacity") || strings.Contains(stdout.String(), "do-not-store") || strings.Contains(stdout.String(), "private-value") || strings.Contains(stdout.String(), "confidential") {
		t.Fatalf("events leaked the failure message: %s", stdout.String())
	}
	parsed, err := ids.ParseExecutionID(id)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(journalPath, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	execution, err := journal.GetExecution(ctx, parsed)
	journal.Close()
	if err != nil {
		t.Fatal(err)
	}
	problem := a.delegateFailedExecution(ctx, common{journalPath: journalPath}, execution)
	if problem == nil || problem.Code != output.CodeExecutionFailed || !problem.Retryable {
		t.Fatalf("delegate failure=%v", problem)
	}
	view, _ := problem.Details["failure"].(*nativeFailureView)
	if view == nil || view.NativeCode != "server_overloaded" || view.Category != "capacity" || view.Message != failureMessageCapacity || strings.Contains(view.Message, "private-value") {
		t.Fatalf("delegate details=%#v", problem.Details["failure"])
	}
	raw, err := os.ReadFile(journalPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte("private-value")) || bytes.Contains(raw, []byte("confidential task")) || bytes.Contains(raw, []byte("do-not-store-this-answer")) {
		t.Fatal("failure payload entered the journal")
	}
}

func TestZCodeFailureReasonSurfacesAcrossDelegateStatusAndResult(t *testing.T) {
	for _, tc := range []struct {
		name, stdoutShape, stderrShape, category, nativeCode, message string
		retryable                                                     bool
	}{
		{
			name: "quota",
			stderrShape: `ProviderBusinessError: [1308][Usage limit reached for 5 hour. Your limit will reset at 2026-10-15 19:38:47]
  code: 'PROVIDER_BUSINESS_ERROR', providerCode: '1308', responseStatus: 429, 'retry-after': '1266'`,
			category: "capacity", nativeCode: "1308", message: "ZCode provider usage limit reached; resets 2026-10-15 19:38:47", retryable: true,
		},
		{name: "HTTP429 with separate provider code", stderrShape: "ProviderBusinessError: providerCode: 7314 responseStatus: 429 provider request failed", category: "capacity", nativeCode: "7314", message: "ZCode provider usage limit reached", retryable: true},
		{name: "provider429 is not HTTP", stderrShape: "ProviderBusinessError: providerCode: 429 provider request failed", category: "provider", nativeCode: "429", message: failureMessageGeneric, retryable: false},
		{name: "bracket429 is not HTTP", stderrShape: "ProviderBusinessError: [429] provider request failed", category: "provider", nativeCode: "429", message: failureMessageGeneric, retryable: false},
		{name: "authentication outranks HTTP429", stderrShape: "ProviderBusinessError: providerCode: 429 responseStatus: 429 authentication required", nativeCode: "429", message: failureMessageAuth, retryable: true},
		{
			name:        "authentication JSON",
			stdoutShape: `{"error":{"message":"Authentication required; please login to continue (requestId: fixture-only traceId: fixture-only)"}}`,
			message:     failureMessageAuth, retryable: true,
		},
		{
			name:        "workspace trust stderr",
			stderrShape: "Workspace trust required before running the task (requestId: fixture-only traceId: fixture-only)",
			message:     failureMessageTrust, retryable: true,
		},
		{
			name: "unknown provider",
			stderrShape: `ProviderBusinessError: provider request failed
  code: 'PROVIDER_BUSINESS_ERROR', providerCode: '7314', responseStatus: 500`,
			category: "provider", nativeCode: "7314", message: failureMessageGeneric, retryable: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testutil.TempDir(t)
			bin := filepath.Join(root, "zcode")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo 0.16.9; exit 0; fi\n"
			if tc.stdoutShape != "" {
				script += "cat <<'RESULT'\n" + tc.stdoutShape + "\nRESULT\n"
			}
			if tc.stderrShape != "" {
				script += "cat >&2 <<'DIAGNOSTIC'\n" + tc.stderrShape + "\nError: Turn execution failed (traceId: fixture-only)\nDIAGNOSTIC\n"
			}
			script += "exit 1\n"
			if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
				t.Fatal(err)
			}
			configPath := filepath.Join(root, "config.json")
			cfg := config.Config{
				SchemaVersion:  1,
				DefaultProfile: "test",
				Profiles: map[string]config.Profile{"test": {
					Adapters: map[string]config.Adapter{"zcode": {Executable: bin}},
					AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{
						Agent: "zcode", Family: "glm", Model: "zai/glm-5.3", Effort: "high", Default: true,
					}}},
				}},
			}
			if err := config.Save(configPath, cfg, false); err != nil {
				t.Fatal(err)
			}
			requestPath := filepath.Join(root, "request.json")
			request := `{"schema_version":1,"request_key":"zcode-failure","selector":{"family":"glm"}}`
			if err := os.WriteFile(requestPath, []byte(request), 0o600); err != nil {
				t.Fatal(err)
			}

			journalPath := filepath.Join(root, "state", "journal.db")
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			a.stdin = strings.NewReader("synthetic task")
			code := a.run(ctx, []string{"--output", "json", "--config", configPath, "--journal", journalPath, "delegate", "--cwd", root, "--request-file", requestPath, "--prompt-stdin", "--wait"})
			if code != output.ExitCodeFor(output.CodeExecutionFailed) {
				t.Fatalf("delegate --wait exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
			}
			executionID := failureExecutionIDFromOutput(t, stdout.String())
			surfaces := []struct{ name, text string }{{name: "delegate --wait", text: stdout.String()}}
			for _, command := range []string{"status", "result"} {
				stdout.Reset()
				if code := a.run(ctx, []string{"--output", "json", "--journal", journalPath, command, executionID}); code != 0 {
					t.Fatalf("%s exit=%d stdout=%s stderr=%s", command, code, stdout.String(), stderr.String())
				}
				surfaces = append(surfaces, struct{ name, text string }{name: command, text: stdout.String()})
			}
			for _, surface := range surfaces {
				wants := []string{`"retryable":` + jsonBool(tc.retryable), tc.message}
				if tc.category != "" {
					wants = append(wants, `"category":"`+tc.category+`"`)
				}
				if tc.nativeCode != "" {
					wants = append(wants, `"native_code":"`+tc.nativeCode+`"`)
				}
				for _, want := range wants {
					if !strings.Contains(surface.text, want) {
						t.Fatalf("%s surface missing %s: %s", surface.name, want, surface.text)
					}
				}
				for _, forbidden := range []string{"ProviderBusinessError", "providerCode", "responseStatus", "retry-after", "request id", "requestId", "trace id", "traceId", "fixture-only"} {
					if strings.Contains(surface.text, forbidden) {
						t.Fatalf("%s surface leaked %q: %s", surface.name, forbidden, surface.text)
					}
				}
			}
			journalBytes, err := os.ReadFile(journalPath)
			if err != nil {
				t.Fatal(err)
			}
			for _, forbidden := range []string{"ProviderBusinessError", "providerCode", "responseStatus", "retry-after", "fixture-only"} {
				if bytes.Contains(journalBytes, []byte(forbidden)) {
					t.Fatalf("journal retained raw stderr marker %q", forbidden)
				}
			}
		})
	}
}

func failureExecutionIDFromOutput(t *testing.T, document string) string {
	t.Helper()
	const marker = `"execution_id":"exec-`
	start := strings.Index(document, marker)
	if start < 0 {
		t.Fatalf("no execution id in failure output: %s", document)
	}
	rest := document[start+len(`"execution_id":"`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("malformed execution id in failure output: %s", document)
	}
	return rest[:end]
}

func jsonBool(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func TestBuildOutcomeBoundsStructuredFailure(t *testing.T) {
	now := time.Date(2026, 10, 13, 0, 0, 0, 0, time.UTC)
	journal, err := store.Open(filepath.Join(testutil.TempDir(t), "state", "journal.db"), store.Options{Clock: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	execution, _, err := journal.CreateExecution(context.Background(), model.Execution{
		Authority: model.AuthorityNative, Adapter: "codex", Mode: model.ModeDirect, Acquisition: model.AcquisitionLaunched,
		State: model.StateRunning, Liveness: model.LivenessAlive, SourceBindings: []model.SourceBinding{},
		Capabilities: model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "test", Items: []model.CapabilityItem{}},
		Observation:  model.Observation{Source: model.ObservationNativeStream, Integrity: model.IntegrityVerified, ObservedAt: now},
	}, contracts.MutationKey{})
	if err != nil {
		t.Fatal(err)
	}
	secret := `request rejected: {"api_key":"private-value","prompt":"confidential task"}`
	execution.State = model.StateFailed
	outcome := buildOutcome(execution, adapter.Result{
		Success: false, State: adapter.StateFailed,
		Error: secret,
	}, now, false)
	if outcome.Failure == nil || outcome.Failure.Message != failureMessageGeneric || strings.Contains(outcome.Failure.Message, "private-value") || strings.Contains(outcome.Failure.Message, "confidential") {
		t.Fatalf("unknown failure was not allowlisted: %#v", outcome.Failure)
	}
	if err := outcome.Validate(); err != nil {
		t.Fatal(err)
	}
	view := projectNativeFailure(&model.OutcomeFailure{Code: "native_execution_failed", Kind: "unknown", Source: "codex", Message: secret})
	if view != nil && (strings.Contains(view.Message, "private-value") || strings.Contains(view.Message, "confidential")) {
		t.Fatalf("status projection kept provider text: %#v", view)
	}
	long := "server overloaded: " + strings.Repeat("x", 300) + " " + secret
	updated, err := finalizeResult(context.Background(), journal, execution, adapter.Result{
		Success: false, State: adapter.StateFailed, Error: long, Data: map[string]any{"native_code": "server_overloaded"},
	}, now, false)
	if err != nil || !updated.State.Terminal() {
		t.Fatalf("terminal commit failed: %v %#v", err, updated.State)
	}
	stored, err := journal.GetOutcome(context.Background(), execution.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Failure == nil || stored.Failure.Category != "capacity" || !stored.Failure.Retryable || stored.Failure.Message != failureMessageCapacity || len(stored.Failure.Message) > model.NativeFailureMessageLimit {
		t.Fatalf("stored=%#v", stored.Failure)
	}
	if strings.Contains(stored.Failure.Message, "private-value") || strings.Contains(stored.Failure.Message, "xxxx") {
		t.Fatalf("stored provider text: %s", stored.Failure.Message)
	}
	inferred := buildOutcome(execution, adapter.Result{
		Success: false, State: adapter.StateFailed,
		Error: strings.Repeat("x", 500) + " at capacity",
	}, now, false)
	if inferred.Failure == nil || inferred.Failure.Category != "" || inferred.Failure.NativeCode != "" || inferred.Failure.Message != failureMessageGeneric || len(inferred.Failure.Message) > model.NativeFailureMessageLimit || inferred.Failure.Retryable || strings.Contains(inferred.Failure.Message, "xxxx") {
		t.Fatalf("inferred=%#v", inferred.Failure)
	}
	if err := inferred.Validate(); err != nil {
		t.Fatal(err)
	}
}

func executionIDFromOutput(t *testing.T, document string) string {
	t.Helper()
	const marker = `"id":"exec-`
	start := strings.Index(document, marker)
	if start < 0 {
		t.Fatalf("no execution id in %s", document)
	}
	rest := document[start+len(`"id":"`):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatal(document)
	}
	return rest[:end]
}

func TestZCodeCapacityWordingRequiresUsageLimitProof(t *testing.T) {
	overload := model.OutcomeFailure{Source: "zcode", Code: "provider_capacity", Category: "capacity", NativeCode: "server_overloaded", Message: failureMessageCapacity}
	if got := allowlistedFailureMessage(overload); got != failureMessageCapacity {
		t.Fatalf("zcode overload must keep generic capacity wording, got %q", got)
	}
	limit := model.OutcomeFailure{Source: "zcode", Code: "provider_capacity", Category: "capacity", NativeCode: "1308", Message: "ZCode provider usage limit reached; resets 2026-10-15 19:38:47"}
	if got := allowlistedFailureMessage(limit); got != "ZCode provider usage limit reached; resets 2026-10-15 19:38:47" {
		t.Fatalf("zcode 1308 must keep usage-limit wording with reset, got %q", got)
	}
}

func TestStderrMetadataSurvivesOutcomeAndStatusWithoutSourceText(t *testing.T) {
	execution := model.Execution{Adapter: "codex", State: model.StateFailed}
	line := []byte("fatal: unable to process confidential user prompt opaque-token-Xb7sQ9")
	metadata := diagnostic.FromStderrLine(line)
	outcome := buildOutcome(execution, adapter.Result{State: adapter.StateFailed, Error: string(line), Data: map[string]any{"stderr_diagnostic": metadata, "stderr_message": string(line)}}, time.Now(), false)
	if outcome.Failure == nil || outcome.Failure.MessageSource != "" || outcome.Failure.Stderr == nil || *outcome.Failure.Stderr != *metadata {
		t.Fatalf("outcome=%#v", outcome)
	}
	for _, failure := range []*model.OutcomeFailure{outcome.Failure, withAllowlistedFailure(outcome).Failure} {
		view := projectNativeFailure(failure)
		if view == nil || view.Message != failureMessageGeneric || view.Stderr == nil || *view.Stderr != *metadata {
			t.Fatalf("view=%#v", view)
		}
	}
	legacy := withAllowlistedFailure(model.Outcome{Failure: &model.OutcomeFailure{Code: "native_execution_failed", Source: "codex", MessageSource: "stderr", Message: string(line)}})
	if legacy.Failure.Message != failureMessageGeneric || legacy.Failure.MessageSource != "" {
		t.Fatal("legacy marker authorized arbitrary text")
	}
}
func TestUnavailableOutcomeRecordsReason(t *testing.T) {
	for _, authority := range []model.Authority{model.AuthorityNative, model.AuthorityMultica} {
		execution := model.Execution{Adapter: "cursor", Authority: authority, State: model.StateCompleted}
		outcome := buildOutcome(execution, adapter.Result{Success: true}, time.Now(), false)
		want := "native_answer_missing"
		if authority == model.AuthorityMultica {
			want = "multica_result_route_unavailable"
		}
		if outcome.Availability != model.OutcomeUnavailableAtSource || outcome.UnavailableReason != want {
			t.Fatalf("outcome=%#v", outcome)
		}
	}
}

func TestStderrCauseAtExitPersistsWithoutSecrets(t *testing.T) {
	for _, tc := range []struct {
		name, line, want string
		retryable        bool
	}{
		{"unknown", "fatal: unable to process confidential user prompt opaque-token-Xb7sQ9", failureMessageGeneric, false},
		{"token-like", "fatal: opaque-token-Xb7sQ9", failureMessageGeneric, false},
		{"unlabeled prompt", "Please summarize the private meeting and send its transcript", failureMessageGeneric, false},
		{"github token", "fatal ghp_PRIVATEfixtureToken12345", failureMessageGeneric, false},
		{"aws token", "fatal AKIAFIXTUREPRIVATE1234", failureMessageGeneric, false},
		{"slack token", "fatal xoxb-12345-PRIVATEfixture", failureMessageGeneric, false},
		{"classified-with-prompt", "model not found: confidential user prompt opaque-token-Xb7sQ9", "model not found", false},
		{"JSON stderr", `{"type":"result","is_error":true,"result":"confidential user prompt opaque-token-Xb7sQ9"}`, failureMessageGeneric, false},
		{"JSON native code", `{"type":"turn.failed","error":{"message":"confidential user prompt","codex_error_info":"opaque_token_secret"}}`, failureMessageGeneric, false},
		{"capacity", "Selected model is at capacity. Please try a different model.", failureMessageCapacity, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root := testutil.TempDir(t)
			bin := filepath.Join(root, "codex")
			script := "#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo test-version; exit 0; fi\nprintf '\\n%s\\n' '" + tc.line + "' >&2\nprintf '%s\\n' 'Usage: native-command' >&2\nexit 1\n"
			if err := os.WriteFile(bin, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			journalPath := filepath.Join(root, "state", "journal.db")
			var stdout, stderr bytes.Buffer
			a := testApp(&stdout, &stderr)
			if code := a.run(context.Background(), []string{"--journal", journalPath, "run", "--adapter", "codex", "--", bin, "exec", "--json", "task"}); code != 0 {
				t.Fatalf("run exit=%d %s %s", code, stdout.String(), stderr.String())
			}
			id := executionIDFromOutput(t, stdout.String())
			for _, command := range []string{"status", "result"} {
				stdout.Reset()
				if code := a.run(context.Background(), []string{"--journal", journalPath, command, id}); code != 0 {
					t.Fatalf("%s exit=%d %s", command, code, stdout.String())
				}
				if !strings.Contains(stdout.String(), tc.want) || !strings.Contains(stdout.String(), `"retryable":`+jsonBool(tc.retryable)) {
					t.Fatalf("%s dropped reason: %s", command, stdout.String())
				}
				metadata := diagnostic.FromStderrLine([]byte(tc.line))
				if !strings.Contains(stdout.String(), metadata.Hash) || !strings.Contains(stdout.String(), `"bytes":`+strconv.Itoa(metadata.Bytes)) {
					t.Fatalf("%s lost correlation: %s", command, stdout.String())
				}
				for _, secret := range []string{"confidential user prompt", "opaque-token-Xb7sQ9", "opaque_token_secret", "Please summarize the private meeting", "ghp_PRIVATEfixtureToken12345", "AKIAFIXTUREPRIVATE1234", "xoxb-12345-PRIVATEfixture", "Usage:"} {
					if strings.Contains(stdout.String(), secret) {
						t.Fatalf("%s leaked %s", command, secret)
					}
				}
			}
			raw, err := os.ReadFile(journalPath)
			for _, secret := range []string{"Please summarize the private meeting", "ghp_PRIVATEfixtureToken12345", "AKIAFIXTUREPRIVATE1234", "xoxb-12345-PRIVATEfixture"} {
				if bytes.Contains(raw, []byte(secret)) {
					t.Fatal("journal retained unlabelled prompt/token")
				}
			}
			if err != nil || bytes.Contains(raw, []byte("confidential user prompt")) || bytes.Contains(raw, []byte("opaque-token-Xb7sQ9")) || bytes.Contains(raw, []byte("opaque_token_secret")) {
				t.Fatalf("journal retained secret: %v", err)
			}
		})
	}
}

func TestCapacityRequiresHarnessSignalAndAuthenticationWins(t *testing.T) {
	for _, tt := range []struct {
		name, message, native, harness, category, want string
		retry                                          bool
	}{
		{"auth429", "authentication required", "429", "codex", "", "authentication_required", true},
		{"provider429", "provider request failed", "429", "codex", "", "native_execution_failed", false},
		{"zcodeProvider429", "provider request failed", "429", "zcode", "", "native_execution_failed", false},
		{"auth7314", "authentication required", "7314", "zcode", "", "authentication_required", true},
		{"negatedAuth", "not at capacity; authentication required", "", "codex", "", "authentication_required", true},
		{"incidental", "earlier request was at capacity", "", "codex", "", "native_execution_failed", false},
		{"negated", "not at capacity", "", "codex", "", "native_execution_failed", false},
		{"sentenceSuffix", diagnostic.CodexCapacityMessage + " private task", "", "codex", "", "native_execution_failed", false},
		{"sentenceOtherHarness", diagnostic.CodexCapacityMessage, "", "cursor", "", "native_execution_failed", false},
		{"codexCode", "the turn stopped", "server_overloaded", "codex", "", "provider_capacity", true},
		{"wrongCodeHarness", "the turn stopped", "server_overloaded", "cursor", "", "native_execution_failed", false},
		{"codexSentence", diagnostic.CodexCapacityMessage, "", "codex", "", "provider_capacity", true},
		{"zcode1308", "provider request failed", "1308", "zcode", "", "provider_capacity", true},
		{"wrong1308Harness", "provider request failed", "1308", "codex", "", "native_execution_failed", false},
		{"zcodeHTTP429", "ZCode provider usage limit reached", "7314", "zcode", "capacity", "provider_capacity", true},
		{"authOverload", "authentication required", "server_overloaded", "codex", "", "authentication_required", true},
		{"authZCode1308", "authentication required", "1308", "zcode", "capacity", "authentication_required", true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			code, _, category, retry := classifyOutcomeFailure(tt.message, "", tt.native, tt.harness, tt.category)
			if code != tt.want || retry != tt.retry || (code != "provider_capacity" && category == "capacity") {
				t.Fatalf("got %s %s %v", code, category, retry)
			}
		})
	}
}
