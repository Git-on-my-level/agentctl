package adapter

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
			if !obs.Terminal || !obs.Success || obs.Content != tc.want {
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
	if obs.Success || obs.Data["diagnostic_code"] != "empty_terminal_result" {
		t.Fatalf("obs = %#v", obs)
	}
}

func TestDevinParserIgnoresStderr(t *testing.T) {
	obs := devinParser{}.Parse([]byte("noise\n"), true)
	if obs.Terminal || obs.Content != "" || obs.SourceState != "stderr" {
		t.Fatalf("obs = %#v", obs)
	}
}
