package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func (f poolFixture) loadExecution(t *testing.T, id string) model.Execution {
	t.Helper()
	parsed, err := ids.ParseExecutionID(id)
	if err != nil {
		t.Fatal(err)
	}
	journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	e, err := journal.GetExecution(context.Background(), parsed)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// updateProfile edits the fixture profile in place.
func (f poolFixture) updateProfile(t *testing.T, update func(map[string]any)) {
	t.Helper()
	body, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	update(cfg["profiles"].(map[string]any)["test"].(map[string]any))
	if body, err = json.Marshal(cfg); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func (f poolFixture) ran(t *testing.T) string {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(f.root, "ran"))
	if os.IsNotExist(err) {
		return ""
	}
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(body))
}

type delegateDocument struct {
	Result struct {
		ID        string                    `json:"id"`
		Requested map[string]any            `json:"requested"`
		Capacity  *model.DelegationCapacity `json:"capacity"`
		Defaulted []string                  `json:"defaulted"`
	} `json:"result"`
	Warnings []any `json:"warnings"`
}

func decodeDelegateDocument(t *testing.T, out string) delegateDocument {
	t.Helper()
	var document delegateDocument
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatalf("%v: %s", err, out)
	}
	return document
}

// The incident shape: the default account is at its weekly limit and the
// other has room. A request naming the pool's model must run the other one.
func TestModelSelectorInfersTheOnlyPool(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.15})
	f.selectRequest(t, `{"model":"cursor-grok-4.6-high"}`)
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if got := f.ran(t); got != "account-b" {
		t.Fatalf("ran %q: %s", got, out)
	}
	document := decodeDelegateDocument(t, out)
	capacity := document.Result.Capacity
	if capacity == nil || capacity.Pool != "work" || capacity.Account != "account-b" || capacity.InferredFrom != "model" || len(capacity.Considered) != 2 || capacity.Reason == "" {
		t.Fatalf("capacity=%+v", capacity)
	}
	if _, stored := document.Result.Requested["pool"]; stored || document.Result.Requested["model"] != "cursor-grok-4.6-high" {
		t.Fatalf("requested=%v", document.Result.Requested)
	}
	if strings.Contains(strings.Join(document.Result.Defaulted, ","), "model") {
		t.Fatalf("a named model must not count as defaulted: %v", document.Result.Defaulted)
	}
	assertResultMatchesSchemaShape(t, out, "delegate-result.schema.json", "capacity")
	// The record must load through the strict binding validator again.
	e := f.loadExecution(t, document.Result.ID)
	if e.Delegation.Capacity == nil || e.Delegation.Capacity.InferredFrom != "model" || strings.Contains(string(e.Delegation.Requested), `"pool"`) {
		t.Fatalf("journal=%+v requested=%s", e.Delegation.Capacity, e.Delegation.Requested)
	}
}

func TestFamilySelectorInfersTheOnlyPool(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.2})
	f.selectRequest(t, `{"family":"grok"}`)
	capacity, warnings := f.plan(t)
	if capacity["pool"] != "work" || capacity["account"] != "account-b" || capacity["inferred_from"] != "family" || len(warnings) != 0 {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
}

func TestModelInTwoPoolsKeepsDefaultAccountAndWarns(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.1})
	f.updateCapacity(t, func(c map[string]any) {
		pools := c["pools"].(map[string]any)
		// A second pool holding the model with a different candidate list.
		work := pools["work"].(map[string]any)["candidates"].([]any)
		pools["review"] = map[string]any{"candidates": []any{work[0]}}
	})
	f.selectRequest(t, `{"model":"cursor-grok-4.6-high"}`)
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	document := decodeDelegateDocument(t, out)
	if document.Result.Capacity != nil || f.ran(t) != "" {
		t.Fatalf("ambiguous model took a pool: %s", out)
	}
	if codes := warningCodes(document.Warnings); codes != "capacity_pool_inference_ambiguous" || !strings.Contains(out, `"pools":["review","work"]`) {
		t.Fatalf("warnings=%s out=%s", codes, out)
	}
}

