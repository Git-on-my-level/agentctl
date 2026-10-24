package diagnostic

import "testing"

func TestCodexGitTrustDiagnostic(t *testing.T) {
	line := []byte("Not inside a trusted directory and --skip-git-repo-check was not specified.")
	d := FromStderrLine(line, "codex")
	if d.Code != "codex_git_repo_check_required" || d.Validate() != nil {
		t.Fatalf("%#v", d)
	}
	for _, harness := range []string{"codex", "generic-process"} {
		for _, text := range []string{"prefix " + string(line), string(line) + " suffix", "Not inside a trusted directory"} {
			if got := FromStderrLine([]byte(text), harness); got.Code == d.Code {
				t.Fatalf("overclassified %s", text)
			}
		}
	}
	if got := FromStderrLine(line, "generic-process"); got.Code == d.Code {
		t.Fatal("wrong harness")
	}
}
