package adapter

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/diagnostic"
	"github.com/Git-on-my-level/agentctl/internal/ids"
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
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	id, _ := ids.FromPayload(ids.TypeExecution, 1)
	record := &processRecord{parser: genericParser{}, done: make(chan struct{}), outputDir: dir, executionID: id.String()}
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

func TestZCodeCapacityNeedsProvider1308OrHTTP429(t *testing.T) {
	for _, tt := range []struct{ line, native, category string }{
		{"ProviderBusinessError providerCode: 429 provider request failed", "429", ""},
		{"ProviderBusinessError: [429] provider request failed", "429", ""},
		{"ProviderBusinessError providerCode: 7314 provider request failed", "7314", ""},
		{"ProviderBusinessError providerCode: 7314 responseStatus: 429 provider request failed", "7314", "capacity"},
		{"ProviderBusinessError providerCode: 1308 provider request failed", "1308", "capacity"},
		{"ProviderBusinessError providerCode: 1308 responseStatus: 429 authentication required", "1308", ""},
		{"ProviderBusinessError providerCode: 429 authentication required", "429", ""},
	} {
		t.Run(tt.line, func(t *testing.T) {
			parsed := parseZCodeFailure(tt.line)
			if parsed.NativeCode != tt.native || parsed.Category != tt.category {
				t.Fatalf("parsed=%#v", parsed)
			}
			record := &processRecord{parser: zcodeParser{}, done: make(chan struct{})}
			record.ingest([]byte(tt.line), true)
			record.finish(errors.New("exit status 1"))
			if record.result.Data["native_code"] != tt.native {
				t.Fatalf("lost provider code: %#v", record.result)
			}
			if tt.category == "capacity" && record.result.Data["failure_category"] != "capacity" {
				t.Fatalf("lost HTTP category %#v", record.result)
			}
			if strings.Contains(tt.line, "authentication required") && !strings.Contains(record.result.Error, "authentication required") {
				t.Fatalf("auth lost: %#v", record.result)
			}
		})
	}
}
