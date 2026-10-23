package launchrecipe

import (
	"slices"
	"testing"
)

func TestUnattendedRecipes(t *testing.T) {
	for _, tc := range []struct{ harness, access, flag, permission string }{
		{"codex", "coding", "--dangerously-bypass-approvals-and-sandbox", PermissionsFull},
		{"codex", "read_only", `approval_policy="never"`, PermissionsReadOnly},
		{"cursor", "coding", "--force", PermissionsFull},
		{"cursor", "read_only", "ask", PermissionsReadOnly},
		{"claude-code", "coding", "auto", PermissionsConstrained},
		{"zcode", "coding", "yolo", PermissionsConstrained},
		{"zcode", "read_only", "plan", PermissionsReadOnly},
		{"devin", "coding", "acp", PermissionsFull},
		{"hermes", "coding", "--yolo", PermissionsConstrained},
	} {
		t.Run(tc.harness+"/"+tc.access, func(t *testing.T) {
			model := "test-model"
			if tc.harness == "zcode" {
				model = "glm-5.3"
			}
			r, err := Build(Input{Harness: tc.harness, Model: model, Access: tc.access, UnattendedCodingPermissions: true, CursorWorkspaceTrust: true})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(r.Argv, tc.flag) || r.Permissions != tc.permission {
				t.Fatalf("%#v", r)
			}
			if tc.harness == "codex" && !slices.Contains(r.Argv, "--skip-git-repo-check") {
				t.Fatal("missing git check bypass")
			}
		})
	}
	for _, h := range []string{"codex", "hermes"} {
		r, err := Build(Input{Harness: h, Model: "test-model"})
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(r.Argv, "--yolo") || slices.Contains(r.Argv, "--dangerously-bypass-approvals-and-sandbox") {
			t.Fatalf("implicit grant: %#v", r)
		}
	}
	if _, err := Build(Input{Harness: "hermes", Model: "glm-5.3-flash", Access: "read_only"}); err == nil {
		t.Fatal("Hermes read_only must fail closed")
	}
}
