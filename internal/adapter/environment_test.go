package adapter

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"testing"
)

func TestWithoutCallerDeclarationPreservesEnvironment(t *testing.T) {
	env := []string{"A=1", CallerHarnessEnv + "=hermes", "B=two=three", CallerHarnessEnv + "=codex", "AGENTCTL_CALLER_HARNESS_EXTRA=keep", CallerHarnessEnv}
	original := append([]string(nil), env...)
	got := WithoutCallerDeclaration(env)
	want := []string{"A=1", "B=two=three", "AGENTCTL_CALLER_HARNESS_EXTRA=keep"}
	if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(env, original) {
		t.Fatalf("filtered=%v source=%v", got, env)
	}
}

func TestMergeEnvironmentReplacesInheritedSelectors(t *testing.T) {
	got := mergeEnvironment(
		[]string{"KEEP=two=three", "ZCODE_MODEL=glm-5.3-flash", "ZCODE_MODEL=duplicate"},
		[]string{"ZCODE_MODEL=glm-5.3", "ZCODE_MODEL="},
	)
	want := []string{"KEEP=two=three", "ZCODE_MODEL="}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("environment=%v want=%v", got, want)
	}
}

func TestWithoutCallerDeclarationMatchesPlatformKeySemantics(t *testing.T) {
	lowercase := "agentctl_caller_harness=hermes"
	got := WithoutCallerDeclaration([]string{lowercase})
	if runtime.GOOS == "windows" {
		if len(got) != 0 {
			t.Fatal("case-insensitive caller environment survived on Windows")
		}
	} else if !reflect.DeepEqual(got, []string{lowercase}) {
		t.Fatal("unrelated case-sensitive environment was changed")
	}
}

func TestNativeLaunchStripsInheritedCallerAndAllowsExplicitChildDeclaration(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fixture uses a POSIX shell")
	}
	t.Setenv(CallerHarnessEnv, "hermes")
	for _, tc := range []struct {
		name string
		env  []string
		want string
	}{{"inherited", nil, "unset"}, {"explicit_child", []string{CallerHarnessEnv + "=cursor"}, "cursor"}} {
		t.Run(tc.name, func(t *testing.T) {
			capture := filepath.Join(t.TempDir(), "caller")
			_, err := NewGenericProcess().Launch(context.Background(), LaunchRequest{Argv: []string{"/bin/sh", "-c", `printf '%s' "${AGENTCTL_CALLER_HARNESS-unset}" > "$1"`, "_", capture}, Env: tc.env})
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(capture)
			if err != nil || string(got) != tc.want {
				t.Fatalf("child declaration=%q want=%q err=%v", got, tc.want, err)
			}
		})
	}
}
