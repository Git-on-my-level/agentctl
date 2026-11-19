package skillpack

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestUpdateAttemptCarriesFirstFailureAndBoundsMessage(t *testing.T) {
	home := t.TempDir()
	if attempt, err := ReadUpdateAttempt(home); err != nil || attempt != nil {
		t.Fatalf("missing record = %+v, %v", attempt, err)
	}
	first := time.Date(2026, 11, 18, 5, 0, 0, 0, time.UTC)
	if err := RecordUpdateAttempt(home, UpdateAttempt{AttemptedAt: first, Trigger: "auto", Error: strings.Repeat("é", 400)}); err != nil {
		t.Fatal(err)
	}
	if err := RecordUpdateAttempt(home, UpdateAttempt{AttemptedAt: first.Add(time.Hour), Trigger: "auto", Error: "again"}); err != nil {
		t.Fatal(err)
	}
	attempt, err := ReadUpdateAttempt(home)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.ConsecutiveFailures != 2 || attempt.FirstFailedAt == nil || !attempt.FirstFailedAt.Equal(first) || attempt.Error != "again" {
		t.Fatalf("unexpected failure record: %+v", attempt)
	}
	if bounded := boundAttemptError(strings.Repeat("é", 400)); len(bounded) > maxAttemptErrorBytes+3 || !strings.HasSuffix(bounded, "...") || !strings.HasPrefix(bounded, "é") {
		t.Fatalf("message not bounded on a rune boundary: %d", len(bounded))
	}
	if err := RecordUpdateAttempt(home, UpdateAttempt{AttemptedAt: first.Add(2 * time.Hour), Trigger: "explicit", Succeeded: true, Healthy: true}); err != nil {
		t.Fatal(err)
	}
	if attempt, err = ReadUpdateAttempt(home); err != nil || !attempt.Succeeded || attempt.FirstFailedAt != nil || attempt.ConsecutiveFailures != 0 {
		t.Fatalf("success did not clear failure: %+v %v", attempt, err)
	}
	path, err := UpdateAttemptPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(path) != "skill-update-last.json" {
		t.Fatalf("path = %s", path)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("record mode = %v %v", info, err)
	}
}
