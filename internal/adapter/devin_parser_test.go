package adapter

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRewriteDevinArgvKeepsExplicitPath(t *testing.T) {
	argv := []string{"/opt/devin/bin/devin", "--model", "swe-2-high", "-p", "hi"}
	got := rewriteDevinArgv(argv)
	if len(got) != len(argv) || got[0] != argv[0] {
		t.Fatalf("explicit path rewritten: %#v", got)
	}
}

func TestDevinParserReadsPrintFixtures(t *testing.T) {
	cases := []struct {
		name    string
		fixture string
		want    string
	}{
		{name: "swe-2-high", fixture: "devin-swe-2-high-print.txt", want: "Hi"},
		{name: "fusion", fixture: "devin-fusion-print.txt", want: "Hi."},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join("testdata", tc.fixture))
			if err != nil {
				t.Fatal(err)
			}
			if len(raw) == 0 || raw[0] == '{' || raw[0] == '[' {
				t.Fatalf("print fixture is not the observed plain-text shape: %q", raw[:1])
			}
			obs := devinParser{}.Parse(raw, false)
			if obs.Terminal || obs.Success || obs.Content != tc.want {
				t.Fatalf("obs = %#v", obs)
			}
			if obs.ContentSource != "assistant_terminal_result" {
				t.Fatalf("source = %q", obs.ContentSource)
			}
			if strings.Contains(obs.Content, "@") || strings.Contains(obs.Content, "Welcome") {
				t.Fatalf("banner leaked into content: %q", obs.Content)
			}
		})
	}
}

func TestDevinParserRejectsEmptyPrint(t *testing.T) {
	obs := devinParser{}.Parse([]byte(" \n"), false)
	if obs.Success || obs.Terminal || obs.Data["diagnostic_code"] != "empty_terminal_result" {
		t.Fatalf("obs = %#v", obs)
	}
}

func TestDevinParserDropsWelcomeBanner(t *testing.T) {
	raw := []byte("\x1b[1mWelcome to Devin CLI!\x1b[0m\n\nLogged in as user@example.com\n")
	obs := devinParser{}.Parse(raw, false)
	if obs.Success || obs.Terminal || obs.Content != "" || obs.Data["diagnostic_code"] != "empty_terminal_result" {
		t.Fatalf("obs = %#v", obs)
	}
}

func TestDevinPrintSuccessFollowsCleanExit(t *testing.T) {
	path := fixtureExecutable(t, `printf 'Hi\n'`)
	a := NewDevin()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{path, "-p"}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted || got.Result.Content != "Hi" {
		t.Fatalf("result = %#v", got.Result)
	}
}

func TestDevinPrintFailureAndCancelAreNotSuccess(t *testing.T) {
	failed := fixtureExecutable(t, `printf 'Hi\n'; exit 1`)
	a := NewDevin()
	got, err := a.Launch(context.Background(), LaunchRequest{Argv: []string{failed, "-p"}, DiscoveryWindow: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateFailed {
		t.Fatalf("nonzero exit result = %#v", got.Result)
	}

	slow := fixtureExecutable(t, `printf 'Hi\n'; sleep 30`)
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	got, err = a.Launch(ctx, LaunchRequest{Argv: []string{slow, "-p"}, DiscoveryWindow: 50 * time.Millisecond})
	if err == nil {
		t.Fatal("expected cancellation")
	}
	if got.Result == nil || got.Result.Success || got.Result.State != StateCancelled {
		t.Fatalf("cancelled result = %#v err=%v", got.Result, err)
	}
}

func TestDevinParserIgnoresStderr(t *testing.T) {
	obs := devinParser{}.Parse([]byte("noise\n"), true)
	if obs.Terminal || obs.Content != "" || obs.SourceState != "stderr" {
		t.Fatalf("obs = %#v", obs)
	}
}
