package anxrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

func TestReReviewNoFreeTextOutcome(t *testing.T) {
	for _, text := range []string{`{"API_TOKEN": "private-customer-token"}`, `"PASSWORD"="private-customer-password"`, `C:\Users\private\file`, `2001:db8:1234::5`, "Implemented the fix."} {
		r := testRegistration("exec-a", PhaseTerminal, 1)
		r.NativeSession = nil
		r.Outcome = ProjectOutcome(model.Outcome{Availability: model.OutcomeStored, Content: &model.OutcomeContent{Text: "---anx-summary---\n" + text + "\n---end-anx-summary---"}})
		projection, _ := json.Marshal(r.Outcome)
		if strings.Contains(string(projection), `"summary"`) {
			t.Fatal("summary schema field remains")
		}
		s := testSpool(t)
		entry, err := s.Enqueue(r)
		if err != nil {
			t.Fatal(err)
		}
		runner := &fakeRunner{}
		if err := (&CLISender{run: runner.run, Resolve: fixtureResolve(testRegistration("exec-a", PhaseTerminal, 1))}).Send(context.Background(), entry.Registration); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(entry)
		for _, data := range append(runner.stdins, string(raw)) {
			if strings.Contains(data, text) || strings.Contains(data, `\"API_TOKEN\"`) {
				t.Errorf("free-text result escaped: %s", data)
			}
		}
	}
}

func TestReReviewCredentialCoordinates(t *testing.T) {
	for _, prefix := range []string{"gho_", "ghs_", "ghu_", "ghr_"} {
		for _, field := range []string{"owner", "repo"} {
			link := Link{Kind: LinkKindPR, Owner: "o", Repo: "r", Number: 1}
			if field == "owner" {
				link.Owner = prefix + strings.Repeat("a", 30)
			} else {
				link.Repo = prefix + strings.Repeat("a", 36)
			}
			if _, code := validateLink(link); code != EgressSecretShape {
				t.Fatalf("credential coordinate accepted: %s %s %d", field, prefix, code)
			}
		}
		if len(ProjectOutcome(model.Outcome{Content: &model.OutcomeContent{Text: "https://github.com/o/" + prefix + strings.Repeat("a", 36) + "/pull/1"}}).Links) > 0 {
			t.Errorf("credential evidence accepted: %s", prefix)
		}
	}
	if cards := ResolveCards([]string{"anx.card.glpat-abcdefghijklmnopqrst"}); len(cards) != 0 {
		t.Fatalf("credential slug accepted: %v", cards)
	}

}

