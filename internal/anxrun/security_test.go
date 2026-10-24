package anxrun

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

func TestSecurityUnmarkedSummaryNeverLeavesHost(t *testing.T) {
	for _, text := range []string{
		"private customer notes", "-----BEGIN PRIVATE KEY-----\nSENSITIVE_KEY_MATERIAL\n-----END PRIVATE KEY-----",
		`inline FOO="private-value" here`, "https://user:private-value@github.com/o/r/pull/1",
		"edited `/Users/private/file`", "host 100.100.100.100",
	} {
		got := ProjectOutcome(model.Outcome{Availability: model.OutcomeStored, Content: &model.OutcomeContent{Text: text}}, 2048)
		if got.Summary != "" {
			t.Errorf("unmarked result escaped: %q", got.Summary)
		}
	}
}

func TestSecurityExplicitSummaryBlockIsConservative(t *testing.T) {
	wrap := func(text string) string {
		return "private surrounding text\n---anx-summary---\n" + text + "\n---end-anx-summary---\nprivate tail"
	}
	if got := MarkedSummary(wrap("Implemented the fix and passed tests."), 2048); got.Summary != "Implemented the fix and passed tests." {
		t.Fatalf("explicit summary missing: %+v", got)
	}
	for _, text := range []string{
		"-----BEGIN PRIVATE KEY-----\nSENSITIVE_KEY_MATERIAL\n-----END PRIVATE KEY-----",
		`inline FOO="private-value" here`, `export FOO='private-value'`, "https://user:private-value@github.com/o/r/pull/1",
		"edited `/Users/private/file`", "host 100.64.0.1", "host 100.127.255.254", "used sk-" + "abcdef01234567",
		"-----BEGIN OPENSSH PRIVATE KEY-----\nKEY_BODY\n-----END OPENSSH PRIVATE KEY-----",
		"token: short-value", "Authorization: Bearer short-value", "edited `/etc/private-file`", "~operator/private-file", "host fd7a:115c:a1e0::1",
		"used sk-a", "used xoxb-short",
	} {
		if got := MarkedSummary(wrap(text), 2048); got.Summary != "" || got.OmittedReason != SummaryOmittedRedactionFailed {
			t.Errorf("ambiguous opted-in text survived: %+v", got)
		}
	}
	for _, text := range []string{"---anx-summary---\nincomplete", "---end-anx-summary---\n---anx-summary---", wrap("ok") + wrap("again"), "inline ---anx-summary---\ntext\n---end-anx-summary---"} {
		if got := MarkedSummary(text, 2048); got.Summary != "" {
			t.Fatalf("malformed block accepted: %+v", got)
		}
	}
}

func TestSecurityRequestKeyHMACStableAndHostLocal(t *testing.T) {
	s := testSpool(t)
	r := BuildStart(testExecution(t, []string{"anx.card.dogfooding"}, model.StateRunning), time.Now())
	r.RequestKey = "private request key"
	first, err := s.Enqueue(r)
	if err != nil {
		t.Fatal(err)
	}
	if !digestPattern.MatchString(first.Registration.RequestKeyHash) {
		t.Fatal("missing keyed request identity")
	}
	other, _ := OpenSpool(s.Root())
	replay, err := other.Enqueue(r)
	if err != nil || replay.Registration.RequestKeyHash != first.Registration.RequestKeyHash {
		t.Fatal("hash changed on reopen")
	}
	fresh, err := testSpool(t).Enqueue(r)
	if err != nil || fresh.Registration.RequestKeyHash == first.Registration.RequestKeyHash {
		t.Fatal("hash not scoped to host-local key")
	}
	files, _ := filepath.Glob(filepath.Join(s.Root(), "pending", "*.json"))
	for _, path := range files {
		raw, _ := os.ReadFile(path)
		if strings.Contains(string(raw), r.RequestKey) {
			t.Fatal("raw key persisted")
		}
	}
}

func TestSecurityCatalogAndDurableIdentity(t *testing.T) {
	r := BuildStart(testExecution(t, []string{"anx.card.dogfooding"}, model.StateRunning), time.Now())
	r.Model = "reviewed-model"
	catalog := []route.ModelRecord{{Adapter: "claude-code", Model: "reviewed-model"}}
	safe, err := Allowlisted(r, catalog)
	if err != nil || safe.Model != "reviewed-model" {
		t.Fatal("catalog model omitted")
	}
	safe, err = Allowlisted(r, nil)
	if err != nil || safe.Model != "other" {
		t.Fatal("unknown model sent")
	}
	for _, mutate := range []func(*Registration){
		func(r *Registration) { r.OriginHostID = "host-not-real" },
		func(r *Registration) { r.ExternalID = "exec-not-real" },
		func(r *Registration) { r.ExternalID = r.OriginHostID },
	} {
		bad := r
		mutate(&bad)
		if _, err := Allowlisted(bad, catalog); err == nil {
			t.Fatal("invalid word-ID accepted")
		}
	}
	r.DurableID = "agentctl://private-value"
	safe, err = Allowlisted(r, catalog)
	if err != nil || safe.DurableID != "agentctl://"+r.OriginHostID+"/"+r.ExternalID {
		t.Fatal("unvalidated durable URI accepted")
	}
}

