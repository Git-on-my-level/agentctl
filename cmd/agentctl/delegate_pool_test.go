package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

// poolFixture has two accounts on one model, each with its own launcher, so a
// test can prove which executable actually ran.
type poolFixture struct {
	delegateFixture
	facts string
}

func newPoolFixture(t *testing.T, mode string, candidateExtras ...map[string]any) poolFixture {
	t.Helper()
	f := poolFixture{delegateFixture: newDelegateFixture(t, delegateSuccessScript)}
	f.facts = filepath.Join(f.root, "capacity.json")
	base, err := os.ReadFile(filepath.Join(f.root, "cursor-agent"))
	if err != nil {
		t.Fatal(err)
	}
	var candidates []any
	for i, account := range []string{"account-a", "account-b"} {
		path := filepath.Join(f.root, account)
		marker := "echo " + account + " >> '" + filepath.Join(f.root, "ran") + "'\n"
		script := strings.Replace(string(base), "#!/bin/sh\n", "#!/bin/sh\nif [ \"$1\" != '--version' ]; then "+strings.TrimSpace(marker)+"; fi\n", 1)
		if err := os.WriteFile(path, []byte(script), 0700); err != nil {
			t.Fatal(err)
		}
		candidate := map[string]any{"harness": "cursor", "model": "cursor-grok-4.6-high", "account": account, "executable": path}
		if i < len(candidateExtras) {
			for k, v := range candidateExtras[i] {
				candidate[k] = v
			}
		}
		candidates = append(candidates, candidate)
	}
	body, err := os.ReadFile(f.config)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(body, &cfg); err != nil {
		t.Fatal(err)
	}
	cfg["profiles"].(map[string]any)["test"].(map[string]any)["capacity"] = map[string]any{"file": f.facts,
		"pools": map[string]any{"work": map[string]any{"mode": mode, "candidates": candidates}}}
	body, err = json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(f.config, body, 0600); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"pool":"work"}`)
	return f
}

func (f poolFixture) writeFacts(t *testing.T, used map[string]float64) {
	t.Helper()
	var accounts []string
	for id, ratio := range used {
		accounts = append(accounts, fmt.Sprintf(`{"id":%q,"windows":[{"name":"weekly","used_ratio":%v}]}`, id, ratio))
	}
	body := fmt.Sprintf(`{"schema_version":1,"generated_at":%q,"accounts":[%s]}`, time.Now().UTC().Format(time.RFC3339), strings.Join(accounts, ","))
	if err := os.WriteFile(f.facts, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
}

func (f poolFixture) plan(t *testing.T) (map[string]any, []any) {
	t.Helper()
	code, out := f.invoke("work", "--plan")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	var document struct {
		Result   map[string]any `json:"result"`
		Warnings []any          `json:"warnings"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	capacity, ok := document.Result["capacity"].(map[string]any)
	if !ok {
		t.Fatalf("no capacity in %s", out)
	}
	return capacity, document.Warnings
}

func warningCodes(warnings []any) string {
	var codes []string
	for _, warning := range warnings {
		codes = append(codes, warning.(map[string]any)["code"].(string))
	}
	return strings.Join(codes, ",")
}