// ui and council both run Opus on the same Claude accounts: pools with
// identical candidate lists choose alike, so the model still routes by capacity.
func TestModelInIdenticalPoolsStillInfers(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.1})
	f.updateCapacity(t, func(c map[string]any) {
		pools := c["pools"].(map[string]any)
		pools["review"] = pools["work"]
	})
	f.selectRequest(t, `{"model":"cursor-grok-4.6-high"}`)
	capacity, warnings := f.plan(t)
	if capacity["pool"] != "review" || capacity["account"] != "account-b" || capacity["inferred_from"] != "model" || len(warnings) != 0 {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
}

func TestModelInNoPoolIsUnchanged(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.1})
	f.updateProfile(t, func(profile map[string]any) {
		preferences := profile["agent_preferences"].(map[string]any)
		preferences["preferred"] = append(preferences["preferred"].([]any), map[string]any{"agent": "cursor", "family": "sonnet", "version": "5", "model": "cursor-sonnet-5", "speed": "regular"})
	})
	f.selectRequest(t, `{"model":"cursor-sonnet-5"}`)
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	document := decodeDelegateDocument(t, out)
	if document.Result.Capacity != nil || len(document.Warnings) != 0 || f.ran(t) != "" {
		t.Fatalf("unpooled model changed: %s", out)
	}
}

func TestPoolMixingModelsIsNotInferred(t *testing.T) {
	f := newPoolFixture(t, nil, map[string]any{"model": "cursor-sonnet-5"})
	f.updateProfile(t, func(profile map[string]any) {
		preferences := profile["agent_preferences"].(map[string]any)
		preferences["preferred"] = append(preferences["preferred"].([]any), map[string]any{"agent": "cursor", "family": "sonnet", "version": "5", "model": "cursor-sonnet-5", "speed": "regular"})
	})
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.1})
	f.selectRequest(t, `{"model":"cursor-grok-4.6-high"}`)
	code, out := f.invoke("work", "--plan")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	document := decodeDelegateDocument(t, out)
	if strings.Contains(out, `"capacity":{`) || warningCodes(document.Warnings) != "capacity_pool_inference_conflict" || !strings.Contains(out, `"reason":"pool_mixes_models"`) {
		t.Fatalf("out=%s", out)
	}
}

// Resuming across accounts is impossible, so a follow-up turn of an inferred
// pool launch must stay on the account that holds the session.
func TestContinueOfInferredPoolLaunchKeepsItsAccount(t *testing.T) {
	f := newPoolFixture(t)
	f.writeFacts(t, map[string]float64{"account-a": 1.0, "account-b": 0.15})
	f.selectRequest(t, `{"model":"cursor-grok-4.6-high"}`)
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	first := resultID(t, out)
	// Facts flip: a fresh choice would now be account-a.
	f.writeFacts(t, map[string]float64{"account-a": 0.0, "account-b": 1.0})
	code, out = f.command(t, "follow up", "continue", first, "--request-key", "turn-2", "--prompt-stdin", "--wait")
	if code != 0 {
		t.Fatalf("continue exit=%d %s", code, out)
	}
	if got := f.ran(t); got != "account-b\naccount-b" {
		t.Fatalf("ran %q", got)
	}
	document := decodeDelegateDocument(t, out)
	if c := document.Result.Capacity; c == nil || c.Account != "account-b" || c.InferredFrom != "model" {
		t.Fatalf("continue capacity=%+v", c)
	}
}

func inferenceProfile(candidates ...config.CapacityCandidate) config.Profile {
	return config.Profile{
		AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{
			{Agent: "codex", Family: "sol", Model: "gpt-6.2-sol", Effort: "high", Default: true},
			{Agent: "codex", Family: "sol", Model: "gpt-6.2-sol", Effort: "low"},
		}},
		Capacity: &config.Capacity{Pools: map[string]config.CapacityPool{"implement": {Candidates: candidates}}},
	}
}