func TestSecurityLegacySummaryAndMetadataCannotEgress(t *testing.T) {
	r := testRegistration("exec-a", PhaseTerminal, 1)
	r.SchemaVersion = 1
	r.Labels = []string{"private-label"}
	r.RequestKey = "private-key"
	r.Outcome = &Outcome{Availability: "private-availability", Summary: "private unmarked legacy summary", Failure: &FailureProjection{Code: "secret-failure", Kind: "/Users/private"}}
	safe, err := Allowlisted(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(safe)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "secret") {
		t.Fatalf("legacy data escaped: %s", raw)
	}
}

func TestSecurityShortSummaryByteLimit(t *testing.T) {
	for n := 1; n < len(truncationMarker); n++ {
		if got := RedactSummary(strings.Repeat("ok ", 20), n); len(got.Summary) > n {
			t.Fatalf("summary exceeds %d-byte bound", n)
		}
	}
}

func TestSecurityEvidenceStrictAllowlist(t *testing.T) {
	for _, ref := range []string{
		"https://github.com/o/r/sk-" + strings.Repeat("a", 30),
		"https://github.com/o/r/pull/1/extra", "https://github.com/o/r/pull/1?", "https://github.com/o/r/pull/1#",
		"https://github.com:443/o/r/pull/1", "https://gitlab.com/o/r/pull/1",
		"pr: https://github.com/o/r/issues/1", "https://github.com/o/r/pull/01",
	} {
		if _, ok := SanitizeLink(ref); ok {
			t.Errorf("unsafe evidence accepted: %q", ref)
		}
	}
}

