package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// Simulate the whole-file digest written by released binaries, including a
// policy present at the last apply (which cannot be reconstructed from Git).
func legacySourceState(t *testing.T, configPath string) SourceState {
	t.Helper()
	statePath, _ := SourceStatePath(configPath)
	state, err := LoadSourceState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	state.SchemaVersion = 1
	state.LiveSHA256, err = fileSHA256(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSourceState(statePath, state, true); err != nil {
		t.Fatal(err)
	}
	return state
}

func TestSourceHostLocalPolicyChanges(t *testing.T) {
	off, on := false, true
	tests := []struct {
		name   string
		before Config
		after  Config
	}{
		{name: "add journal", after: Config{Journal: &Journal{Retention: &JournalRetention{Enabled: &off}}}},
		{name: "change journal", before: Config{Journal: &Journal{Retention: &JournalRetention{Enabled: &on, Days: 14}}}, after: Config{Journal: &Journal{Retention: &JournalRetention{Enabled: &off, Days: 30}}}},
		{name: "remove journal", before: Config{Journal: &Journal{Retention: &JournalRetention{Enabled: &off}}}},
		{name: "add bootstrap", after: Config{Bootstrap: &Bootstrap{InstructionPointers: "off"}}},
		{name: "change bootstrap", before: Config{Bootstrap: &Bootstrap{InstructionPointers: "manage"}}, after: Config{Bootstrap: &Bootstrap{InstructionPointers: "off"}}},
		{name: "remove bootstrap", before: Config{Bootstrap: &Bootstrap{InstructionPointers: "off"}}},
	}
	for _, legacy := range []bool{false, true} {
		version := "current/"
		if legacy {
			version = "legacy/"
		}
		for _, tt := range tests {
			t.Run(version+tt.name, func(t *testing.T) {
				remote, author, configPath, checkout := sourceFixture(t)
				ctx := context.Background()
				initialConfig := tt.before
				initialConfig.SchemaVersion = SchemaVersion
				if err := Save(configPath, initialConfig, false); err != nil {
					t.Fatal(err)
				}
				initial, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				if legacy {
					legacySourceState(t, configPath)
				}
				cfg, err := Load(configPath)
				if err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(cfg.Journal, tt.before.Journal) || !reflect.DeepEqual(cfg.Bootstrap, tt.before.Bootstrap) {
					t.Fatalf("init lost host-local policy: %#v", cfg)
				}
				cfg.Journal, cfg.Bootstrap = tt.after.Journal, tt.after.Bootstrap
				if err := Save(configPath, cfg, true); err != nil {
					t.Fatal(err)
				}
				statePath, _ := SourceStatePath(configPath)
				beforeState, _ := os.ReadFile(statePath)
				beforeLive, _ := os.ReadFile(configPath)
				status, err := SourceStatusWithGit(ctx, localOnlySourceGit{t}, configPath)
				if err != nil || !status.InSync || status.LiveSHA256 != status.ExpectedLiveSHA {
					t.Fatalf("host-local change drifted: status=%#v err=%v", status, err)
				}
				afterState, _ := os.ReadFile(statePath)
				if string(beforeState) != string(afterState) {
					t.Fatal("read-only status migrated state")
				}
				updated, err := UpdateSource(ctx, configPath, time.Now())
				if err != nil || !updated.Status.InSync || updated.Changed != legacy {
					t.Fatalf("update did not converge: update=%#v err=%v", updated, err)
				}
				afterLive, _ := os.ReadFile(configPath)
				if string(beforeLive) != string(afterLive) {
					t.Fatal("migration rewrote live config")
				}
				state, err := LoadSourceState(statePath)
				if err != nil || state.SchemaVersion != SourceSchemaVersion || !state.AppliedAt.Equal(*initial.Status.LastAppliedAt) {
					t.Fatalf("unexpected migrated state: %#v err=%v", state, err)
				}
				if again, err := UpdateSource(ctx, configPath, time.Now()); err != nil || again.Changed || !again.Status.InSync {
					t.Fatalf("update was not idempotent: %#v err=%v", again, err)
				}

				// A real source update still applies bundle-owned changes while
				// preserving the host's policy, including policy removal.
				nextBundle := strings.Replace(validBundle, "/opt/bin/codex", "/new/bin/codex", 1)
				if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), []byte(nextBundle), 0o644); err != nil {
					t.Fatal(err)
				}
				runGitTest(t, author, "add", defaultSourceBundle)
				runGitTest(t, author, "commit", "--quiet", "-m", "update adapter")
				runGitTest(t, author, "push", "--quiet", "origin", "main")
				if advanced, err := UpdateSource(ctx, configPath, time.Now()); err != nil || !advanced.Changed || !advanced.Status.InSync {
					t.Fatalf("source did not advance: %#v err=%v", advanced, err)
				}
				cfg, err = Load(configPath)
				if err != nil || cfg.Profiles["private"].Adapters["codex"].Executable != "/new/bin/codex" || !reflect.DeepEqual(cfg.Journal, tt.after.Journal) || !reflect.DeepEqual(cfg.Bootstrap, tt.after.Bootstrap) {
					t.Fatalf("update lost policy or bundle-owned change: %#v err=%v", cfg, err)
				}
			})
		}
	}
}

