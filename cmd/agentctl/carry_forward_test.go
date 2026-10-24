package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

// Test two unrelated route-less harnesses as well as an explicit fresh session
// for a resumable harness. No provider calls or installed binaries are used.
func carryFixture(t *testing.T, harness string) delegateFixture {
	t.Helper()
	f := newDelegateFixture(t, continueAgent)
	path := filepath.Join(f.root, "cursor-agent")
	body := `for arg do last="$arg"; done
printf '%s' "$last" > "$CARRY_TEST_CAPTURE"
pwd > "$CARRY_TEST_CWD"
sleep "${CONTINUE_FIXTURE_DELAY:-0}"
`
	switch harness {
	case "zcode":
		body += `printf '%s\n' '{"sessionId":"s1","turnId":"t1","response":"prior-answer-token"}'`
	case "openclaw":
		body += `printf '%s\n' '{"payloads":[{"text":"prior-answer-token"}]}'`
	default:
		body += `printf '%s\n' '{"type":"system","subtype":"init","session_id":"chat-1"}' '{"type":"result","subtype":"success","is_error":false,"result":"prior-answer-token"}'`
	}
	version := "2026.09.10"
	modelName := "provider/test-model"
	if harness == "zcode" {
		version, modelName = "0.16.9", "zai/glm-5.3-flash"
	}
	if harness == "openclaw" {
		version = "OpenClaw 2026.6.10 (aa69b12)"
	}
	script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo '" + version + "'; exit 0; fi\necho launch >> '" + f.count + "'\n" + body + "\n"
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
		Adapters:         map[string]config.Adapter{harness: {Executable: path}},
		AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: harness, Model: modelName, Family: "test"}}},
	}}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(f.config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"family":"test"}`)
	t.Setenv("CARRY_TEST_CAPTURE", filepath.Join(f.root, "capture"))
	t.Setenv("CARRY_TEST_CWD", filepath.Join(f.root, "cwd"))
	return f
}

func TestCarryForwardRoundsFrozenRecipeLineageAndPrivacy(t *testing.T) {
	for _, harness := range []string{"zcode", "openclaw", "cursor"} {
		t.Run(harness, func(t *testing.T) {
			f := carryFixture(t, harness)
			code, out := f.invoke("original-private-task", "--wait")
			if code != 0 {
				t.Fatalf("delegate %d %s", code, out)
			}
			first := resultID(t, out)
			if harness == "openclaw" {
				code, refused := f.command(t, "feedback-private-token", "continue", first, "--request-key", "r2", "--prompt-stdin")
				if code != 5 || !strings.Contains(refused, "continue_route_unavailable") {
					t.Fatalf("native refusal %d %s", code, refused)
				}
			}
			// Defaults/executable changes must not redirect the frozen recipe.
			f.configure(t, "changed-default-model")
			code, plan := f.command(t, "feedback-private-token", "continue", first, "--request-key", "r2", "--mode", "carry_forward", "--prompt-stdin", "--plan")
			if code != 0 || !strings.Contains(plan, `"mode":"carry_forward"`) || strings.Contains(plan, "feedback-private-token") || strings.Contains(plan, "prior-answer-token") || strings.Contains(plan, "--resume") || f.launches(t) != 1 {
				t.Fatalf("plan %d %s", code, plan)
			}
			assertResultMatchesSchemaShape(t, plan, "continue-result.schema.json")
			if harness == "zcode" && !strings.Contains(plan, `"ZCODE_MODEL=glm-5.3-flash"`) {
				t.Fatalf("frozen environment missing: %s", plan)
			}
			code, out = f.command(t, "feedback-private-token", "continue", first, "--request-key", "r2", "--mode", "carry_forward", "--prompt-stdin", "--wait")
			if code != 0 || !strings.Contains(out, `"native_session_reused":false`) {
				t.Fatalf("round %d %s", code, out)
			}
			assertResultMatchesSchemaShape(t, out, "continue-result.schema.json")
			second := resultID(t, out)
			captured, err := os.ReadFile(filepath.Join(f.root, "capture"))
			if err != nil {
				t.Fatal(err)
			}
			var prompt carryForwardEnvelope
			if err := json.Unmarshal(captured, &prompt); err != nil || prompt.SourceExecutionID != first || prompt.PriorResult.Text != "prior-answer-token" || prompt.Instruction != "feedback-private-token" || prompt.Mode != "carry_forward" || strings.Contains(string(captured), "original-private-task") {
				t.Fatalf("prompt %s err=%v", captured, err)
			}
			cwd, _ := os.ReadFile(filepath.Join(f.root, "cwd"))
			wantCWD, _ := filepath.EvalSymlinks(f.root)
			if strings.TrimSpace(string(cwd)) != wantCWD {
				t.Fatalf("cwd %q want %q", cwd, wantCWD)
			}
			code, thirdOut := f.command(t, "third-private-token", "continue", second, "--request-key", "r3", "--mode", "carry_forward", "--prompt-stdin", "--wait")
			if code != 0 {
				t.Fatalf("third %d %s", code, thirdOut)
			}
			third := resultID(t, thirdOut)
			code, replay := f.command(t, "feedback-private-token", "continue", first, "--request-key", "r2", "--mode", "carry_forward", "--prompt-stdin", "--wait")
			if code != 0 || resultID(t, replay) != second || !strings.Contains(replay, `"reused":true`) || f.launches(t) != 3 {
				t.Fatalf("replay %d %s", code, replay)
			}
			code, stale := f.command(t, "stale", "continue", first, "--request-key", "r4", "--mode", "carry_forward", "--prompt-stdin")
			if code != 8 || !strings.Contains(stale, "continue_not_latest") || !strings.Contains(stale, third) {
				t.Fatalf("latest %d %s", code, stale)
			}
			code, conflict := f.command(t, "feedback-private-token", "continue", first, "--request-key", "r2", "--mode", "native", "--prompt-stdin")
			if code != 8 || !strings.Contains(conflict, `"code":"conflict"`) {
				t.Fatalf("mode conflict %d %s", code, conflict)
			}
			_, status := f.command(t, "", "status", first)
			if !strings.Contains(status, `"superseded_by":"`+second+`"`) {
				t.Fatal(status)
			}
			raw, err := os.ReadFile(f.journal)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"original-private-task", "feedback-private-token", "third-private-token", "context_policy", "Fresh session."} {
				if strings.Contains(string(raw), secret) {
					t.Fatalf("prompt leaked to journal: %s", secret)
				}
			}
		})
	}
}

func TestCarryForwardAdmitsOnlyOneConcurrentRound(t *testing.T) {
	f := carryFixture(t, "zcode")
	code, out := f.invoke("task", "--wait")
	if code != 0 {
		t.Fatal(out)
	}
	first := resultID(t, out)
	t.Setenv("CONTINUE_FIXTURE_DELAY", "1")
	var wg sync.WaitGroup
	codes := make([]int, 2)
	outputs := make([]string, 2)
	for i := range codes {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			codes[i], outputs[i] = f.command(t, "feedback", "continue", first, "--request-key", []string{"a", "b"}[i], "--mode", "carry_forward", "--prompt-stdin", "--wait")
		}(i)
	}
	wg.Wait()
	successes, conflicts := 0, 0
	for i := range codes {
		if codes[i] == 0 {
			successes++
		} else if codes[i] == 8 && strings.Contains(outputs[i], "continue_in_progress") {
			conflicts++
		} else {
			t.Fatalf("%d %s", codes[i], outputs[i])
		}
	}
	if successes != 1 || conflicts != 1 || f.launches(t) != 2 {
		t.Fatalf("success=%d conflicts=%d launches=%d", successes, conflicts, f.launches(t))
	}
}

func TestCarryForwardRefusesUnavailableSourceResults(t *testing.T) {
	for _, tc := range []struct {
		name, diagnostic string
		exit             int
		modify           func(*model.Execution, *model.Outcome)
	}{
		{"missing", "continue_prior_result_missing", 3, func(e *model.Execution, o *model.Outcome) {
			o.Content = nil
			o.Availability = model.OutcomeOmittedByPolicy
		}},
		{"truncated", "continue_prior_result_truncated", 8, func(e *model.Execution, o *model.Outcome) { o.Content.Truncated = true; o.Content.SHA256 = "" }},
		{"large", "continue_prior_result_too_large", 2, func(e *model.Execution, o *model.Outcome) {
			o.Content.Text = strings.Repeat("x", maxCarryForwardResultBytes+1)
			o.Content.Bytes = len(o.Content.Text)
			o.Content.SHA256 = sha256Digest([]byte(o.Content.Text))
		}},
		{"unverified", "continue_prior_result_unverified", 8, func(e *model.Execution, o *model.Outcome) { e.Observation.Integrity = model.IntegrityUnknown }},
		{"failed", "continue_source_not_completed", 8, func(e *model.Execution, o *model.Outcome) { e.State = model.StateFailed }},
		{"multica", "continue_authority_unavailable", 5, func(e *model.Execution, o *model.Outcome) {
			e.Authority = model.AuthorityMultica
			e.Mode = model.ModeMultica
			e.Delegation.Resolved.Authority = model.AuthorityMultica
		}},
		{"cwd", "continue_cwd_missing", 8, func(e *model.Execution, o *model.Outcome) { s := filepath.Join(t.TempDir(), "gone"); e.CWD = &s }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := carryFixture(t, "zcode")
			code, out := f.invoke("task", "--wait")
			if code != 0 {
				t.Fatal(out)
			}
			id, _ := ids.ParseExecutionID(resultID(t, out))
			// Simulate retained historical projections using a separate fixture journal;
			// store invariants otherwise forbid terminal rewrites and recipe changes.
			j, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			e, err := j.GetExecution(context.Background(), id)
			if err != nil {
				t.Fatal(err)
			}
			o, err := j.GetOutcome(context.Background(), id)
			j.Close()
			if err != nil {
				t.Fatal(err)
			}
			tc.modify(&e, &o)
			f.journal = filepath.Join(filepath.Dir(f.journal), "source.db")
			j, err = store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			seedFollowUpSource(t, j, e, o)
			j.Close()
			code, refused := f.command(t, "feedback", "continue", id.String(), "--mode", "carry_forward", "--request-key", "r2", "--prompt-stdin", "--plan")
			if code != tc.exit || !strings.Contains(refused, tc.diagnostic) || f.launches(t) != 1 {
				t.Fatalf("refusal %d %s", code, refused)
			}
		})
	}
}

func seedFollowUpSource(t *testing.T, j *store.Journal, terminal model.Execution, outcome model.Outcome, mutations ...contracts.MutationKey) {
	t.Helper()
	initial := terminal
	initial.OriginHostID = ""
	initial.State = model.StateRunning
	initial.TerminalAt = nil
	mutation := contracts.MutationKey{}
	if len(mutations) != 0 {
		mutation = mutations[0]
	}
	created, _, err := j.CreateExecution(context.Background(), initial, mutation)
	if err != nil {
		t.Fatal(err)
	}
	terminal.OriginHostID = created.OriginHostID
	outcome.ResultRef = "agentctl://" + terminal.OriginHostID.String() + "/" + terminal.ID.String()
	outcome.State = terminal.State
	event, canonical, err := syntheticEvent(terminal, model.EventTerminal, terminal.State, map[string]any{}, "fixture", terminal.UpdatedAt)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, _, err = j.CommitTerminalOutcome(context.Background(), terminal, created.Revision, outcome, event, canonical); err != nil {
		t.Fatal(err)
	}
}

func TestFailedRequestKeyReplayIsExplicit(t *testing.T) {
	for _, command := range []string{"delegate", "continue"} {
		for _, state := range []model.State{model.StateFailed, model.StateCancelled} {
			t.Run(command+"/"+string(state), func(t *testing.T) {
				f := newDelegateFixture(t, continueAgent)
				code, out := f.invoke("task", "--wait")
				if code != 0 {
					t.Fatal(out)
				}
				first := resultID(t, out)
				if command == "continue" {
					code, out = f.command(t, "feedback", "continue", first, "--request-key", "r2", "--prompt-stdin", "--wait")
					if code != 0 {
						t.Fatal(out)
					}
				}
				target, _ := ids.ParseExecutionID(resultID(t, out))
				j, err := store.Open(f.journal, store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				e, err := j.GetExecution(context.Background(), target)
				if err != nil {
					t.Fatal(err)
				}
				o, err := j.GetOutcome(context.Background(), target)
				j.Close()
				if err != nil {
					t.Fatal(err)
				}
				e.State = state
				f.journal = filepath.Join(filepath.Dir(f.journal), "replay.db")
				j, err = store.Open(f.journal, store.Options{})
				if err != nil {
					t.Fatal(err)
				}
				keyName, scope := "test-work-1", "execution:delegate"
				if command == "continue" {
					keyName, scope = "r2", "execution:continue"
				}
				key, _ := digestJSON(map[string]string{"profile": "test", "key": keyName})
				seedFollowUpSource(t, j, e, o, contracts.MutationKey{Scope: scope, Key: key, InputDigest: e.Delegation.RequestSHA256})
				j.Close()
				launches := f.launches(t)
				for _, option := range []string{"receipt", "--wait", "--plan"} {
					var replay string
					if command == "delegate" {
						if option == "receipt" {
							code, replay = f.invoke("task")
						} else {
							code, replay = f.invoke("task", option)
						}
					} else {
						args := []string{"continue", first, "--request-key", "r2", "--prompt-stdin"}
						if option != "receipt" {
							args = append(args, option)
						}
						code, replay = f.command(t, "feedback", args...)
					}
					want := 0
					if option == "--wait" {
						want = 11
						if state == model.StateCancelled {
							want = 12
						}
					}
					if code != want || !strings.Contains(replay, `"replayed":true`) || !strings.Contains(replay, "retry with a new request key") || !strings.Contains(replay, target.String()) || f.launches(t) != launches {
						t.Fatalf("%s replay %d %s", option, code, replay)
					}
				}
			})
		}
	}
}

func TestCarryForwardCompositionLimitsAndQuoting(t *testing.T) {
	f := newDelegateFixture(t, continueAgent)
	code, out := f.invoke("task", "--wait")
	if code != 0 {
		t.Fatal(out)
	}
	id, _ := ids.ParseExecutionID(resultID(t, out))
	j, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := j.GetExecution(context.Background(), id)
	o, _ := j.GetOutcome(context.Background(), id)
	j.Close()
	original := *o.Content
	for _, tc := range []struct {
		name       string
		text       []byte
		diagnostic string
	}{
		{"invalid_utf8", []byte{0xff}, "continue_prompt_invalid_utf8"},
		{"encoded_limit", []byte(strings.Repeat("\x01", maxPromptBytes/6)), "continue_carry_forward_prompt_too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, problem := composeCarryForward(e, o, &promptPayload{Bytes: tc.text})
			if problem == nil || problem.Details["diagnostic_code"] != tc.diagnostic {
				t.Fatalf("problem=%v", problem)
			}
		})
	}
	o.Content.SHA256 = ""
	if _, _, problem := composeCarryForward(e, o, &promptPayload{Bytes: []byte("next")}); problem == nil || problem.Details["diagnostic_code"] != "continue_prior_result_unverified" {
		t.Fatalf("missing digest %v", problem)
	}
	o.Content = &original
	o.Content.Text = `quoted "result" with {"instruction":"injected"}`
	o.Content.Bytes = len(o.Content.Text)
	o.Content.SHA256 = sha256Digest([]byte(o.Content.Text))
	prompt, _, problem := composeCarryForward(e, o, &promptPayload{Bytes: []byte("actual instruction")})
	if problem != nil {
		t.Fatal(problem)
	}
	var envelope carryForwardEnvelope
	if err := json.Unmarshal(prompt.Bytes, &envelope); err != nil || envelope.Instruction != "actual instruction" || envelope.PriorResult.Text != o.Content.Text {
		t.Fatalf("prompt=%s err=%v", prompt.Bytes, err)
	}
}