func TestSecurityMetadataNeverSerializedRaw(t *testing.T) {
	execution := testExecution(t, []string{"secret-label", "anx.card.sk-" + strings.Repeat("a", 32), "anx.card.dogfooding"}, model.StateRunning)
	execution.Adapter = "private-harness"
	execution.Delegation = &model.DelegationBinding{RequestKey: "secret-request-key", Resolved: model.DelegationTarget{Model: "secret-model"}}
	entry, err := testSpool(t).Enqueue(BuildStart(execution, time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(entry)
	for _, secret := range []string{"secret-label", "secret-request-key", "secret-model", "private-harness", "sk-" + strings.Repeat("a", 32)} {
		if strings.Contains(string(raw), secret) {
			t.Errorf("metadata escaped: %s", secret)
		}
	}
}

type senderFunc func(context.Context, Registration) error

func (f senderFunc) Send(ctx context.Context, r Registration) error { return f(ctx, r) }

func TestSecurityErrorsHaveNoMessagesOrPaths(t *testing.T) {
	s := testSpool(t)
	if _, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
		t.Fatal(err)
	}
	_, err := s.Flush(context.Background(), senderFunc(func(context.Context, Registration) error { return errors.New("/Users/private/bin: secret-value") }), 0)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := s.ListPending()
	raw, _ := json.Marshal(entries)
	if strings.Contains(string(raw), "private") || strings.Contains(string(raw), "secret-value") {
		t.Fatal("error text persisted")
	}
}

func TestSecurityFlushPreservesConcurrentRefresh(t *testing.T) {
	s := testSpool(t)
	r := testRegistration("exec-a", PhaseTerminal, 0)
	if _, err := s.Enqueue(r); err != nil {
		t.Fatal(err)
	}
	_, err := s.Flush(context.Background(), senderFunc(func(context.Context, Registration) error {
		r.State = StateCompleted
		_, err := s.Enqueue(r)
		return err
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	entries, _ := s.ListPending()
	if len(entries) != 1 || entries[0].Registration.State != StateCompleted {
		t.Fatal("newer unsent payload lost")
	}
}

func TestSecurityDeliveredReplayKeepsSequence(t *testing.T) {
	s := testSpool(t)
	first, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 0))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Flush(context.Background(), &recordingSender{}, 0); err != nil {
		t.Fatal(err)
	}
	next, err := s.Enqueue(testRegistration("exec-b", PhaseStart, 0))
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenSpool(s.Root())
	if err != nil {
		t.Fatal(err)
	}
	replay, err := reopened.Enqueue(testRegistration("exec-a", PhaseTerminal, 0))
	if err != nil {
		t.Fatal(err)
	}
	if replay.Registration.Sequence != first.Registration.Sequence || replay.Registration.Sequence >= next.Registration.Sequence {
		t.Fatal("delivered replay received newer sequence")
	}
}

func TestSecurityConcurrentSequencesUnique(t *testing.T) {
	s := testSpool(t)
	var wg sync.WaitGroup
	seqs := make(chan int, 24)
	for i := range 24 {
		wg.Go(func() {
			other, err := OpenSpool(s.Root())
			if err != nil {
				t.Error(err)
				return
			}
			e, err := other.Enqueue(testRegistration(fmt.Sprintf("exec-%d", i), PhaseStart, 0))
			if err != nil {
				t.Error(err)
				return
			}
			seqs <- e.Registration.Sequence
		})
	}
	wg.Wait()
	close(seqs)
	seen := map[int]bool{}
	for seq := range seqs {
		if seen[seq] {
			t.Errorf("duplicate sequence %d", seq)
		}
		seen[seq] = true
	}
}

func TestSecuritySpoolProcessHelper(t *testing.T) {
	root := os.Getenv("ANX_TEST_SPOOL")
	if root == "" {
		t.Skip("child process only")
	}
	s, err := OpenSpool(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Enqueue(testRegistration(os.Getenv("ANX_TEST_EXEC"), PhaseStart, 0)); err != nil {
		t.Fatal(err)
	}
}

func TestSecurityCrossProcessSequenceLock(t *testing.T) {
	s := testSpool(t)
	var commands []*exec.Cmd
	for i := range 12 {
		cmd := exec.Command(os.Args[0], "-test.run=^TestSecuritySpoolProcessHelper$")
		cmd.Env = append(os.Environ(), "ANX_TEST_SPOOL="+s.Root(), fmt.Sprintf("ANX_TEST_EXEC=exec-%d", i))
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		commands = append(commands, cmd)
	}
	for _, cmd := range commands {
		if err := cmd.Wait(); err != nil {
			t.Error(err)
		}
	}
	entries, err := s.ListPending()
	if err != nil || len(entries) != 12 {
		t.Fatalf("entries=%d err=%v", len(entries), err)
	}
	seen := map[int]bool{}
	for _, entry := range entries {
		if seen[entry.Registration.Sequence] {
			t.Fatal("duplicate cross-process sequence")
		}
		seen[entry.Registration.Sequence] = true
	}
}

func TestSecurityConcurrentFlushIsSerialized(t *testing.T) {
	s := testSpool(t)
	if _, err := s.Enqueue(testRegistration("exec-a", PhaseStart, 0)); err != nil {
		t.Fatal(err)
	}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		_, err := s.Flush(context.Background(), senderFunc(func(context.Context, Registration) error { close(started); <-release; return nil }), 0)
		finished <- err
	}()
	<-started
	other, err := OpenSpool(s.Root())
	if err != nil {
		close(release)
		t.Fatal(err)
	}
	result, err := other.Flush(context.Background(), senderFunc(func(context.Context, Registration) error { t.Error("concurrent delivery"); return nil }), 0)
	close(release)
	if firstErr := <-finished; firstErr != nil {
		t.Fatal(firstErr)
	}
	if err != nil || result.Delivered != 0 {
		t.Fatalf("second flush: %+v %v", result, err)
	}
}

func TestSecurityAutomaticDeadLetterRetention(t *testing.T) {
	for _, action := range []string{"enqueue", "flush"} {
		t.Run(action, func(t *testing.T) {
			s := testSpool(t)
			now := time.Now()
			s.now = func() time.Time { return now.Add(-2 * DefaultRetention) }
			if _, err := s.Enqueue(testRegistration("exec-a", PhaseTerminal, 0)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Flush(context.Background(), &recordingSender{failures: 1, fatalOnce: true}, 0); err != nil {
				t.Fatal(err)
			}
			s.now = func() time.Time { return now }
			if action == "enqueue" {
				_, _ = s.Enqueue(testRegistration("exec-b", PhaseStart, 0))
			} else {
				_, _ = s.Flush(context.Background(), &recordingSender{}, 0)
			}
			dead, _ := s.ListDead()
			if len(dead) != 0 {
				t.Fatal("expired dead letters retained automatically")
			}
		})
	}
}

func TestSecurityNonzeroCLIEnvelopeClassification(t *testing.T) {
	for code, reason := range map[string]string{"not_found": ReasonCardUnresolved, "auth_required": ReasonAuthRequired, "conflict": ReasonConflict} {
		t.Run(code, func(t *testing.T) {
			executable := filepath.Join(t.TempDir(), "anx")
			body := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":false,\"error\":{\"code\":\"" + code + "\",\"message\":\"secret-value /Users/private\"}}'\nexit 3\n"
			if err := os.WriteFile(executable, []byte(body), 0700); err != nil {
				t.Fatal(err)
			}
			err := NewCLISender(CLIOptions{Executable: executable}).Send(context.Background(), testRegistration("exec-a", PhaseTerminal, 1))
			var sendErr *SendError
			if !errors.As(err, &sendErr) || !sendErr.Fatal || classifyDeadReason(err) != reason {
				t.Fatalf("classification lost: %v", err)
			}
			if strings.Contains(err.Error(), "secret-value") || strings.Contains(err.Error(), "private") {
				t.Fatal("unsafe error detail")
			}
		})
	}
}

func TestSecurityGenericProcessFingerprintMatchesIdentity(t *testing.T) {
	execution := testExecution(t, nil, model.StateRunning)
	execution.Adapter = "generic-process"
	got := BuildStart(execution, time.Now()).NativeSession
	want := adapter.Fingerprint(IdentityV1Domain, NativeSessionScope, "generic-process", *execution.SourceBindings[0].OpaqueID)
	if got == nil || got.Fingerprint != want {
		t.Fatal("generic-process fingerprint differs from identity v1")
	}
}