type localOnlySourceGit struct{ t *testing.T }

func (g localOnlySourceGit) Run(ctx context.Context, dir string, args ...string) ([]byte, error) {
	g.t.Helper()
	if args[0] != "show" && args[0] != "rev-parse" && args[0] != "status" && args[0] != "config" {
		g.t.Fatalf("unexpected mutation/network operation: git %v", args)
	}
	return (NativeSourceGit{}).Run(ctx, dir, args...)
}

func TestSourceBundleOwnedChangesStillDrift(t *testing.T) {
	for _, legacy := range []bool{false, true} {
		for _, field := range []string{"adapter", "multica", "default_profile", "skills", "anx"} {
			name := "current/"
			if legacy {
				name = "legacy/"
			}
			t.Run(name+field, func(t *testing.T) {
				remote, _, configPath, checkout := sourceFixture(t)
				ctx := context.Background()
				if _, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err != nil {
					t.Fatal(err)
				}
				if legacy {
					legacySourceState(t, configPath)
				}
				cfg, err := Load(configPath)
				if err != nil {
					t.Fatal(err)
				}
				off := false
				cfg.Journal = &Journal{Retention: &JournalRetention{Enabled: &off}}
				cfg.Bootstrap = &Bootstrap{InstructionPointers: "off"}
				switch field {
				case "adapter":
					cfg.Profiles["private"].Adapters["codex"] = Adapter{Executable: "/different/codex"}
				case "multica":
					cfg.Profiles["private"].Multica.WorkspaceID = "other-workspace"
				case "default_profile":
					cfg.DefaultProfile = ""
				case "skills":
					cfg.Skills = &Skills{Source: SkillSource{Remote: remote, ManifestPath: "pack.json"}, UpdatePolicy: "manual"}
				case "anx":
					cfg.Anx = &Anx{RunLinks: &RunLinks{Default: "off"}}
				}
				if err := Save(configPath, cfg, true); err != nil {
					t.Fatal(err)
				}
				statePath, _ := SourceStatePath(configPath)
				beforeState, _ := os.ReadFile(statePath)
				beforeLive, _ := os.ReadFile(configPath)
				status, err := SourceStatusReadOnly(ctx, configPath)
				if err != nil || status.InSync || !containsString(status.Drift, "live_config") {
					t.Fatalf("bundle-owned drift missed: %#v err=%v", status, err)
				}
				if _, err := UpdateSourceWithGit(ctx, localOnlySourceGit{t}, configPath, time.Now()); !errors.Is(err, ErrConflict) {
					t.Fatalf("drifted update must refuse before fetch: %v", err)
				}
				afterState, _ := os.ReadFile(statePath)
				afterLive, _ := os.ReadFile(configPath)
				if string(beforeState) != string(afterState) || string(beforeLive) != string(afterLive) {
					t.Fatal("refused update changed config or state")
				}
				restored, err := RestoreSourceWithGit(ctx, localOnlySourceGit{t}, configPath)
				if err != nil || !restored.Changed || !restored.Status.InSync {
					t.Fatalf("restore failed: %#v err=%v", restored, err)
				}
				got, err := Load(configPath)
				if err != nil || !reflect.DeepEqual(got.Journal, cfg.Journal) || !reflect.DeepEqual(got.Bootstrap, cfg.Bootstrap) {
					t.Fatalf("restore lost host-local policy: %#v err=%v", got, err)
				}
			})
		}
	}
}

