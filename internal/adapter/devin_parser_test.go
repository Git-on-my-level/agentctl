package adapter

import (
	"context"
	"errors"
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

func fakeDevinOnPath(t *testing.T, body string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "devin")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nset -eu\n"+body+"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestDevinResumePassesExplicitSessionIDAndCallerArgv(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "argv")
	fakeDevinOnPath(t, `printf '%s\n' "$@" > "`+capture+`"; printf 'resumed answer\n'`)
	a := NewDevin()
	got, err := a.Resume(context.Background(), ResumeRequest{
		Ref:             SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "midi-yttrium"},
		Argv:            []string{"-p", "what exact token", "--permission-mode", "auto"},
		DiscoveryWindow: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "-r\nmidi-yttrium\n-p\nwhat exact token\n--permission-mode\nauto\n"
	if string(raw) != want {
		t.Fatalf("native argv = %q, want %q", raw, want)
	}
	if got.Result == nil || !got.Result.Success || got.Result.State != StateCompleted || got.Result.Content != "resumed answer" {
		t.Fatalf("result = %#v", got.Result)
	}
	if got.Session.Ref.PID == 0 || got.Session.Ref.OpaqueID == "midi-yttrium" {
		t.Fatalf("resume must report a new process-bound ref, not the native session ID: %#v", got.Session.Ref)
	}
}

func TestDevinResumeAcceptsPrintAndPositionalPromptForms(t *testing.T) {
	root := t.TempDir()
	capture := filepath.Join(root, "argv")
	fakeDevinOnPath(t, `printf '%s\n' "$@" > "`+capture+`"; printf 'ok\n'`)
	a := NewDevin()
	ref := SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "midi-yttrium"}
	for _, argv := range [][]string{
		{"--print", "follow-up"},
		{"-p", "--", "follow-up", "words"},
	} {
		if _, err := a.Resume(context.Background(), ResumeRequest{Ref: ref, Argv: argv, DiscoveryWindow: time.Second}); err != nil {
			t.Fatalf("argv=%v rejected: %v", argv, err)
		}
	}
	raw, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	want := "-r\nmidi-yttrium\n-p\n--\nfollow-up\nwords\n"
	if string(raw) != want {
		t.Fatalf("last native argv = %q, want %q", raw, want)
	}
}

func TestDevinResumeRejectsUnsafeRequestsWithoutLaunching(t *testing.T) {
	root := t.TempDir()
	marker := filepath.Join(root, "called")
	fakeDevinOnPath(t, `touch "`+marker+`"; printf 'should not run\n'`)
	a := NewDevin()
	ref := SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "midi-yttrium"}
	argv := []string{"-p", "follow-up"}
	for _, test := range []struct {
		name string
		ref  SourceRef
		argv []string
	}{
		{name: "wrong adapter", ref: SourceRef{Adapter: "codex", Kind: "devin_session", OpaqueID: "midi-yttrium"}, argv: argv},
		{name: "wrong kind", ref: SourceRef{Adapter: "devin", Kind: "codex_thread", OpaqueID: "midi-yttrium"}, argv: argv},
		{name: "pid ref", ref: SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "midi-yttrium", PID: 4321}, argv: argv},
		{name: "numeric opaque id", ref: SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "4321"}, argv: argv},
		{name: "empty opaque id", ref: SourceRef{Adapter: "devin", Kind: "devin_session"}, argv: argv},
		{name: "whitespace opaque id", ref: SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "bad id"}, argv: argv},
		{name: "leading dash opaque id", ref: SourceRef{Adapter: "devin", Kind: "devin_session", OpaqueID: "-x"}, argv: argv},
		{name: "missing print flag", ref: ref, argv: []string{"--permission-mode", "auto"}},
		{name: "missing prompt", ref: ref, argv: []string{"-p", "--permission-mode", "auto"}},
		{name: "empty prompt", ref: ref, argv: []string{"-p", "  "}},
		{name: "positional prompt only", ref: ref, argv: []string{"--", "follow-up"}},
		{name: "whitespace positional prompt", ref: ref, argv: []string{"-p", "--", "   "}},
		{name: "conflicting -r", ref: ref, argv: []string{"-r", "other-session", "-p", "x"}},
		{name: "conflicting --resume", ref: ref, argv: []string{"-p", "x", "--resume"}},
		{name: "conflicting -c", ref: ref, argv: []string{"-c", "-p", "x"}},
		{name: "conflicting --continue=", ref: ref, argv: []string{"--continue=other", "-p", "x"}},
		{name: "conflicting -r=", ref: ref, argv: []string{"-r=other", "-p", "x"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := a.Resume(context.Background(), ResumeRequest{Ref: test.ref, Argv: test.argv, DiscoveryWindow: time.Second})
			var adapterErr *AdapterError
			if err == nil || !errors.As(err, &adapterErr) || adapterErr.Code != ErrUsage {
				t.Fatalf("resume error = %v, want usage rejection", err)
			}
		})
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("invalid resume launched the native executable")
	}
}

func TestDevinManifestDeclaresExplicitSessionResume(t *testing.T) {
	manifest := devinManifest()
	var decl *CapabilityDeclaration
	for i := range manifest.Capabilities {
		if manifest.Capabilities[i].Name == CapabilityResume {
			decl = &manifest.Capabilities[i]
		}
	}
	if decl == nil || decl.Implementation != CapabilitySupported {
		t.Fatalf("devin resume declaration = %#v", decl)
	}
	if decl.Constraints["scope"] != "explicit_native_session_id" || decl.Constraints["cross_restart"] != true || decl.Constraints["requires_session_id"] != true || decl.Constraints["session_id_source"] != "caller_supplied" {
		t.Fatalf("devin resume constraints = %#v", decl.Constraints)
	}
	if decl.Constraints["required_output_mode"] != "print" {
		t.Fatalf("devin resume required_output_mode = %#v", decl.Constraints["required_output_mode"])
	}
	flags, _ := decl.Constraints["accepted_print_flags"].([]string)
	if len(flags) != 2 || flags[0] != "-p" || flags[1] != "--print" {
		t.Fatalf("devin resume accepted_print_flags = %#v", decl.Constraints["accepted_print_flags"])
	}
	probed := NegotiateInvocation(manifest, []string{"devin", "-r", "midi-yttrium", "-p", "follow-up"}, CapabilityResume)
	if probed.Status != CapabilitySupported {
		t.Fatalf("negotiated resume = %#v", probed)
	}
}
