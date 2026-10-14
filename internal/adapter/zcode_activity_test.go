package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real-shaped ZCode daily-log lines with ids, prompts, and paths scrubbed.
const (
	zcodeLogModelStarted    = `{"timestamp":"2026-10-14T16:03:50.100Z","level":"info","event":"model.request.started","module":"adapters.model","message":"Model request started","sessionId":"sess_x","context":{"modelId":"m","querySource":"main_turn"}}`
	zcodeLogStreamCompleted = `{"timestamp":"2026-10-14T16:03:50.200Z","level":"info","event":"model.sdk.stream.completed","module":"adapters.model","message":"done","sessionId":"sess_x"}`
	zcodeLogToolStarted     = `{"timestamp":"2026-10-14T16:03:51.000Z","level":"info","event":"tool.call.started","module":"core.runtime","message":"Tool call started","sessionId":"sess_x"}`
	zcodeLogStartup         = `{"timestamp":"2026-10-14T16:03:49.000Z","level":"info","event":"bootstrap.app.startup.started","module":"bootstrap","message":"startup","sessionId":"sess_x"}`
	zcodeLogMemorySample    = `{"timestamp":"2026-10-14T16:03:52.000Z","level":"info","event":"zcode_protocol.process.memory_sample","module":"m","message":"sample"}`
)

func fastActivityIntervals(t *testing.T) {
	t.Helper()
	poll, emit := activityPollInterval, activityEmitInterval
	activityPollInterval, activityEmitInterval = 15*time.Millisecond, 40*time.Millisecond
	t.Cleanup(func() { activityPollInterval, activityEmitInterval = poll, emit })
}

func appendLines(t *testing.T, path string, lines ...string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, line := range lines {
		if _, err := f.WriteString(line + "\n"); err != nil {
			t.Fatal(err)
		}
	}
}

func TestZCodeLogActivityCountsWorkNotHousekeeping(t *testing.T) {
	source, env, err := newZCodeActivity(nil)
	if err != nil || source == nil || len(env) != 1 || !strings.HasPrefix(env[0], zcodeLogDirEnv+"=") {
		t.Fatalf("source=%v env=%v err=%v", source, env, err)
	}
	dir := strings.TrimPrefix(env[0], zcodeLogDirEnv+"=")
	defer source.Close(0)
	log := filepath.Join(dir, "zcode-2026-10-14.jsonl")
	if source.Poll() != 0 {
		t.Fatal("empty directory reported activity")
	}
	appendLines(t, log, zcodeLogStartup, zcodeLogMemorySample)
	if got := source.Poll(); got != 0 {
		t.Fatalf("housekeeping counted as activity: %d", got)
	}
	appendLines(t, log, zcodeLogModelStarted, zcodeLogStreamCompleted, zcodeLogToolStarted)
	if got := source.Poll(); got != 3 {
		t.Fatalf("activity = %d, want 3", got)
	}
	// A line split across polls is counted once, when it completes.
	f, _ := os.OpenFile(log, os.O_APPEND|os.O_WRONLY, 0o600)
	half := len(zcodeLogToolStarted) / 2
	f.WriteString(zcodeLogToolStarted[:half])
	if got := source.Poll(); got != 3 {
		t.Fatalf("partial line counted: %d", got)
	}
	f.WriteString(zcodeLogToolStarted[half:] + "\n")
	f.Close()
	if got := source.Poll(); got != 4 {
		t.Fatalf("completed line activity = %d, want 4", got)
	}
	// Malformed lines and non-jsonl files are ignored.
	appendLines(t, log, "not json", `{"event":`)
	appendLines(t, filepath.Join(dir, "other.log"), zcodeLogToolStarted)
	if got := source.Poll(); got != 4 {
		t.Fatalf("malformed/foreign counted: %d", got)
	}
}

func TestZCodeActivityDeclinesCallerChosenLogDir(t *testing.T) {
	source, env, err := newZCodeActivity([]string{"PATH=/bin", zcodeLogDirEnv + "=/shared/logs"})
	if err != nil || source != nil || env != nil {
		t.Fatalf("caller-chosen shared log dir must not be watched: %v %v %v", source, env, err)
	}
}

func TestZCodeLogActivityRemovedAfterCleanExitKeptAfterFailure(t *testing.T) {
	for _, test := range []struct {
		code int
		kept bool
	}{{0, false}, {2, true}} {
		source, env, _ := newZCodeActivity(nil)
		dir := strings.TrimPrefix(env[0], zcodeLogDirEnv+"=")
		source.Close(test.code)
		_, err := os.Stat(dir)
		if (err == nil) != test.kept {
			t.Fatalf("code %d: stat err=%v kept=%v", test.code, err, test.kept)
		}
		os.RemoveAll(dir)
	}
}

