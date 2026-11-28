package launchrecipe

import (
	"slices"
	"testing"
)

func TestUnattendedRecipes(t *testing.T) {
	for _, tc := range []struct{ harness, access, flag, permission string }{
		{"codex", "coding", "workspace-write", PermissionsConstrained},
		{"codex", "read_only", `approval_policy="never"`, PermissionsReadOnly},
		{"cursor", "coding", "--force", PermissionsFull},
		{"cursor", "read_only", "ask", PermissionsReadOnly},
		{"claude-code", "coding", "bypassPermissions", PermissionsFull},
		{"zcode", "coding", "yolo", PermissionsConstrained},
		{"zcode", "read_only", "plan", PermissionsReadOnly},
		{"devin", "coding", "acp", PermissionsFull},
		{"hermes", "coding", "--yolo", PermissionsConstrained},
		{"opencode", "coding", "--auto", PermissionsConstrained},
	} {
		t.Run(tc.harness+"/"+tc.access, func(t *testing.T) {
			model := "test-model"
			if tc.harness == "zcode" {
				model = "glm-5.3"
			} else if tc.harness == "opencode" {
				model = "provider/test-model"
			}
			r, err := Build(Input{Harness: tc.harness, Model: model, Access: tc.access, UnattendedCodingPermissions: true, CursorWorkspaceTrust: true})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(r.Argv, tc.flag) || r.Permissions != tc.permission {
				t.Fatalf("%#v", r)
			}
			if tc.harness == "codex" && tc.access == "coding" {
				if !slices.Contains(r.Argv, "--skip-git-repo-check") || !slices.Contains(r.Argv, `approval_policy="never"`) || slices.Contains(r.Argv, `approval_policy="on-request"`) || slices.Contains(r.Argv, "--dangerously-bypass-approvals-and-sandbox") || !slices.Contains(r.Env, "AGENTCTL_CODEX_DELEGATE_ALLOWLIST=1") {
					t.Fatalf("Codex grant must declare its headless policy and scoped allowlist: %#v", r)
				}
			}
		})
	}
	for _, h := range []string{"codex", "hermes"} {
		r, err := Build(Input{Harness: h, Model: "test-model"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(r.Argv, "--yolo") || slices.Contains(r.Argv, "--dangerously-bypass-approvals-and-sandbox") || slices.Contains(r.Env, "AGENTCTL_CODEX_DELEGATE_ALLOWLIST=1") {
			t.Fatalf("implicit grant: %#v", r)
		}
	}
	readOnly, err := Build(Input{Harness: "codex", Model: "test-model", Access: AccessReadOnly, UnattendedCodingPermissions: true})
	if err != nil || slices.Contains(readOnly.Env, "AGENTCTL_CODEX_DELEGATE_ALLOWLIST=1") || !slices.Contains(readOnly.Argv, "read-only") || !slices.Contains(readOnly.Argv, `approval_policy="never"`) {
		t.Fatalf("Codex read_only semantics changed: %#v, err=%v", readOnly, err)
	}
	if _, err := Build(Input{Harness: "hermes", Model: "glm-5.3-flash", Access: "read_only"}); err == nil {
		t.Fatal("Hermes read_only must fail closed")
	}
}
