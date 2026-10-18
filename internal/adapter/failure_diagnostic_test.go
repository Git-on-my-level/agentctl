package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"os"
	"strings"
	"testing"
	"time"
)

func TestFailedProcessCapturesFirstNonemptyStderrMetadata(t *testing.T) {
	for _, parser := range []outputParser{codexParser{}, cursorParser{}, genericParser{}} {
		record := &processRecord{parser: parser, done: make(chan struct{})}
		line := []byte("fatal: unable to process private prompt and opaque-token-Xb7sQ9")
		record.ingest([]byte("  "), true)
		record.ingest(line, true)
		record.ingest([]byte("Usage: native-command"), true)
		record.finish(errors.New("exit status 1"))
		got, _ := record.result.Data["stderr_diagnostic"].(*diagnostic.Stderr)
		want := diagnostic.FromStderrLine(line)
		if got == nil || *got != *want || record.result.Error != "native execution failed" {
			t.Fatalf("metadata/result=%#v %#v", got, record.result)
		}
		encoded, _ := json.Marshal(record.result)
		for _, secret := range []string{"private prompt", "opaque-token-Xb7sQ9", "Usage:", "stderr_message"} {
			if strings.Contains(string(encoded), secret) {
				t.Fatal("stderr escaped adapter metadata boundary")
			}
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