const zcodeFakeResponse = `printf '%s\n' '{"sessionId":"sess_fixture","response":"ok","projection":{"status":"idle"}}'`

func newTestZCode(t *testing.T) *NativeAdapter {
	t.Helper()
	return newNativeAdapter(nativeConfig{
		Manifest: zcodeManifest(), Binary: "zcode", Parser: zcodeParser{},
		LaunchKind: "zcode_session", WholeStdout: true, NewActivitySource: newZCodeActivity,
	})
}

func activityEvents(t *testing.T, a *NativeAdapter, ref SourceRef) []Event {
	t.Helper()
	events, err := a.Events(context.Background(), EventsRequest{Ref: ref})
	if err != nil {
		t.Fatal(err)
	}
	var out []Event
	for _, event := range events {
		if event.SourceState == "zcode."+activitySourceState {
			out = append(out, event)
		}
	}
	return out
}

func TestZCodeProgressAdvancesDuringSilentStdoutStream(t *testing.T) {
	fastActivityIntervals(t)
	// Stdout is silent until exit; only the private log shows work.
	script := `log="$ZCODE_LOG_DIR/zcode-2026-10-14.jsonl"
echo '` + zcodeLogStartup + `' >> "$log"
for i in 1 2 3 4 5 6; do
  echo '` + zcodeLogModelStarted + `' >> "$log"
  echo '` + zcodeLogStreamCompleted + `' >> "$log"
  sleep 0.1
done
` + zcodeFakeResponse
	a := newTestZCode(t)
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{fixtureExecutable(t, script)}, StartOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	record := a.recordFor(got.Session.Ref)
	select {
	case <-record.done:
	case <-time.After(10 * time.Second):
		t.Fatal("fake zcode did not finish")
	}
	events := activityEvents(t, a, got.Session.Ref)
	if len(events) < 2 {
		t.Fatalf("progress did not advance during the run: %#v", events)
	}
	seen := map[string]bool{}
	var last time.Time
	for _, event := range events {
		if event.Kind != "progress" || event.State != StateRunning || seen[event.DedupeKey] || event.ObservedAt.Before(last) {
			t.Fatalf("events must be distinct, ordered running progress: %#v", events)
		}
		seen[event.DedupeKey] = true
		last = event.ObservedAt
	}
	if events[len(events)-1].Payload["activity_events"].(int) <= events[0].Payload["activity_events"].(int) {
		t.Fatalf("activity count did not grow: %#v", events)
	}
	// No running-state progress is appended after the terminal result.
	time.Sleep(5 * activityPollInterval)
	if after := activityEvents(t, a, got.Session.Ref); len(after) != len(events) {
		t.Fatalf("progress appended after exit: %d -> %d", len(events), len(after))
	}
	record.mu.Lock()
	result := record.result
	record.mu.Unlock()
	if result == nil || !result.Success {
		t.Fatalf("result = %#v", result)
	}
}

func TestZCodeProgressDoesNotAdvanceForSilentProcess(t *testing.T) {
	fastActivityIntervals(t)
	for name, script := range map[string]string{
		"no output at all": "sleep 0.4\n" + zcodeFakeResponse,
		"housekeeping only": `log="$ZCODE_LOG_DIR/zcode-2026-10-14.jsonl"
for i in 1 2 3 4; do
  echo '` + zcodeLogMemorySample + `' >> "$log"
  sleep 0.1
done
` + zcodeFakeResponse,
	} {
		t.Run(name, func(t *testing.T) {
			a := newTestZCode(t)
			got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{fixtureExecutable(t, script)}, StartOnly: true})
			if err != nil {
				t.Fatal(err)
			}
			record := a.recordFor(got.Session.Ref)
			select {
			case <-record.done:
			case <-time.After(10 * time.Second):
				t.Fatal("fake zcode did not finish")
			}
			if events := activityEvents(t, a, got.Session.Ref); len(events) != 0 {
				t.Fatalf("silent process produced progress: %#v", events)
			}
		})
	}
}

func TestZCodeLaunchBindsEachRunToItsOwnLogDir(t *testing.T) {
	fastActivityIntervals(t)
	script := `printf '%s\n' "$ZCODE_LOG_DIR" > "$1"
` + zcodeFakeResponse
	dirs := map[string]bool{}
	a := newTestZCode(t)
	for range 2 {
		out := filepath.Join(t.TempDir(), "dir.txt")
		got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{fixtureExecutable(t, script), out}, StartOnly: true})
		if err != nil {
			t.Fatal(err)
		}
		<-a.recordFor(got.Session.Ref).done
		raw, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		dir := strings.TrimSpace(string(raw))
		if dir == "" || dirs[dir] {
			t.Fatalf("runs must not share a log dir: %q %v", dir, dirs)
		}
		dirs[dir] = true
	}
}
