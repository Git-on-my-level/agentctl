package adapter

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// activitySource is an adapter-declared secondary signal of agent activity for
// native CLIs whose stdout is silent until exit (for example `zcode --json`,
// which prints one document at the end). It is bound to one launched process:
// the source must read only files that process alone writes, never a shared
// location that a concurrent session could also touch. Progress derived from it
// means observed agent activity (model, tool, turn events), not process
// existence.
type activitySource interface {
	// Poll returns the cumulative count of activity events seen so far. It must
	// be safe to call repeatedly and must not block on the child.
	Poll() int
	// Close releases the source after the child has exited.
	Close(exitCode int)
}

var (
	// activityPollInterval is how often a source is read.
	activityPollInterval = 2 * time.Second
	// activityEmitInterval bounds how many progress events a busy source adds
	// to the journal: at most one per interval.
	activityEmitInterval = 15 * time.Second
)

const activitySourceState = "activity.observed"

// IsActivityObservation reports whether a native event's source state comes
// from an activity source. Each such event is new agent activity (it carries
// a growing count), so it advances progress even though its source state
// repeats; other repeated same-state events are treated as heartbeats.
func IsActivityObservation(sourceState string) bool {
	return strings.HasSuffix(sourceState, "."+activitySourceState)
}

// watchActivity turns growth of an activity source into bounded progress
// observations until the process record finishes. Every emitted event carries
// the cumulative count, so successive observations are distinct journal events
// (and are not coalesced as repeated metadata), while a silent source emits
// nothing.
func (a *NativeAdapter) watchActivity(record *processRecord, source activitySource, family string, pollEvery, emitEvery time.Duration) {
	defer func() {
		record.mu.Lock()
		code := -1
		if record.exitCode != nil {
			code = *record.exitCode
		}
		record.mu.Unlock()
		source.Close(code)
	}()
	ticker := time.NewTicker(pollEvery)
	defer ticker.Stop()
	reported, lastEmit := 0, time.Time{}
	for {
		select {
		case <-record.done:
			return
		case <-ticker.C:
		}
		count := source.Poll()
		if count <= reported || (!lastEmit.IsZero() && time.Since(lastEmit) < emitEvery) {
			continue
		}
		reported, lastEmit = count, time.Now()
		record.ingestActivity(parsedObservation{
			Kind: "progress", State: StateRunning, Liveness: LivenessAlive, SourceState: family + "." + activitySourceState,
			Data: map[string]any{"family": family, "activity_source": "session_log", "activity_events": count},
		})
	}
}

// ingestActivity records an observation only while the child is still running,
// so a late poll can never append running-state evidence after a terminal.
func (p *processRecord) ingestActivity(obs parsedObservation) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exitCode != nil || p.result != nil {
		return
	}
	p.ingestObservationLocked(obs)
}

// zcodeActivityEvents are ZCode log events that show the agent doing work. Pure
// housekeeping events (startup, MCP connect, memory samples, routing snapshots)
// are excluded so an idle or wedged session cannot look busy.
var zcodeActivityEvents = map[string]bool{
	"model.request.started":      true,
	"model.request.completed":    true,
	"model.sdk.stream.completed": true,
	"model.response.diagnostics": true,
	"tool.call.started":          true,
	"tool.call.completed":        true,
	"turn.started":               true,
	"turn.completed":             true,
	"turn.phase.started":         true,
	"turn.phase.completed":       true,
}

// zcodeActivityDirs records the log directories this process created for
// ZCode activity, so read-only isolation can tell them from caller input.
var zcodeActivityDirs sync.Map

// zcodeLogDirEnv names the directory ZCode writes its daily JSONL log to.
const zcodeLogDirEnv = "ZCODE_LOG_DIR"

// zcodeLogActivity tails a log directory that belongs to exactly one ZCode
// process. ZCode shares ~/.zcode/cli/log between every session on the host, so
// the adapter points this run at its own private directory instead of guessing
// a session file, which could be another session's.
type zcodeLogActivity struct {
	dir     string
	offsets map[string]int64
	partial map[string][]byte
	count   int
}

// newZCodeActivity creates the private log directory for one launch and returns
// the environment entry that binds ZCode to it. When the caller already chose a
// log directory, the adapter cannot prove it is private to this run, so it
// declines rather than watch a shared location.
func newZCodeActivity(env []string) (activitySource, []string, error) {
	for _, entry := range env {
		if key, _, _ := strings.Cut(entry, "="); key == zcodeLogDirEnv {
			return nil, nil, nil
		}
	}
	dir, err := os.MkdirTemp("", "agentctl-zcode-log-")
	if err != nil {
		return nil, nil, err
	}
	zcodeActivityDirs.Store(filepath.Clean(dir), struct{}{})
	return &zcodeLogActivity{dir: dir, offsets: map[string]int64{}, partial: map[string][]byte{}}, []string{zcodeLogDirEnv + "=" + dir}, nil
}

func (z *zcodeLogActivity) Poll() int {
	entries, err := os.ReadDir(z.dir)
	if err != nil {
		return z.count
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".jsonl") {
			continue
		}
		z.read(filepath.Join(z.dir, entry.Name()))
	}
	return z.count
}

func (z *zcodeLogActivity) read(path string) {
	file, err := os.Open(path)
	if err != nil {
		return
	}
	defer file.Close()
	if _, err := file.Seek(z.offsets[path], io.SeekStart); err != nil {
		return
	}
	data, err := io.ReadAll(io.LimitReader(file, 4<<20))
	if err != nil || len(data) == 0 {
		return
	}
	z.offsets[path] += int64(len(data))
	data = append(z.partial[path], data...)
	z.partial[path] = nil
	for {
		idx := bytes.IndexByte(data, '\n')
		if idx < 0 {
			break
		}
		line := data[:idx]
		data = data[idx+1:]
		var record struct {
			Event string `json:"event"`
		}
		if json.Unmarshal(line, &record) == nil && zcodeActivityEvents[record.Event] {
			z.count++
		}
	}
	if len(data) > 0 && len(data) < 1<<20 {
		z.partial[path] = append([]byte(nil), data...)
	}
}

// Close removes the private log directory after a clean exit and keeps it for
// diagnosis after a failure.
func (z *zcodeLogActivity) Close(exitCode int) {
	if exitCode == 0 {
		_ = os.RemoveAll(z.dir)
	}
}
