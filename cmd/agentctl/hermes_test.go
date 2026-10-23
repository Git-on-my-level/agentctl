package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func TestHermesDelegateContinueAndModelSummary(t *testing.T) {
	f := newDelegateFixture(t, `cat >/dev/null
case " $* " in
*" --resume hermes-fixture "*)
 for arg do
 case "$arg" in --model|--reasoning|--yolo|--max-turns) exit 2;; esac
 done
 answer=resumed;;
*) answer=first;;
esac
printf '%s\n' "{\"type\":\"result\",\"session_id\":\"hermes-fixture\",\"exit_code\":0,\"text\":\"$answer answer\"}"`)
	cfg := config.Config{SchemaVersion: 1, DefaultProfile: "test", Profiles: map[string]config.Profile{"test": {
		Adapters:         map[string]config.Adapter{"hermes": {Executable: filepath.Join(f.root, "cursor-agent")}},
		Delegation:       &config.DelegationPolicy{UnattendedCodingPermissions: true},
		AgentPreferences: &config.AgentPreferences{Mode: "advisory", Preferred: []config.AgentPreference{{Agent: "hermes", Model: "glm-5.3-flash", Effort: "high"}}},
	}}}
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(f.config, data, 0600); err != nil {
		t.Fatal(err)
	}
	f.selectRequest(t, `{"harness":"hermes","model":"glm-5.3-flash"}`)
	code, plan := f.invoke("private query", "--plan")
	if code != 0 || !strings.Contains(plan, `"--query-file","-"`) || !strings.Contains(plan, `"permissions":"constrained"`) || strings.Contains(plan, "private query") {
		t.Fatalf("%d %s", code, plan)
	}
	code, out := f.invoke("private query", "--wait")
	if code != 0 || !strings.Contains(out, "first answer") {
		t.Fatalf("%d %s", code, out)
	}
	first := resultID(t, out)
	code, out = f.command(t, "private followup", "continue", first, "--request-key", "hermes-2", "--prompt-stdin", "--wait")
	if code != 0 || !strings.Contains(out, "resumed answer") {
		t.Fatalf("%d %s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "continue-result.schema.json")
	// Include legacy and Multica rows without a recorded model.
	seedRecentExport(t, f.journal, time.Now().UTC().Add(-time.Hour), 2)
	doc, out, code := readRecentExport(t, f.journal, "--summary", "--limit", "1")
	if code != 0 || doc.Result.Total != 4 || doc.Result.Summary.ByModel["glm-5.3-flash"] != 2 || doc.Result.Summary.ByModel["unknown"] != 2 {
		t.Fatalf("%d %s", code, out)
	}
	assertResultMatchesSchemaShape(t, out, "recent-result.schema.json")
}

func TestNativeApprovalAttentionRepair(t *testing.T) {
	for _, name := range []string{"codex", "cursor", "claude-code", "zcode", "devin", "hermes"} {
		e := model.Execution{Authority: model.AuthorityNative, Adapter: name, State: model.StateAttention}
		source := "approval_required"
		e.SourceState = &source
		warning := attentionAuthorityWarning(e)
		if warning.Details["diagnostic_code"] != "native_approval_required" || warning.Details["repair_action"] == nil {
			t.Fatalf("%#v", warning)
		}
		actions := attentionNextActions(output.JSON, e)
		for _, action := range actions {
			if strings.Contains(strings.Join(action.Argv, " "), "--ignore-attention") {
				t.Fatalf("pointless wait: %#v", action)
			}
		}
		e.Authority = model.AuthorityMultica
		if nativeApprovalAttention(e) {
			t.Fatal("native policy affected Multica")
		}
	}
}

func TestCodexExpertTrustFailureIsActionable(t *testing.T) {
	f := newDelegateFixture(t, `printf '%s\n' 'Not inside a trusted directory and --skip-git-repo-check was not specified.' >&2; exit 1`)
	code, out := f.command(t, "", "run", "--adapter", "codex", "--", filepath.Join(f.root, "cursor-agent"), "exec", "--json", "task")
	if code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	id := resultID(t, out)
	for _, cmd := range []string{"status", "result"} {
		code, out = f.command(t, "", cmd, id)
		if code != 0 || !strings.Contains(out, `"code":"codex_git_repo_check_required"`) || !strings.Contains(out, "add --skip-git-repo-check") {
			t.Fatalf("%s %d %s", cmd, code, out)
		}
	}
}