func TestLegacySourceRecoveryRequiresVerifiedAppliedBundle(t *testing.T) {
	for _, problem := range []string{"bundle digest", "missing applied commit", "missing checkout", "dirty checkout"} {
		t.Run(problem, func(t *testing.T) {
			remote, _, configPath, checkout := sourceFixture(t)
			ctx := context.Background()
			if _, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err != nil {
				t.Fatal(err)
			}
			state := legacySourceState(t, configPath)
			statePath, _ := SourceStatePath(configPath)
			switch problem {
			case "bundle digest":
				state.BundleSHA256 = strings.Repeat("0", 64)
			case "missing applied commit":
				state.AppliedCommit = strings.Repeat("0", 40)
			case "missing checkout":
				if err := os.Rename(checkout, checkout+".moved"); err != nil {
					t.Fatal(err)
				}
			case "dirty checkout":
				if err := os.WriteFile(filepath.Join(checkout, "local.txt"), []byte("local"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if err := SaveSourceState(statePath, state, true); err != nil {
				t.Fatal(err)
			}
			beforeState, _ := os.ReadFile(statePath)
			beforeLive, _ := os.ReadFile(configPath)
			status, err := SourceStatusWithGit(ctx, localOnlySourceGit{t}, configPath)
			if err != nil || status.InSync {
				t.Fatalf("unverified state was accepted: %#v err=%v", status, err)
			}
			if _, err := UpdateSourceWithGit(ctx, localOnlySourceGit{t}, configPath, time.Now()); err == nil {
				t.Fatal("unverified update succeeded")
			}
			if _, err := RestoreSourceWithGit(ctx, localOnlySourceGit{t}, configPath); err == nil {
				t.Fatal("unverified restore succeeded")
			}
			afterState, _ := os.ReadFile(statePath)
			afterLive, _ := os.ReadFile(configPath)
			if string(beforeState) != string(afterState) || string(beforeLive) != string(afterLive) {
				t.Fatal("failed recovery changed live config or state")
			}
		})
	}
}

func TestLegacySourceRecoveryWithAdvancingRemote(t *testing.T) {
	for _, localDrift := range []bool{false, true} {
		name := "host-local only"
		if localDrift {
			name = "bundle-owned drift matches next bundle"
		}
		t.Run(name, func(t *testing.T) {
			remote, author, configPath, checkout := sourceFixture(t)
			ctx := context.Background()
			if _, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err != nil {
				t.Fatal(err)
			}
			legacySourceState(t, configPath)
			cfg, err := Load(configPath)
			if err != nil {
				t.Fatal(err)
			}
			off := false
			cfg.Journal = &Journal{Retention: &JournalRetention{Enabled: &off}}
			if localDrift {
				cfg.Profiles["private"].Adapters["codex"] = Adapter{Executable: "/new/bin/codex"}
			}
			if err := Save(configPath, cfg, true); err != nil {
				t.Fatal(err)
			}
			nextBundle := strings.Replace(validBundle, "/opt/bin/codex", "/new/bin/codex", 1)
			if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), []byte(nextBundle), 0o644); err != nil {
				t.Fatal(err)
			}
			runGitTest(t, author, "add", defaultSourceBundle)
			runGitTest(t, author, "commit", "--quiet", "-m", "update adapter")
			runGitTest(t, author, "push", "--quiet", "origin", "main")
			updated, err := UpdateSource(ctx, configPath, time.Now())
			if localDrift {
				if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "live config drifted") {
					t.Fatalf("next bundle must not bless existing drift: %v", err)
				}
				return
			}
			if err != nil || !updated.Changed || !updated.Status.InSync {
				t.Fatalf("advance and migrate: %#v err=%v", updated, err)
			}
			got, err := Load(configPath)
			if err != nil || got.Profiles["private"].Adapters["codex"].Executable != "/new/bin/codex" || !reflect.DeepEqual(got.Journal, cfg.Journal) {
				t.Fatalf("advance lost local policy or source change: %#v err=%v", got, err)
			}
			statePath, _ := SourceStatePath(configPath)
			state, err := LoadSourceState(statePath)
			if err != nil || state.SchemaVersion != SourceSchemaVersion || state.AppliedCommit != runGitTest(t, author, "rev-parse", "HEAD") {
				t.Fatalf("advance did not migrate state: %#v err=%v", state, err)
			}
		})
	}
}
