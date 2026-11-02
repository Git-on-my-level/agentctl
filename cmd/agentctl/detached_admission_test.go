package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func TestDetachedAdmissionSurvivesWorkerAndReceiptFailures(t *testing.T) {
	for _, scenario := range []string{"start_failure", "worker_failure", "receipt_eof", "receipt_wrong_id"} {
		t.Run(scenario, func(t *testing.T) {
			f := newDelegateFixture(t, delegateSuccessScript)
			var out, stderr bytes.Buffer
			a := testApp(&out, &stderr)
			a.stdin = strings.NewReader("private reservation prompt")
			starts := 0
			a.startDetachedWorker = func(cmd *exec.Cmd) error {
				starts++
				var payload detachedLaunch
				if err := json.NewDecoder(cmd.Stdin).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				for _, arg := range payload.Argv {
					if strings.Contains(arg, "private reservation prompt") {
						t.Fatal("prompt attached before worker handoff; worker would append it twice")
					}
				}
				if scenario == "start_failure" {
					return errors.New("injected worker unavailable")
				}
				cmd.Path = "/bin/sh"
				cmd.Args = []string{"sh", "-c", "exit 0"}
				if scenario == "worker_failure" {
					cmd.Args = []string{"sh", "-c", `printf '%s' '{"ok":false,"error":{"code":"usage","message":"protocol refused","exit_code":2,"retryable":false,"details":{}}}'`}
				}
				if scenario == "receipt_wrong_id" {
					cmd.Args = []string{"sh", "-c", `printf '%s' '{"ok":true,"result":{"id":"unrelated"}}'`}
				}
				// Avoid os/exec's stdin copier seeing a closed reader as an error.
				cmd.Stdin = nil
				return cmd.Start()
			}
			args := []string{"--config", f.config, "--journal", f.journal, "delegate", "--cwd", f.root, "--request-file", f.request, "--prompt-stdin", "--detach"}
			code := a.run(context.Background(), args)
			failure := scenario == "start_failure" || scenario == "worker_failure"
			if (failure && code == 0) || (!failure && code != 0) {
				t.Fatalf("code=%d %s", code, out.String())
			}
			j, err := store.Open(f.journal, store.Options{ReadOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			rows, err := j.ListExecutions(context.Background(), false)
			j.Close()
			if err != nil || len(rows) != 1 {
				t.Fatalf("rows=%d err=%v", len(rows), err)
			}
			if !strings.Contains(out.String(), rows[0].ID.String()) || strings.Contains(out.String(), "detached_receipt_unknown") {
				t.Fatal(out.String())
			}
			if failure && rows[0].State != model.StateFailed {
				t.Fatalf("state=%s", rows[0].State)
			}
			if !failure {
				var doc struct {
					Result struct {
						LaunchStatus string `json:"launch_status"`
					} `json:"result"`
				}
				if json.Unmarshal(out.Bytes(), &doc) != nil || doc.Result.LaunchStatus != "admitted" {
					t.Fatal(out.String())
				}
			}
			out.Reset()
			a.stdin = strings.NewReader("private reservation prompt")
			if code := a.run(context.Background(), args); code != 0 {
				t.Fatalf("replay code=%d %s", code, out.String())
			}
			if starts != 1 || !strings.Contains(out.String(), rows[0].ID.String()) {
				t.Fatalf("starts=%d %s", starts, out.String())
			}
		})
	}
}

func TestDelegationRecoveryReadHonorsBusyBudget(t *testing.T) {
	f := newDelegateFixture(t, delegateSuccessScript)
	writer, err := store.Open(f.journal, store.Options{})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { defer close(done); time.Sleep(800 * time.Millisecond); writer.Close() }()
	defer func() { <-done }()
	var out, stderr bytes.Buffer
	a := testApp(&out, &stderr)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, found, problem := a.findDelegation(ctx, common{journalPath: f.journal, journalBusyBudget: 2 * time.Second}, contracts.MutationKey{Scope: "test", Key: "not-yet-admitted", InputDigest: "sha256:" + strings.Repeat("a", 64)})
	if problem != nil || found {
		t.Fatalf("found=%v problem=%v", found, problem)
	}
}