func TestPoolBalanceRunsTheChosenAccountsExecutable(t *testing.T) {
	f := newPoolFixture(t, "balance")
	f.writeFacts(t, map[string]float64{"account-a": 0.9, "account-b": 0.1})
	code, out := f.invoke("work", "--wait", "--content")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	ran, err := os.ReadFile(filepath.Join(f.root, "ran"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(ran)) != "account-b" {
		t.Fatalf("ran %q", ran)
	}
}

func TestPoolPriorityStaysOnExemptAccountPastItsLimit(t *testing.T) {
	f := newPoolFixture(t, "priority", map[string]any{"cutoff_exempt": true})
	f.writeFacts(t, map[string]float64{"account-a": 1.3, "account-b": 0})
	capacity, warnings := f.plan(t)
	if capacity["account"] != "account-a" || capacity["reason"] != "first_eligible" || len(warnings) != 0 {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
}

func TestPoolDefaultThresholdSwitchesAt95Percent(t *testing.T) {
	f := newPoolFixture(t, "priority")
	f.writeFacts(t, map[string]float64{"account-a": 0.95, "account-b": 0.2})
	if capacity, _ := f.plan(t); capacity["account"] != "account-b" {
		t.Fatalf("capacity=%v", capacity)
	}
	f.writeFacts(t, map[string]float64{"account-a": 0.94, "account-b": 0.2})
	if capacity, _ := f.plan(t); capacity["account"] != "account-a" {
		t.Fatalf("capacity=%v", capacity)
	}
}

func TestPoolCandidateThresholdOverridesDefault(t *testing.T) {
	f := newPoolFixture(t, "priority", map[string]any{"switch_threshold": 0.5})
	f.writeFacts(t, map[string]float64{"account-a": 0.6, "account-b": 0.7})
	if capacity, _ := f.plan(t); capacity["account"] != "account-b" {
		t.Fatalf("capacity=%v", capacity)
	}
}

func TestPoolNeverBlocksWhenEveryAccountIsOver(t *testing.T) {
	f := newPoolFixture(t, "priority")
	f.writeFacts(t, map[string]float64{"account-a": 0.99, "account-b": 0.97})
	capacity, warnings := f.plan(t)
	if capacity["account"] != "account-b" || capacity["reason"] != "all_over_threshold" || !strings.Contains(warningCodes(warnings), "capacity_all_over_threshold") {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
}

func TestPoolUnusableFactsKeepOrderAndWarn(t *testing.T) {
	f := newPoolFixture(t, "priority")
	capacity, warnings := f.plan(t)
	if capacity["account"] != "account-a" || capacity["facts_status"] != "missing" || !strings.Contains(warningCodes(warnings), "capacity_facts_unusable") {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
	if err := os.WriteFile(f.facts, []byte(`{"schema_version":1,"generated_at":"2020-01-01T00:00:00Z","accounts":[{"id":"account-a","windows":[{"name":"w","used_ratio":1}]}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	capacity, warnings = f.plan(t)
	if capacity["account"] != "account-a" || capacity["facts_status"] != "stale" || !strings.Contains(warningCodes(warnings), "capacity_facts_unusable") {
		t.Fatalf("capacity=%v warnings=%v", capacity, warnings)
	}
}

func TestPoolReplayKeepsOriginalChoiceAfterFactsChange(t *testing.T) {
	f := newPoolFixture(t, "balance")
	f.writeFacts(t, map[string]float64{"account-a": 0.1, "account-b": 0.9})
	if code, out := f.invoke("work", "--wait", "--content"); code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	f.writeFacts(t, map[string]float64{"account-a": 0.9, "account-b": 0.1})
	if capacity, _ := f.plan(t); capacity["account"] != "account-a" {
		t.Fatalf("capacity=%v", capacity)
	}
	ran, err := os.ReadFile(filepath.Join(f.root, "ran"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(string(ran)) != "account-a" {
		t.Fatalf("ran %q", ran)
	}
}

func TestPoolFanOutSpreadsWhileFactsLag(t *testing.T) {
	f := newPoolFixture(t, "balance")
	f.writeFacts(t, map[string]float64{"account-a": 0.1, "account-b": 0.1})
	for i := 0; i < 2; i++ {
		if err := os.WriteFile(f.request, []byte(fmt.Sprintf(`{"schema_version":1,"request_key":"fan-%d","selector":{"pool":"work"}}`, i)), 0600); err != nil {
			t.Fatal(err)
		}
		if code, out := f.invoke("work", "--wait", "--content"); code != 0 {
			t.Fatalf("exit=%d %s", code, out)
		}
	}
	ran, err := os.ReadFile(filepath.Join(f.root, "ran"))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Fields(string(ran)); len(got) != 2 || got[0] == got[1] {
		t.Fatalf("ran %q", got)
	}
}

func TestPoolRecordStaysReadableByStrictDecode(t *testing.T) {
	f := newPoolFixture(t, "priority")
	f.writeFacts(t, map[string]float64{"account-a": 0.1})
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	var document struct {
		Result struct {
			Requested map[string]any `json:"requested"`
			Capacity  struct {
				Pool string `json:"pool"`
			} `json:"capacity"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &document); err != nil {
		t.Fatal(err)
	}
	if _, stored := document.Result.Requested["pool"]; stored || document.Result.Requested["model"] != "cursor-grok-4.6-high" || document.Result.Capacity.Pool != "work" {
		t.Fatalf("result=%+v", document.Result)
	}
}

func TestPoolRefusals(t *testing.T) {
	for name, tc := range map[string]struct {
		selector, code string
		exit           output.Code
	}{
		"unknown pool": {`{"pool":"absent"}`, "delegate_unknown_pool", output.CodeUsage},
		"with family":  {`{"pool":"work","family":"grok"}`, "delegate_invalid_request", output.CodeUsage},
		"with model":   {`{"pool":"work","model":"cursor-grok-4.6-high"}`, "delegate_invalid_request", output.CodeUsage},
		"with effort":  {`{"pool":"work","settings":{"effort":"high"}}`, "delegate_invalid_request", output.CodeUsage},
	} {
		t.Run(name, func(t *testing.T) {
			f := newPoolFixture(t, "priority")
			f.selectRequest(t, tc.selector)
			code, out := f.invoke("work", "--plan")
			if code != output.ExitCodeFor(tc.exit) {
				t.Fatalf("exit=%d %s", code, out)
			}
			if issue := decodeDelegateDiagnostic(t, out); issue.Details["diagnostic_code"] != tc.code {
				t.Fatalf("issue=%#v", issue)
			}
		})
	}
}

func TestPoolCannotWidenTheAllowlist(t *testing.T) {
	unreviewed := map[string]any{"model": "unreviewed"}
	f := newPoolFixture(t, "priority", unreviewed, unreviewed)
	code, out := f.invoke("work", "--plan")
	if code != output.ExitCodeFor(output.CodeCapabilityUnavailable) {
		t.Fatalf("exit=%d %s", code, out)
	}
	if issue := decodeDelegateDiagnostic(t, out); issue.Details["diagnostic_code"] != "delegate_pool_no_candidate" {
		t.Fatalf("issue=%#v", issue)
	}
}

func TestNamingAModelIgnoresPools(t *testing.T) {
	f := newPoolFixture(t, "balance")
	f.writeFacts(t, map[string]float64{"account-a": 0.9, "account-b": 0.1})
	f.selectRequest(t, `{"family":"grok"}`)
	code, out := f.invoke("work", "--plan")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, `"capacity"`) || strings.Contains(out, "account-b") {
		t.Fatalf("named model took a pool: %s", out)
	}
}

func TestPoolCandidateEffortCountsAsDefaulted(t *testing.T) {
	entries := []delegation.Entry{{Harness: "zcode", Model: "m", Effort: "high"}, {Harness: "zcode", Model: "m", Effort: "low"}}
	resolution, err := resolvePoolCandidate(config.CapacityCandidate{Harness: "zcode", Model: "m", Effort: "high", Account: "a"}, entries)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.Resolved.Effort != "high" || !strings.Contains(strings.Join(resolution.Defaulted, ","), "effort") {
		t.Fatalf("resolution=%+v", resolution)
	}
}

func TestPoolDoesNotAddCapacityToNamedModelResults(t *testing.T) {
	f := newPoolFixture(t, "balance")
	f.selectRequest(t, `{"family":"grok"}`)
	code, out := f.invoke("work", "--wait")
	if code != 0 {
		t.Fatalf("exit=%d %s", code, out)
	}
	if strings.Contains(out, `"capacity"`) {
		t.Fatalf("named model result gained capacity: %s", out)
	}
}