func TestReReviewSecretShapesInEveryString(t *testing.T) {
	shapes := []string{"gho_", "ghs_", "ghp_", "ghu_", "ghr_", "github_pat_", "glpat-", "sk-", "xoxa-", "xoxb-", "xoxp-", "xoxr-", "AKIA", "AIza", "hf_", "npm_", "pypi-", "ya29."}
	for _, prefix := range shapes {
		if code := stringCode(prefix+"example", 128, false); code != EgressSecretShape {
			t.Errorf("missing prefix: %s", prefix)
		}
	}
	for _, value := range []string{"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJleGFtcGxlIn0.signaturevalue", "abcdefghijklmnopqrstuvwxyz0123456789", "cR8yLa3mK9xQ7vZ1bF6dT4wN2pJ5sU0h"} {
		if code := stringCode(value, 128, false); code != EgressSecretShape {
			t.Errorf("missed shape: %s", value)
		}
	}
	if code := stringCode(strings.Repeat("a", 129), 128, false); code != EgressLength {
		t.Fatal("missing length limit")
	}
	secret := "glpat-" + "abcdefghijklmnopqrst"
	for name, mutate := range map[string]func(*Registration){
		"card":    func(r *Registration) { r.CardRefs = []string{"card:" + secret} },
		"model":   func(r *Registration) { r.Model = secret },
		"harness": func(r *Registration) { r.Adapter = secret },
		"owner": func(r *Registration) {
			r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindPR, Owner: secret, Repo: "r", Number: 1}}}
		},
		"repo": func(r *Registration) {
			r.Outcome = &Outcome{Availability: AvailabilityStored, Links: []Link{{Kind: LinkKindIssue, Owner: "o", Repo: secret, Number: 1}}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := testRegistration("exec-a", PhaseTerminal, 1)
			r.NativeSession = nil
			mutate(&r)
			s := testSpool(t)
			s.Catalog = []route.ModelRecord{{Adapter: r.Adapter, Model: secret}}
			entry, err := s.Enqueue(r)
			if err != nil {
				t.Fatal(err)
			}
			if entry.Registration.EgressErrorCode != EgressSecretShape {
				t.Fatalf("missing diagnostic: %+v", entry)
			}
			runner := &fakeRunner{}
			// Card rejection leaves no target; all other optional drops still deliver.
			_ = (&CLISender{run: runner.run, Resolve: fixtureResolve(testRegistration("exec-a", PhaseTerminal, 1))}).Send(context.Background(), entry.Registration)
			raw, _ := json.Marshal(entry)
			for _, data := range append(runner.stdins, string(raw)) {
				if strings.Contains(data, secret) {
					t.Fatal("secret escaped")
				}
			}
			files, _ := filepath.Glob(filepath.Join(s.Root(), "pending", "*.json"))
			for _, file := range files {
				raw, _ := os.ReadFile(file)
				if strings.Contains(string(raw), secret) {
					t.Fatal("secret persisted")
				}
			}
		})
	}
	for _, field := range []string{"host", "exec"} {
		s := testSpool(t)
		r := testRegistration("exec-a", PhaseStart, 1)
		if field == "host" {
			r.OriginHostID = secret
		} else {
			r.ExternalID = secret
		}
		if _, err := s.Enqueue(r); err == nil {
			t.Fatal("required identity accepted")
		} else {
			class, code := ErrorClass(err)
			if class != ReasonEgressRejected || code != EgressSecretShape {
				t.Fatal(class, code)
			}
		}
		pending, _ := s.ListPending()
		if len(pending) != 0 {
			t.Fatal("invalid registration persisted")
		}
		receipt := s.LastRejection()
		if receipt == nil || receipt.Code != EgressSecretShape {
			t.Fatal("no value-free rejection receipt")
		}
		if err := filepath.WalkDir(s.Root(), func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() {
				raw, _ := os.ReadFile(path)
				if strings.Contains(string(raw), secret) {
					t.Fatal("required secret persisted")
				}
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestReReviewLegacySummaryAndURLDroppedAtReadBoundary(t *testing.T) {
	s := testSpool(t)
	entry, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 1))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(entry)
	var old map[string]any
	_ = json.Unmarshal(raw, &old)
	r := old["registration"].(map[string]any)
	r["schema_version"] = 2
	r["outcome"] = map[string]any{"availability": "stored", "summary": `{"API_TOKEN":"private-customer-token"}`, "summary_omitted_reason": "redaction_failed", "links": []any{map[string]any{"kind": "pr", "ref": "https://github.com/o/ghs_" + strings.Repeat("a", 36) + "/pull/1"}}}
	raw, _ = json.Marshal(old)
	path := s.pendingPath(entryKey(entry.ExecutionID, entry.Phase))
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	entries, err := s.ListPending()
	if err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{}
	if err := (&CLISender{run: runner.run, Resolve: fixtureResolve(testRegistration("exec-a", PhaseTerminal, 1))}).Send(context.Background(), entries[0].Registration); err != nil {
		t.Fatal(err)
	}
	for _, data := range runner.stdins {
		for _, bad := range []string{"private-customer-token", "https://", "summary_omitted_reason"} {
			if strings.Contains(data, bad) {
				t.Fatal("legacy field escaped")
			}
		}
	}
}

func TestReReviewFlushLockWaitCancellation(t *testing.T) {
	s := testSpool(t)
	unlock, err := s.lock("flush", true)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := s.Flush(ctx, &recordingSender{}, 0); err != context.DeadlineExceeded {
		t.Fatalf("wait cancellation: %v", err)
	}
}

func TestReReviewServerAndRoutingCoordinatesGuarded(t *testing.T) {
	for _, value := range []string{"ghs_" + strings.Repeat("a", 36), "glpat-" + "abcdefghijklmnopqrst", "abcdefghijklmnopqrstuvwxyz0123456789", strings.Repeat("a", 129)} {
		if safeServerCoordinate(value) {
			t.Fatal("unsafe server coordinate accepted")
		}
	}
	if !safeServerCoordinate("agent_8d92a65c-dea3-4125-8402-d8cdc8fc8a7c") {
		t.Fatal("typed server UUID rejected")
	}
	runner := &fakeRunner{}
	sender := &CLISender{options: CLIOptions{WorkspaceAlias: "glpat-" + "abcdefghijklmnopqrst"}, run: runner.run}
	if err := sender.Send(context.Background(), testRegistration("exec-a", PhaseStart, 1)); err == nil {
		t.Fatal("unsafe workspace alias accepted")
	}
	if len(runner.stdins) != 0 {
		t.Fatal("unsafe alias reached subprocess")
	}
}

func TestReReviewWaitingFlushDeliversTerminal(t *testing.T) {
	s := testSpool(t)
	if _, err := s.Enqueue(testRegistration("exec-a", PhaseStart, 0)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	held, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		_, err := s.Flush(ctx, senderFunc(func(context.Context, Registration) error { close(held); <-release; return nil }), 0)
		first <- err
	}()
	<-held
	if _, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		close(release)
		t.Fatal(err)
	}
	second := make(chan FlushResult, 1)
	secondErr := make(chan error, 1)
	go func() { r, err := s.Flush(ctx, &recordingSender{}, 0); second <- r; secondErr <- err }()
	select {
	case <-second:
		close(release)
		<-first
		t.Fatal("contending worker discarded the terminal wakeup")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatal(err)
	}
	if r := <-second; r.Delivered != 1 {
		t.Fatalf("terminal not delivered after owner release: %+v", r)
	}
	if err := <-secondErr; err != nil {
		t.Fatal(err)
	}
	if pending, _ := s.ListPending(); len(pending) != 0 {
		t.Fatal("terminal stranded")
	}
}
