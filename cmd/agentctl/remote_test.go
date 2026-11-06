package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const remoteTestID = "exec-abandon-abandon-abandon-abandon-abandon-aisle"
const remoteNextID = "exec-zoo-zoo-zoo-zoo-zoo-wish"
const remoteTestHost = "host-yard-total-plastic-uncover-drip-angry"

type remoteFixture struct{ delegateFixture }

func newRemoteFixture(t *testing.T) remoteFixture {
	t.Helper()
	f := remoteFixture{newDelegateFixture(t, delegateSuccessScript)}
	f.selectRequest(t, `{"family":"grok","host":"remote"}`)
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	p := cfg.Profiles["test"]
	p.Route.SSHHosts = map[string]string{"other-workstation": "operator@ssh-alias"}
	cfg.Profiles["test"] = p
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(f.config, body, 0600); err != nil {
		t.Fatal(err)
	}
	f.writeJSON(t, "receipt", map[string]any{"id": remoteTestID, "origin_host_id": remoteTestHost, "state": "running", "resolved": model.DelegationTarget{Harness: "cursor", Family: "grok", Model: "remote-only-model", Host: "other-workstation", Authority: model.AuthorityNative}, "provenance": map[string]any{"configuration_sha256": "sha256:" + strings.Repeat("b", 64), "defaulted": []string{"model"}}})
	f.writeJSON(t, "next", map[string]any{"id": remoteNextID, "origin_host_id": remoteTestHost, "state": "running", "resolved": model.DelegationTarget{Harness: "cursor", Family: "grok", Model: "remote-only-model", Host: "other-workstation", Authority: model.AuthorityNative}, "provenance": map[string]any{"configuration_sha256": "sha256:" + strings.Repeat("b", 64), "defaulted": []string{"model"}}, "continuation": map[string]any{"mode": "native", "native_session_reused": true}})
	f.writeJSON(t, "terminal", map[string]any{"id": remoteTestID, "origin_host_id": remoteTestHost, "state": "completed", "liveness": "exited"})
	f.writeOutcome(t, model.StateCompleted)
	f.writeJSON(t, "plan", map[string]any{"plan": true, "resolved": map[string]any{"host": "other-workstation"}, "id": remoteTestID, "reused": true})
	script := `#!/bin/sh
set -eu
cd ` + shellQuote(f.root) + `
printf '%s\n' "$@" >> ssh-argv
while [ "$1" = -o ]; do shift 2; done
if [ -f fail ]; then cat diagnostic >&2; cat fail-output; exit "$(cat fail)"; fi
case "$3" in
 umask*)
  printf '%s' "$3" > stage-command
  dir=$(TMPDIR="$PWD" /bin/sh -c "$3")
  printf '%s' "$dir" > staged-dir
  cp "$dir/request.json" staged-request
  printf '%s\n' "$dir" ;;
 *"'delegate'"*)
  printf '%s' "$3" > launch-command
  cat > delegated-prompt
  echo attempt >> attempts
  if [ ! -f launched ]; then echo launch > launched; fi
  case "$3" in *"'--plan'"*) cat plan ;; *) cat receipt ;; esac ;;
 *"'continue'"*) cat > continued-prompt; cat next ;;
 *"'await'"*) cat terminal ;;
 *"'status'"*) cat terminal ;;
 *"'result'"*) cat result ;;
 *) exit 99 ;;
esac
`
	path := filepath.Join(f.root, "fake-ssh")
	if err := os.WriteFile(path, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_SSH", path)
	return f
}
func (f remoteFixture) writeJSON(t *testing.T, name string, value any) {
	t.Helper()
	body, err := json.Marshal(map[string]any{"ok": true, "schema_version": 1, "result": value})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(f.root, name), body, 0600); err != nil {
		t.Fatal(err)
	}
}
func (f remoteFixture) writeOutcome(t *testing.T, state model.State) {
	t.Helper()
	id, _ := ids.ParseExecutionID(remoteTestID)
	outcome := model.Outcome{SchemaVersion: 1, ExecutionID: id, Revision: 1, State: state, Availability: model.OutcomeStored, RecordedAt: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), Source: "cursor", ResultRef: "agentctl://" + remoteTestHost + "/" + remoteTestID}
	if state == model.StateCompleted {
		text := "remote verified answer\n"
		outcome.Content = &model.OutcomeContent{MediaType: "text/plain", Source: "native_result", Text: text, Preview: text, Bytes: len(text), SHA256: sha256Digest([]byte(text))}
	} else {
		outcome.Failure = &model.OutcomeFailure{Code: "native_error", Kind: "execution", Source: "cursor", Message: "native execution failed"}
	}
	f.writeJSON(t, "result", map[string]any{"outcome": outcome})
}
func (f remoteFixture) invoke(prompt string, extra ...string) (int, string) {
	args := []string{"delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin"}
	return f.command(prompt, append(args, extra...)...)
}
func (f remoteFixture) command(stdin string, args ...string) (int, string) {
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	a.stdin = strings.NewReader(stdin)
	argv := append([]string{"--config", f.config, "--journal", f.journal}, args...)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	code := a.run(ctx, argv)
	return code, stdout.String()
}
func remoteLocalID(t *testing.T, out string) string {
	t.Helper()
	var doc struct {
		Result struct {
			ID string `json:"id"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || doc.Result.ID == "" {
		t.Fatal(out)
	}
	return doc.Result.ID
}
func (f remoteFixture) execution(t *testing.T, id string) model.Execution {
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
func TestRemoteDelegateLifecycle(t *testing.T) {
	f := newRemoteFixture(t)
	prompt := "private prompt: ' $()\nexact bytes"
	code, out := f.invoke(prompt, "--label", "remote.test", "--timeout", "2m")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	local := remoteLocalID(t, out)
	if local == remoteTestID || !strings.Contains(out, `"execution_id":"`+remoteTestID+`"`) {
		t.Fatal(out)
	}
	e := f.execution(t, local)
	if !e.RemoteForwarded() || e.Delegation.NativePlan != nil || e.Workspace != nil || e.Delegation.Resolved.Model != "remote-only-model" || e.Delegation.Resolved.Host != "other-workstation" {
		t.Fatalf("%+v", e)
	}
	for _, item := range []struct{ name, want string }{{"delegated-prompt", prompt}, {"staged-request", string(mustRead(t, f.request))}} {
		if got := string(mustRead(t, filepath.Join(f.root, item.name))); got != item.want {
			t.Fatalf("%s=%q", item.name, got)
		}
	}
	argv := string(mustRead(t, filepath.Join(f.root, "ssh-argv")))
	for _, want := range []string{"operator@ssh-alias", "cd " + shellQuote(*e.CWD), "agentctl-delegate.", "'--prompt-stdin' '--detach'", "'--label' 'remote.test'", "'--timeout' '2m0s'"} {
		if !strings.Contains(argv, want) {
			t.Fatalf("missing %s: %s", want, argv)
		}
	}
	if strings.Contains(string(mustRead(t, f.journal)), prompt) {
		t.Fatal("prompt in journal")
	}
	code, out = f.invoke(prompt, "--label", "remote.test", "--timeout", "2m")
	if code != 0 || remoteLocalID(t, out) != local || !strings.Contains(out, `"reused":true`) {
		t.Fatalf("%d %s", code, out)
	}
	if strings.Count(string(mustRead(t, filepath.Join(f.root, "attempts"))), "attempt") != 2 || string(mustRead(t, filepath.Join(f.root, "launched"))) != "launch\n" {
		t.Fatal("replay did not re-use admission")
	}
	code, out = f.command("", "recent")
	if code != 0 || !strings.Contains(out, local) || !strings.Contains(out, "other-workstation") {
		t.Fatalf("recent: %d %s", code, out)
	}
	before := mustRead(t, f.journal)
	code, out = f.command("", "status", local)
	if code != 0 || remoteLocalID(t, out) != local || !strings.Contains(out, `"state":"completed"`) {
		t.Fatalf("status: %d %s", code, out)
	}
	if !bytes.Equal(before, mustRead(t, f.journal)) || f.execution(t, local).State.Terminal() {
		t.Fatal("status changed local state")
	}
	code, out = f.command("", "await", local, "--content", "--timeout", "20s")
	if code != 0 || out != "remote verified answer\n" {
		t.Fatalf("await: %d %s", code, out)
	}
	if f.execution(t, local).State != model.StateCompleted {
		t.Fatal("terminal not recorded")
	}
	journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = journal.GetAcknowledgement(context.Background(), e.ID)
	journal.Close()
	if err != nil {
		t.Fatal("not acknowledged", err)
	}
	code, out = f.command("follow-up bytes", "continue", local, "--request-key", "round-two", "--prompt-stdin")
	if code != 0 {
		t.Fatalf("continue: %d %s", code, out)
	}
	next := f.execution(t, remoteLocalID(t, out))
	if !next.RemoteForwarded() || next.Delegation.RemotePlan.ExecutionID.String() != remoteNextID || len(next.Supersedes) != 1 || next.Supersedes[0].String() != local {
		t.Fatalf("%+v", next)
	}
	if got := string(mustRead(t, filepath.Join(f.root, "continued-prompt"))); got != "follow-up bytes" {
		t.Fatal(got)
	}
	// Every step (staging, admission, status, await, result, continuation)
	// must carry the transport liveness options, ahead of the destination.
	argv = string(mustRead(t, filepath.Join(f.root, "ssh-argv")))
	destination := "--\noperator@ssh-alias\n"
	options := "-o\nBatchMode=yes\n-o\nConnectTimeout=10\n-o\nServerAliveInterval=10\n-o\nServerAliveCountMax=3\n"
	if calls := strings.Count(argv, destination); calls < 6 || strings.Count(argv, options+destination) != calls {
		t.Fatalf("SSH steps missing transport liveness options: %s", argv)
	}
}
func TestRemoteDelegatePoolDoesNotSelectLocally(t *testing.T) {
	f := newRemoteFixture(t)
	f.selectRequest(t, `{"pool":"implement","host":"remote"}`)
	code, out := f.invoke("pool task")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(f.root, "staged-request"))), `"pool":"implement"`) {
		t.Fatal("selector changed")
	}
	for _, path := range []string{f.count, f.probe} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("local runtime touched: %s", path)
		}
	}
}
func TestRemoteDelegateFailures(t *testing.T) {
	for _, tc := range []struct {
		name, exit, body, diagnostic string
		retry                        bool
		code                         int
	}{
		{"connection", "255", "", "remote_failure", true, 11},
		{"missing", "127", "", "delegate_remote_agentctl_missing", false, 6},
		{"cwd", "2", `{"ok":false,"schema_version":1,"error":{"code":"usage","message":"working directory must exist","retryable":false,"exit_code":2,"details":{"diagnostic_code":"delegate_invalid_cwd"},"next_actions":[]}}`, "delegate_invalid_cwd", false, 2},
		{"garbage", "0", "garbage", "delegate_remote_receipt_invalid", false, 11},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRemoteFixture(t)
			for name, body := range map[string]string{"fail": tc.exit, "fail-output": tc.body, "diagnostic": "remote diagnostic tail"} {
				if err := os.WriteFile(filepath.Join(f.root, name), []byte(body), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// Fail launch rather than the staging call for receipt tests.
			script := string(mustRead(t, filepath.Join(f.root, "fake-ssh")))
			script = strings.Replace(script, "if [ -f fail ]; then", `if [ -f fail ] && [ "${3#umask}" = "$3" ]; then`, 1)
			if err := os.WriteFile(filepath.Join(f.root, "fake-ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			code, out := f.invoke("task")
			if code != tc.code || !strings.Contains(out, tc.diagnostic) || !strings.Contains(out, "remote diagnostic tail") || !strings.Contains(out, fmtRetry(tc.retry)) {
				t.Fatalf("%d %s", code, out)
			}
			if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
				t.Fatal("failure created local execution")
			}
		})
	}
}
func fmtRetry(v bool) string {
	if v {
		return `"retryable":true`
	}
	return `"retryable":false`
}
func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestRemoteWaitAndRepairExclusion(t *testing.T) {
	f := newRemoteFixture(t)
	code, out := f.invoke("task", "--wait", "--content", "--require-result-source", "native_result", "--min-result-bytes", "4")
	if code != 0 || out != "remote verified answer\n" {
		t.Fatalf("%d %s", code, out)
	}
	code, out = f.command("", "recent")
	if code != 0 {
		t.Fatal(out)
	}
	var doc struct {
		Result struct {
			Executions []model.Execution `json:"executions"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &doc); err != nil || len(doc.Result.Executions) != 1 {
		t.Fatal(out)
	}
	e := f.execution(t, doc.Result.Executions[0].ID.String())
	acks := store.AcknowledgementIndex{}
	if _, ok := projectInbox(e, time.Now(), time.Minute, acks, output.JSON); ok {
		t.Fatal("remote in inbox")
	}
	report := classifyReconcile([]model.Execution{e}, acks, e.OriginHostID, time.Now(), reconcileOptions{}, func(processIdentity) processProof { t.Fatal("remote process probe"); return processProof{} })
	if len(report.Collect.Executions) != 0 || len(report.Unproven) != 0 {
		t.Fatalf("remote reconcile: %+v", report)
	}
}
func TestResolveRemoteHost(t *testing.T) {
	profile := config.Profile{Route: &config.Route{ThisHost: "here", Hosts: map[string]string{"studio": "there"}, SSHHosts: map[string]string{"there": "user@alias"}}}
	host, local, err := resolveDelegateHost(" STUDIO ", profile)
	if err != nil || local || host != "there" || sshDestination(profile, host) != "user@alias" {
		t.Fatalf("%s %v %v", host, local, err)
	}
	if sshDestination(config.Profile{}, host) != "there" {
		t.Fatal("default destination")
	}
	if _, _, err := resolveDelegateHost("missing", profile); err == nil {
		t.Fatal("unresolved host")
	}
	profile.Route.Hosts["there"] = "elsewhere"
	if _, _, err := resolveDelegateHost("there", profile); err == nil || err.Code != output.CodeAmbiguousReference {
		t.Fatal("ambiguous host", err)
	}
}

func TestRemotePlanAndMulticaBoundary(t *testing.T) {
	f := newRemoteFixture(t)
	code, out := f.invoke("task", "--plan")
	if code != 0 || !strings.Contains(out, `"request_staged":true`) || !strings.Contains(out, `"plan":true`) {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
		t.Fatal("plan journaled admission")
	}
	if strings.Contains(out, `"id":"`+remoteTestID+`"`) || !strings.Contains(out, `"remote_id":"`+remoteTestID+`"`) {
		t.Fatal("remote plan leaked a caller handle", out)
	}
	if !strings.Contains(string(mustRead(t, filepath.Join(f.root, "ssh-argv"))), "'--plan'") {
		t.Fatal("plan not forwarded")
	}
	before := mustRead(t, filepath.Join(f.root, "ssh-argv"))
	code, out = f.invoke("task", "--authority", "multica")
	if code != 5 || !strings.Contains(out, "delegate_multica_result_unavailable") {
		t.Fatalf("%d %s", code, out)
	}
	if !bytes.Equal(before, mustRead(t, filepath.Join(f.root, "ssh-argv"))) {
		t.Fatal("Multica used SSH")
	}
}

func TestRemoteLostReceiptReplay(t *testing.T) {
	f := newRemoteFixture(t)
	script := string(mustRead(t, filepath.Join(f.root, "fake-ssh")))
	script = strings.Replace(script, "cat receipt", `if [ ! -f dropped ]; then touch dropped; echo lost-receipt >&2; exit 255; fi; cat receipt`, 1)
	if err := os.WriteFile(filepath.Join(f.root, "fake-ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("exact same task")
	if code != 11 || !strings.Contains(out, `"retryable":true`) {
		t.Fatalf("%d %s", code, out)
	}
	if _, err := os.Stat(f.journal); !os.IsNotExist(err) {
		t.Fatal("lost receipt journaled unproven admission")
	}
	code, out = f.invoke("exact same task")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if string(mustRead(t, filepath.Join(f.root, "launched"))) != "launch\n" {
		t.Fatal("duplicate launch")
	}
	code, out = f.invoke("different task")
	if code != 8 {
		t.Fatalf("changed input: %d %s", code, out)
	}
}

func TestRemoteFailedAwaitAcknowledgesLocally(t *testing.T) {
	f := newRemoteFixture(t)
	code, out := f.invoke("task")
	if code != 0 {
		t.Fatal(out)
	}
	local := remoteLocalID(t, out)
	f.writeJSON(t, "terminal", map[string]any{"id": remoteTestID, "origin_host_id": remoteTestHost, "state": "failed", "liveness": "exited"})
	f.writeOutcome(t, model.StateFailed)
	script := string(mustRead(t, filepath.Join(f.root, "fake-ssh")))
	failure := `{"ok":false,"schema_version":1,"error":{"code":"execution_failed","message":"remote failed","retryable":false,"exit_code":11,"details":{"diagnostic_code":"upstream_native_error"},"next_actions":[]}}`
	script = strings.Replace(script, `*"'await'"*) cat terminal ;;`, `*"'await'"*) printf '%s' `+shellQuote(failure)+`; exit 11 ;;`, 1)
	if err := os.WriteFile(filepath.Join(f.root, "fake-ssh"), []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	code, out = f.command("", "await", local, "--content")
	if code != 11 || !strings.Contains(out, "upstream_native_error") || !strings.Contains(out, local) {
		t.Fatalf("%d %s", code, out)
	}
	e := f.execution(t, local)
	if e.State != model.StateFailed {
		t.Fatal(e.State)
	}
	journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.GetAcknowledgement(context.Background(), e.ID); err != nil {
		t.Fatal("failed outcome not acknowledged", err)
	}
}

func TestRemoteIdentityMismatchAndLocalCancel(t *testing.T) {
	f := newRemoteFixture(t)
	code, out := f.invoke("task")
	if code != 0 {
		t.Fatal(out)
	}
	local := remoteLocalID(t, out)
	before := mustRead(t, f.journal)
	f.writeJSON(t, "terminal", map[string]any{"id": remoteNextID, "origin_host_id": remoteTestHost, "state": "completed"})
	code, out = f.command("", "status", local)
	if code != 11 || !strings.Contains(out, "delegate_remote_receipt_invalid") {
		t.Fatalf("%d %s", code, out)
	}
	code, out = f.command("", "cancel", local, "--orphan-if-gone")
	if code != 5 || !strings.Contains(out, "remote_cancel_unavailable") {
		t.Fatalf("%d %s", code, out)
	}
	if !bytes.Equal(before, mustRead(t, f.journal)) {
		t.Fatal("invalid remote status/cancel changed local journal")
	}
}

func TestRemoteHelpAndSchemas(t *testing.T) {
	for _, name := range []string{"delegate", "status", "await", "continue"} {
		help := helpTopics()[name]
		body, _ := json.Marshal(help)
		if !strings.Contains(string(body), "SSH") {
			t.Fatalf("%s help omits SSH disclosure", name)
		}
	}
	for _, name := range []string{"delegate-result.schema.json", "continue-result.schema.json", "execution.schema.json", "recent-result.schema.json"} {
		body := mustRead(t, filepath.Join(schemaRepositoryRoot(t), "schemas", name))
		if !strings.Contains(string(body), `"remote"`) {
			t.Fatalf("%s lacks remote contract", name)
		}
	}
	body := mustRead(t, filepath.Join(schemaRepositoryRoot(t), "schemas", "config-bundle.schema.json"))
	if !strings.Contains(string(body), `"ssh_hosts"`) {
		t.Fatal("config schema omits ssh_hosts")
	}
}

func TestRemoteConcurrentReplayAndCollection(t *testing.T) {
	f := newRemoteFixture(t)
	var wg sync.WaitGroup
	outs := make([]string, 2)
	codes := make([]int, 2)
	for i := range outs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], outs[i] = f.invoke("concurrent task") }(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 {
			t.Fatalf("%d: %d %s", i, code, outs[i])
		}
	}
	local := remoteLocalID(t, outs[0])
	if remoteLocalID(t, outs[1]) != local {
		t.Fatal("concurrent admissions made multiple local ids")
	}
	for i := range outs {
		wg.Add(1)
		go func(i int) { defer wg.Done(); codes[i], outs[i] = f.command("", "await", local, "--content") }(i)
	}
	wg.Wait()
	for i, code := range codes {
		if code != 0 || outs[i] != "remote verified answer\n" {
			t.Fatalf("%d: %d %s", i, code, outs[i])
		}
	}
}

func TestRemoteTerminalAdmissionCollectsMissingOutcome(t *testing.T) {
	f := newRemoteFixture(t)
	body := strings.Replace(string(mustRead(t, filepath.Join(f.root, "receipt"))), `"state":"running"`, `"state":"completed"`, 1)
	if err := os.WriteFile(filepath.Join(f.root, "receipt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("already done")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	local := remoteLocalID(t, out)
	if f.execution(t, local).State != model.StateCompleted {
		t.Fatal("receipt lost terminal state")
	}
	code, out = f.command("", "await", local, "--content")
	if code != 0 || out != "remote verified answer\n" {
		t.Fatalf("%d %s", code, out)
	}
}

func TestRemoteTerminalTransportFailureDoesNotAcknowledge(t *testing.T) {
	f := newRemoteFixture(t)
	body := strings.Replace(string(mustRead(t, filepath.Join(f.root, "receipt"))), `"state":"running"`, `"state":"completed"`, 1)
	if err := os.WriteFile(filepath.Join(f.root, "receipt"), []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	code, out := f.invoke("already done")
	if code != 0 {
		t.Fatal(out)
	}
	local := remoteLocalID(t, out)
	for name, value := range map[string]string{"fail": "255", "diagnostic": "connection lost", "fail-output": ""} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out = f.command("", "await", local, "--content")
	if code != 11 {
		t.Fatalf("%d %s", code, out)
	}
	e := f.execution(t, local)
	journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close()
	if _, err := journal.GetAcknowledgement(context.Background(), e.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("transport failure acknowledged an undelivered result", err)
	}
}

func TestRemoteReplayFreezesDestinationAndRetainsLocalID(t *testing.T) {
	f := newRemoteFixture(t)
	code, out := f.invoke("frozen routing")
	if code != 0 {
		t.Fatal(out)
	}
	local := remoteLocalID(t, out)
	cfg, err := config.Load(f.config)
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profiles["test"]
	profile.Route.SSHHosts["other-workstation"] = "changed-destination"
	cfg.Profiles["test"] = profile
	body, _ := json.Marshal(cfg)
	if err := os.WriteFile(f.config, body, 0600); err != nil {
		t.Fatal(err)
	}
	code, out = f.invoke("frozen routing")
	if code != 0 || remoteLocalID(t, out) != local {
		t.Fatalf("%d %s", code, out)
	}
	if strings.Contains(string(mustRead(t, filepath.Join(f.root, "ssh-argv"))), "changed-destination") {
		t.Fatal("replay used changed routing")
	}
	for name, value := range map[string]string{"fail": "255", "diagnostic": "connection lost", "fail-output": ""} {
		if err := os.WriteFile(filepath.Join(f.root, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	code, out = f.invoke("frozen routing")
	if code != 11 || !strings.Contains(out, `"execution_id":"`+local+`"`) {
		t.Fatalf("%d %s", code, out)
	}
}

func TestRemoteCapacityDoesNotDebitLocalAccounts(t *testing.T) {
	f := newRemoteFixture(t)
	var doc map[string]any
	if err := json.Unmarshal(mustRead(t, filepath.Join(f.root, "receipt")), &doc); err != nil {
		t.Fatal(err)
	}
	receipt := doc["result"].(map[string]any)
	receipt["capacity"] = map[string]any{"pool": "implement", "mode": "priority", "account": "shared-name", "reason": "first_eligible", "facts_status": "missing", "considered": []any{map[string]any{"account": "shared-name", "model": "remote-only-model", "threshold": 0.95, "eligible": true, "recent_launches": 0}}}
	f.writeJSON(t, "receipt", receipt)
	f.selectRequest(t, `{"pool":"implement","host":"remote"}`)
	code, out := f.invoke("remote capacity")
	if code != 0 {
		t.Fatal(out)
	}
	e := f.execution(t, remoteLocalID(t, out))
	if strings.Contains(string(e.Delegation.Requested), `"pool"`) || e.Delegation.Capacity.Pool != "implement" {
		t.Fatal("pool journal projection is not rollback-safe", string(e.Delegation.Requested))
	}
	var stdout, stderr bytes.Buffer
	a := testApp(&stdout, &stderr)
	counts, err := a.recentPoolLaunches(context.Background(), common{journalPath: f.journal}, time.Now(), time.Hour)
	if err != nil || len(counts) != 0 {
		t.Fatalf("remote work debited local accounts: %v %v", counts, err)
	}
}

func TestRemoteAwaitBoundsStalledSSH(t *testing.T) {
	for _, stage := range []string{"await", "result"} {
		t.Run(stage, func(t *testing.T) {
			f := newRemoteFixture(t)
			code, out := f.invoke("task")
			if code != 0 {
				t.Fatal(out)
			}
			local := remoteLocalID(t, out)
			before := mustRead(t, f.journal)
			path := filepath.Join(f.root, "fake-ssh")
			script := string(mustRead(t, path))
			// exec keeps the stalled subprocess owned by CommandContext, so
			// cancellation reaps it without leaving a sleeping shell child.
			script = strings.Replace(script, "case \"$3\" in", "case \"$3\" in\n *\"'"+stage+"'\"*) exec sleep 30 ;;", 1)
			if err := os.WriteFile(path, []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			const timeout = 100 * time.Millisecond
			started := time.Now()
			code, out = f.command("", "await", local, "--timeout", timeout.String(), "--content")
			elapsed := time.Since(started)
			if code != output.ExitCodeFor(output.CodeTimeout) || !strings.Contains(out, "await deadline elapsed") || !strings.Contains(out, `"execution_id":"`+local+`"`) {
				t.Fatalf("exit=%d elapsed=%s output=%s", code, elapsed, out)
			}
			if elapsed > timeout+remoteAwaitReportGrace+time.Second {
				t.Fatalf("await exceeded its deadline and reporting grace: %s", elapsed)
			}
			if !bytes.Equal(before, mustRead(t, f.journal)) {
				t.Fatal("stalled collection changed or acknowledged local execution")
			}
		})
	}
}

func TestRemoteAdmissionPreservesReceiptLiveness(t *testing.T) {
	for _, tc := range []struct {
		receipt string
		want    model.Liveness
	}{
		{"alive", model.LivenessAlive},
		{"blocked", model.LivenessBlocked},
		{"", model.LivenessUnknown},
	} {
		t.Run(string(tc.want), func(t *testing.T) {
			f := newRemoteFixture(t)
			var document map[string]any
			if err := json.Unmarshal(mustRead(t, filepath.Join(f.root, "receipt")), &document); err != nil {
				t.Fatal(err)
			}
			receipt := document["result"].(map[string]any)
			if tc.receipt != "" {
				receipt["liveness"] = tc.receipt
			}
			f.writeJSON(t, "receipt", receipt)
			code, out := f.invoke("task")
			if code != 0 || !strings.Contains(out, `"liveness":"`+string(tc.want)+`"`) {
				t.Fatalf("%d %s", code, out)
			}
			local := remoteLocalID(t, out)
			if got := f.execution(t, local).Liveness; got != tc.want {
				t.Fatalf("stored liveness=%s want=%s", got, tc.want)
			}
			code, out = f.command("", "recent")
			if code != 0 || !strings.Contains(out, `"liveness":"`+string(tc.want)+`"`) {
				t.Fatalf("recent: %d %s", code, out)
			}
		})
	}
}

func TestRemoteDelegateAndContinueWaitBoundStalledSSH(t *testing.T) {
	for _, command := range []string{"delegate", "continue"} {
		for _, stage := range []string{"await", "status", "result"} {
			t.Run(command+"/"+stage, func(t *testing.T) {
				f := newRemoteFixture(t)
				var source string
				if command == "continue" {
					code, out := f.invoke("source task", "--wait")
					if code != 0 {
						t.Fatal(out)
					}
					source = remoteLocalID(t, out)
					for _, name := range []string{"terminal", "result"} {
						path := filepath.Join(f.root, name)
						body := strings.ReplaceAll(string(mustRead(t, path)), remoteTestID, remoteNextID)
						if err := os.WriteFile(path, []byte(body), 0600); err != nil {
							t.Fatal(err)
						}
					}
				}
				path := filepath.Join(f.root, "fake-ssh")
				script := string(mustRead(t, path))
				// Exec lets CommandContext reap the stall without orphaning a child.
				script = strings.Replace(script, "case \"$3\" in", "case \"$3\" in\n *\"'"+stage+"'\"*) exec sleep 30 ;;", 1)
				if stage == "status" {
					// A failed remote await needs a status lookup before collection.
					script = strings.Replace(script, `*"'await'"*) cat terminal ;;`, `*"'await'"*) printf '%s' '{"ok":false,"schema_version":1,"error":{"code":"execution_failed","message":"failed","retryable":false,"exit_code":11,"details":{},"next_actions":[]}}'; exit 11 ;;`, 1)
				}
				if err := os.WriteFile(path, []byte(script), 0700); err != nil {
					t.Fatal(err)
				}
				const timeout = 100 * time.Millisecond
				started := time.Now()
				var code int
				var out string
				if command == "delegate" {
					code, out = f.invoke("task", "--wait", "--content", "--timeout", timeout.String())
				} else {
					code, out = f.command("next task", "continue", source, "--request-key", "bounded-next", "--prompt-stdin", "--wait", "--content", "--timeout", timeout.String())
				}
				finished := time.Now()
				elapsed := finished.Sub(started)
				if code != output.ExitCodeFor(output.CodeTimeout) || !strings.Contains(out, "await deadline elapsed") {
					t.Fatalf("exit=%d elapsed=%s output=%s", code, elapsed, out)
				}
				var document struct {
					Error struct {
						Details struct {
							ExecutionID string `json:"execution_id"`
						} `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(out), &document); err != nil {
					t.Fatal(err)
				}
				execution := f.execution(t, document.Error.Details.ExecutionID)
				if execution.DeadlineAt == nil || execution.State.Terminal() {
					t.Fatalf("unexpected timed-out observation: %+v", execution)
				}
				// Admission precedes the recorded deadline; bound collection itself.
				settled := execution.DeadlineAt.Add(10 * time.Second)
				if finished.Before(settled) || finished.After(settled.Add(remoteAwaitReportGrace+time.Second)) {
					t.Fatalf("%s --wait did not respect its deadline, settle window, and reporting grace: elapsed=%s deadline=%s finished=%s", command, elapsed, execution.DeadlineAt, finished)
				}
				if source != "" && (len(execution.Supersedes) != 1 || execution.Supersedes[0].String() != source) {
					t.Fatal("continuation lost its local source")
				}
				journal, err := store.Open(f.journal, store.Options{ReadOnly: true})
				if err != nil {
					t.Fatal(err)
				}
				_, ackErr := journal.GetAcknowledgement(context.Background(), execution.ID)
				journal.Close()
				if !errors.Is(ackErr, store.ErrNotFound) {
					t.Fatal("stalled collection acknowledged an undelivered result", ackErr)
				}
			})
		}
	}
}

func TestRemoteRequestStagingCommands(t *testing.T) {
	for _, mode := range []string{"launch", "failure", "plan"} {
		t.Run(mode, func(t *testing.T) {
			f := newRemoteFixture(t)
			target := filepath.Join(f.root, "keep-me")
			if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
				t.Fatal(err)
			}
			oldName := "agentctl-delegate-" + strings.TrimPrefix(sha256Digest(mustRead(t, f.request)), "sha256:")[:16] + ".json"
			if err := os.Symlink(target, filepath.Join(f.root, oldName)); err != nil {
				t.Fatal(err)
			}
			var flags []string
			if mode == "plan" {
				flags = []string{"--plan"}
			}
			code, out := f.invoke("task", flags...)
			if code != 0 || (mode == "plan" && !strings.Contains(out, `"request_staged":true`)) {
				t.Fatalf("%d %s", code, out)
			}
			dir := string(mustRead(t, filepath.Join(f.root, "staged-dir")))
			for name, want := range map[string]os.FileMode{dir: 0700, filepath.Join(dir, "request.json"): 0600} {
				info, err := os.Stat(name)
				if err != nil || info.Mode().Perm() != want {
					t.Fatalf("staging permissions: %s info=%v err=%v", name, info, err)
				}
			}
			if string(mustRead(t, target)) != "unchanged" {
				t.Fatal("staging followed the predictable symlink")
			}
			// Run the actual generated launch wrapper against a harmless CLI stub.
			bin := t.TempDir()
			script := "#!/bin/sh\ncat > /dev/null\ncat " + shellQuote(filepath.Join(f.root, "receipt")) + "\n"
			wantExit := 0
			if mode == "failure" {
				wantExit = 11
				script += "exit 11\n"
			}
			if err := os.WriteFile(filepath.Join(bin, "agentctl"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, "/bin/sh", "-c", string(mustRead(t, filepath.Join(f.root, "launch-command"))))
			cmd.Env = append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			cmd.Stdin = strings.NewReader("task")
			body, err := cmd.CombinedOutput()
			gotExit := 0
			if err != nil {
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatal(err)
				}
				gotExit = exit.ExitCode()
			}
			if gotExit != wantExit {
				t.Fatalf("exit=%d want=%d output=%s", gotExit, wantExit, body)
			}
			if _, err := os.Stat(dir); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("launch left its staging directory", err)
			}
		})
	}
}

func TestRemoteWaitReplayAfterDeadlineUsesCollectedOutcome(t *testing.T) {
	for _, command := range []string{"delegate", "continue"} {
		t.Run(command, func(t *testing.T) {
			f := newRemoteFixture(t)
			code, out := f.invoke("task", "--timeout", "1s", "--wait")
			if code != 0 {
				t.Fatal(out)
			}
			local := remoteLocalID(t, out)
			args := []string{"delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--timeout", "1s", "--wait"}
			if command == "continue" {
				args = []string{"continue", local, "--request-key", "collected-next", "--prompt-stdin", "--timeout", "1s", "--wait"}
				for _, name := range []string{"terminal", "result"} {
					p := filepath.Join(f.root, name)
					if err := os.WriteFile(p, []byte(strings.ReplaceAll(string(mustRead(t, p)), remoteTestID, remoteNextID)), 0600); err != nil {
						t.Fatal(err)
					}
				}
				code, out = f.command("task", args...)
				if code != 0 {
					t.Fatal(out)
				}
				local = remoteLocalID(t, out)
			}
			execution := f.execution(t, local)
			past := time.Now().Add(-time.Minute)
			execution.DeadlineAt = &past
			journal, err := store.Open(f.journal, store.Options{})
			if err != nil {
				t.Fatal(err)
			}
			_, err = journal.UpdateExecution(context.Background(), execution, execution.Revision)
			journal.Close()
			if err != nil {
				t.Fatal(err)
			}
			called := filepath.Join(f.root, "unexpected-ssh")
			script := "#!/bin/sh\necho called > " + shellQuote(called) + "\nexit 255\n"
			if err := os.WriteFile(filepath.Join(f.root, "fake-ssh"), []byte(script), 0700); err != nil {
				t.Fatal(err)
			}
			code, out = f.command("task", append(args, "--content")...)
			if code != 0 || out != "remote verified answer\n" {
				t.Fatalf("replay: %d %s", code, out)
			}
			if _, err := os.Stat(called); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("collected replay contacted SSH", err)
			}
		})
	}
}
