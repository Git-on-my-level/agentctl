package adapter

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

func TestRedactFailureLine(t *testing.T) {
	raw := "\n\x1b[31mload failed api_key=private-value authorization='Bearer private-bearer' requestId=req-private trace_id=trace-private https://user:password@example.test/?token=private-url {\"prompt\":\"confidential task\",\"token\":\"private-json\"}\x1b[0m\nsecond line"
	got := RedactFailureLine(raw)
	for _, secret := range []string{"private-", "confidential", "second line", "\x1b"} {
		if strings.Contains(got, secret) {
			t.Fatalf("leaked %q in %q", secret, got)
		}
	}
	if !strings.HasPrefix(got, "load failed") || !strings.Contains(got, "REDACTED") || len(got) > 200 {
		t.Fatalf("diagnostic=%q", got)
	}
	for _, raw := range []string{
		"load failed access_token=private-access refresh_token=private-refresh client_secret=private-client cookie=private-cookie",
		"load failed request id: private-request trace-id=private-trace",
		"load failed prompt=confidential task with multiple words",
		`load failed {"access_token":"private-json","prompt":"confidential task"}`,
	} {
		got := RedactFailureLine(raw)
		if strings.Contains(got, "private-") || strings.Contains(got, "confidential") || strings.Contains(got, "multiple words") {
			t.Fatalf("leaked diagnostic: %q", got)
		}
	}
	got = RedactFailureLine(strings.Repeat("界", 200))
	if len(got) > 200 || strings.Contains(got, "�") {
		t.Fatalf("invalid bound: %q", got)
	}
}

func TestFailedProcessCapturesFirstNonemptyStderr(t *testing.T) {
	for _, parser := range []outputParser{codexParser{}, cursorParser{}, genericParser{}} {
		record := &processRecord{parser: parser, done: make(chan struct{})}
		record.ingest([]byte("  "), true)
		record.ingest([]byte("could not load settings token=private-value"), true)
		record.ingest([]byte("Usage: native-command"), true)
		record.finish(errors.New("exit status 1"))
		got, _ := record.result.Data["stderr_message"].(string)
		if !strings.HasPrefix(got, "could not load settings") || strings.Contains(got, "private-value") || strings.Contains(got, "Usage:") {
			t.Fatalf("%s result=%#v", parser.Name(), record.result)
		}
	}
}

func TestGenericExtractionOutputIsBoundedAndPrivate(t *testing.T) {
	dir := t.TempDir()
	record := &processRecord{parser: genericParser{}, done: make(chan struct{}), outputDir: dir}
	record.ingest([]byte(strings.Repeat("x", 100000)), false)
	record.finish(nil)
	path, _ := record.result.Data["output_ref"].(string)
	data, err := os.ReadFile(path)
	if err != nil || len(data) != 64<<10 || record.result.Data["output_truncated"] != true || record.result.State != StateFailed {
		t.Fatalf("result=%#v bytes=%d err=%v", record.result, len(data), err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("stat=%v %v", info, err)
	}
	missing := &processRecord{parser: genericParser{}, done: make(chan struct{}), outputDir: dir + "/missing"}
	missing.finish(nil)
	if missing.result.Data["output_capture_error"] != "output_capture_failed" {
		t.Fatalf("result=%#v", missing.result)
	}
	disabled := &processRecord{parser: genericParser{}, done: make(chan struct{})}
	disabled.ingest([]byte("discard me"), false)
	disabled.finish(nil)
	if disabled.result.Data["output_ref"] != nil || len(disabled.outputPrefix) != 0 {
		t.Fatal("disabled capture retained output")
	}
}

func TestCodexCapacityWithoutNativeCodeSurvivesStderrExit(t *testing.T) {
	path := fixtureExecutable(t, `printf '%s\n' 'Selected model is at capacity. Please try a different model.' >&2
exit 1`)
	got, err := NewCodex().Launch(context.Background(), LaunchRequest{Argv: []string{path}, DiscoveryWindow: time.Second})
	if err != nil || got.Result == nil || !strings.Contains(got.Result.Error, "at capacity") {
		t.Fatalf("result=%#v err=%v", got.Result, err)
	}
}
