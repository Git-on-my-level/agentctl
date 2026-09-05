package fanout_test

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/fanout"
)

// These are real local subprocesses using the existing Codex/Claude parsers,
// not live authenticated provider sessions. No harness interface is replaced.
func TestRunNativeParserFixturesWithExactArgvAndStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX process fixture")
	}
	root := t.TempDir()
	header := "#!/bin/sh\n[ \"$1\" = '--native-flag' ] || exit 9\n"
	codex := header + "printf '%s\\n' '{\"type\":\"thread.started\",\"thread_id\":\"fixture-thread\"}'\nprintf '{\"type\":\"item.completed\",\"item\":{\"id\":\"one\",\"type\":\"agent_message\",\"text\":\"%s\"}}\\n' \"$2\"\nprintf '%s\\n' '{\"type\":\"turn.completed\"}'\n"
	claude := header + "prompt=$(cat)\nprintf '{\"type\":\"result\",\"subtype\":\"success\",\"is_error\":false,\"result\":\"%s\"}\\n' \"$prompt\"\n"
	paths := []string{filepath.Join(root, "codex-fixture"), filepath.Join(root, "claude-fixture")}
	for i, script := range []string{codex, claude} {
		if err := os.WriteFile(paths[i], []byte(script), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	want := []string{"literal $HOME; echo not-a-command", "distinct stdin task"}
	requests := []adapter.LaunchRequest{
		{Argv: []string{paths[0], "--native-flag", want[0]}},
		{Argv: []string{paths[1], "--native-flag"}, Stdin: []byte(want[1])},
	}
	adapters := []adapter.Adapter{adapter.NewCodex(), adapter.NewClaudeCode()}
	results := make([]*adapter.Result, 2)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	attempted, err := fanout.Run(ctx, 2, 2, false, func(ctx context.Context, i int) bool {
		launch, err := adapters[i].Launch(ctx, requests[i])
		if err != nil {
			t.Errorf("child %d: %v", i, err)
			return false
		}
		results[i] = launch.Result
		return launch.Result != nil && launch.Result.Success
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, result := range results {
		if !attempted[i] || result == nil || result.State != adapter.StateCompleted || result.Content != want[i] {
			t.Fatalf("child %d: attempted=%v result=%#v", i, attempted[i], result)
		}
	}
}