func TestInferenceHonoursOrRefusesPinnedEffort(t *testing.T) {
	high := []config.CapacityCandidate{{Harness: "codex", Model: "gpt-6.2-sol", Account: "codex"}, {Harness: "codex", Model: "gpt-6.2-sol", Account: "codex-2", Executable: "/bin/codex-2"}}
	for name, tc := range map[string]struct {
		effort, pool, warning string
	}{
		"unpinned":        {"", "implement", ""},
		"pinned default":  {"high", "implement", ""},
		"pinned conflict": {"low", "", "capacity_pool_inference_conflict"},
	} {
		t.Run(name, func(t *testing.T) {
			profile := inferenceProfile(high...)
			selector := delegation.Selector{Model: "gpt-6.2-sol"}
			if tc.effort != "" {
				selector.Settings = &delegation.Settings{Effort: tc.effort}
			}
			resolution, err := delegation.Resolve(selector, delegateEntries(profile))
			if err != nil {
				t.Fatal(err)
			}
			got := inferDelegatePool(profile, selector, resolution)
			codes := ""
			for _, warning := range got.warnings {
				codes += warning.Code
			}
			if got.pool != tc.pool || codes != tc.warning || (tc.pool != "" && got.from != "model") {
				t.Fatalf("inference=%+v", got)
			}
			if tc.warning != "" && got.warnings[0].Details["field"] != "settings.effort" {
				t.Fatalf("details=%v", got.warnings[0].Details)
			}
		})
	}
}

func TestInferenceSkipsPoolWithoutResolvableCandidate(t *testing.T) {
	// The model is in the pool, but its candidate's effort is not reviewed.
	profile := inferenceProfile(config.CapacityCandidate{Harness: "codex", Model: "gpt-6.2-sol", Effort: "xhigh", Account: "codex"})
	selector := delegation.Selector{Family: "sol"}
	resolution, err := delegation.Resolve(selector, delegateEntries(profile))
	if err != nil {
		t.Fatal(err)
	}
	got := inferDelegatePool(profile, selector, resolution)
	if got.pool != "" || len(got.warnings) != 1 || got.warnings[0].Details["reason"] != "no_resolvable_candidate" {
		t.Fatalf("inference=%+v", got)
	}
}

// A remote launch is resolved by the remote host, which infers its own pool.
// The caller forwards the request unchanged and keeps the remote's choice.
func TestRemoteModelSelectorKeepsTheRemotelyInferredPool(t *testing.T) {
	f := newRemoteFixture(t)
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(f.root, "receipt")), &doc); err != nil {
		t.Fatal(err)
	}
	receipt := doc["result"].(map[string]any)
	receipt["capacity"] = map[string]any{"pool": "implement", "mode": "drain_before_reset", "account": "codex-2", "reason": "drain_before_reset", "facts_status": "ok", "inferred_from": "model",
		"considered": []any{map[string]any{"account": "codex", "model": "remote-only-model", "used_ratio": 1.0, "threshold": 0.97, "eligible": false, "recent_launches": 0}, map[string]any{"account": "codex-2", "model": "remote-only-model", "used_ratio": 0.2, "threshold": 0.97, "eligible": true, "recent_launches": 0}}}
	f.writeJSON(t, "receipt", receipt)
	selector := `{"model":"remote-only-model","host":"remote","settings":{"access":"read_only"}}`
	f.selectRequest(t, selector)
	code, out := f.invoke("remote inferred pool")
	if code != 0 {
		t.Fatal(out)
	}
	if staged := string(mustRead(t, f.stagedRequest(t))); !strings.Contains(staged, `"selector":`+selector) {
		t.Fatalf("request was not forwarded unchanged: %s", staged)
	}
	e := f.execution(t, remoteLocalID(t, out))
	if strings.Contains(string(e.Delegation.Requested), `"pool"`) || e.Delegation.Capacity == nil || e.Delegation.Capacity.Account != "codex-2" || e.Delegation.Capacity.InferredFrom != "model" {
		t.Fatalf("requested=%s capacity=%+v", e.Delegation.Requested, e.Delegation.Capacity)
	}
}

// Resolve trims catalog model IDs, so a padded catalog or pool model must
// still match its pool rather than silently take the default account.
func TestPoolCandidateRunsIgnoresModelPadding(t *testing.T) {
	candidate := config.CapacityCandidate{Harness: "cursor", Model: " cursor-grok-4.6-high "}
	if !poolCandidateRuns(candidate, delegation.Entry{Harness: "cursor", Model: "cursor-grok-4.6-high  "}) {
		t.Fatal("padded model IDs did not match")
	}
	if poolCandidateRuns(candidate, delegation.Entry{Harness: "cursor", Model: "cursor-grok-4.7-high"}) {
		t.Fatal("a different model matched")
	}
}
