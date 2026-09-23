package adapter

import "testing"

func TestRewriteZCodeArgvKeepsExplicitPath(t *testing.T) {
	argv := []string{"/opt/zcode/bin/zcode", "--json", "--prompt", "hi"}
	got := rewriteZCodeArgv(argv)
	if len(got) != len(argv) || got[0] != argv[0] {
		t.Fatalf("explicit path rewritten: %#v", got)
	}
}

func TestZCodeParserReadsResponseDocument(t *testing.T) {
	raw := []byte(`{
  "sessionId": "sess_example",
  "response": "Hi!",
  "projection": {"status": "idle"}
}`)
	obs := zcodeParser{}.Parse(raw, false)
	if !obs.Terminal || !obs.Success || obs.Content != "Hi!" || obs.SessionID != "sess_example" {
		t.Fatalf("obs = %#v", obs)
	}
	if obs.ContentSource != "assistant_terminal_result" {
		t.Fatalf("source = %q", obs.ContentSource)
	}
}

func TestZCodeParserRejectsEmptyResponse(t *testing.T) {
	obs := zcodeParser{}.Parse([]byte(`{"sessionId":"sess_example","response":""}`), false)
	if obs.Success || obs.Data["diagnostic_code"] != "empty_terminal_result" {
		t.Fatalf("obs = %#v", obs)
	}
}
