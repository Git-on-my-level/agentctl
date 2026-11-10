package config

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func publishSourceAnx(t *testing.T, author string, anx *Anx) {
	t.Helper()
	bundle, err := decodeBundle([]byte(validBundle))
	if err != nil {
		t.Fatal(err)
	}
	bundle.Anx = anx
	data, err := json.MarshalIndent(bundle, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(author, defaultSourceBundle), data, 0o644); err != nil {
		t.Fatal(err)
	}
	runGitTest(t, author, "add", defaultSourceBundle)
	runGitTest(t, author, "commit", "--quiet", "-m", "configure Anx")
	runGitTest(t, author, "push", "--quiet", "origin", "main")
}

func TestSourceMaterializesAnxOnInitUpdateAndRestore(t *testing.T) {
	remote, author, configPath, checkout := sourceFixture(t)
	ctx := context.Background()
	want := &Anx{RunLinks: &RunLinks{Default: "off"}}
	publishSourceAnx(t, author, want)
	if _, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertAnx := func(want *Anx) Config {
		t.Helper()
		got, err := Load(configPath)
		if err != nil || !reflect.DeepEqual(got.Anx, want) {
			t.Fatalf("source did not materialize Anx: got=%#v want=%#v err=%v", got.Anx, want, err)
		}
		return got
	}
	assertAnx(want)
	want = &Anx{RunLinks: &RunLinks{Default: "all"}}
	publishSourceAnx(t, author, want)
	if updated, err := UpdateSource(ctx, configPath, time.Now()); err != nil || !updated.Changed || !updated.Status.InSync {
		t.Fatalf("update=%#v err=%v", updated, err)
	}
	cfg := assertAnx(want)
	cfg.Anx.RunLinks.Default = "off"
	if err := Save(configPath, cfg, true); err != nil {
		t.Fatal(err)
	}
	if _, err := UpdateSourceWithGit(ctx, localOnlySourceGit{t}, configPath, time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("manual Anx edit was not refused: %v", err)
	}
	if restored, err := RestoreSourceWithGit(ctx, localOnlySourceGit{t}, configPath); err != nil || !restored.Status.InSync {
		t.Fatalf("restore=%#v err=%v", restored, err)
	}
	assertAnx(want)
	publishSourceAnx(t, author, nil)
	if _, err := UpdateSource(ctx, configPath, time.Now()); err != nil {
		t.Fatal(err)
	}
	assertAnx(nil)
}

func TestSourceInitPreservesExistingAnx(t *testing.T) {
	on := &Anx{RunLinks: &RunLinks{Default: "all"}}
	off := &Anx{RunLinks: &RunLinks{Default: "off"}}
	for _, tt := range []struct {
		name     string
		bundle   *Anx
		existing *Anx
		conflict bool
	}{
		{name: "cannot remove", existing: off, conflict: true},
		{name: "cannot replace", bundle: on, existing: off, conflict: true},
		{name: "matching preserved", bundle: off, existing: off},
		{name: "missing added", bundle: on},
	} {
		t.Run(tt.name, func(t *testing.T) {
			remote, author, configPath, checkout := sourceFixture(t)
			if tt.bundle != nil {
				publishSourceAnx(t, author, tt.bundle)
			}
			cfg := Config{SchemaVersion: SchemaVersion, Anx: tt.existing}
			if err := Save(configPath, cfg, false); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(configPath)
			updated, err := InitSource(context.Background(), configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now())
			if tt.conflict {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("conflicting Anx init error=%v", err)
				}
				after, _ := os.ReadFile(configPath)
				if string(before) != string(after) {
					t.Fatal("conflicting init rewrote live Anx")
				}
				if _, err := os.Lstat(checkout); !os.IsNotExist(err) {
					t.Fatalf("conflicting init activated checkout: %v", err)
				}
				return
			}
			got, loadErr := Load(configPath)
			if err != nil || loadErr != nil || !updated.Status.InSync || !reflect.DeepEqual(got.Anx, tt.bundle) {
				t.Fatalf("init=%#v got=%#v err=%v loadErr=%v", updated, got.Anx, err, loadErr)
			}
		})
	}
}

func TestSourceRepairsHistoricalAnxOmission(t *testing.T) {
	for _, schema := range []int{1, 2} {
		for _, operation := range []string{"update", "restore"} {
			name := operation + "/schema1"
			if schema == 2 {
				name = operation + "/schema2"
			}
			t.Run(name, func(t *testing.T) {
				remote, author, configPath, checkout := sourceFixture(t)
				ctx := context.Background()
				want := &Anx{RunLinks: &RunLinks{Default: "off"}}
				publishSourceAnx(t, author, want)
				initial, err := InitSource(ctx, configPath, SourceSpec{Remote: remote, CheckoutPath: checkout}, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				// Recreate the historical writer's output and its matching
				// applied state. Host-local changes after that must also survive.
				cfg, err := Load(configPath)
				if err != nil {
					t.Fatal(err)
				}
				cfg.Anx = nil
				if err := Save(configPath, cfg, true); err != nil {
					t.Fatal(err)
				}
				state := legacySourceState(t, configPath)
				if schema == 2 {
					state.SchemaVersion = 2
					state.LiveSHA256, err = sourceConfigSHA256(cfg)
					if err != nil {
						t.Fatal(err)
					}
					statePath, _ := SourceStatePath(configPath)
					if err := SaveSourceState(statePath, state, true); err != nil {
						t.Fatal(err)
					}
				}
				off := false
				cfg.Journal = &Journal{Retention: &JournalRetention{Enabled: &off}}
				if err := Save(configPath, cfg, true); err != nil {
					t.Fatal(err)
				}
				apply := func() (SourceUpdate, error) {
					if operation == "restore" {
						return RestoreSourceWithGit(ctx, localOnlySourceGit{t}, configPath)
					}
					return UpdateSource(ctx, configPath, time.Now())
				}
				updated, err := apply()
				if err != nil || !updated.Changed || !updated.Status.InSync || updated.Status.AppliedCommit != initial.Status.AppliedCommit {
					t.Fatalf("historical repair=%#v err=%v", updated, err)
				}
				got, err := Load(configPath)
				if err != nil || !reflect.DeepEqual(got.Anx, want) || !reflect.DeepEqual(got.Journal, cfg.Journal) {
					t.Fatalf("repair lost Anx or local policy: %#v err=%v", got, err)
				}
				if again, err := apply(); err != nil || again.Changed || !again.Status.InSync {
					t.Fatalf("repair is not idempotent: %#v err=%v", again, err)
				}
				got.Anx.RunLinks.Default = "all"
				if err := Save(configPath, got, true); err != nil {
					t.Fatal(err)
				}
				if _, err := UpdateSourceWithGit(ctx, localOnlySourceGit{t}, configPath, time.Now()); !errors.Is(err, ErrConflict) {
					t.Fatalf("repaired Anx is not protected from drift: %v", err)
				}
			})
		}
	}
}

func TestNativeSourceGitReturnsOnlyStdoutOnSuccess(t *testing.T) {
	binary := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\nprintf '%s' 'bundle bytes'\nprintf '%s\\n' 'warning on stderr' >&2\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	out, err := (NativeSourceGit{Binary: binary}).Run(context.Background(), t.TempDir(), "show", "HEAD:config-bundle.json")
	if err != nil || string(out) != "bundle bytes" {
		t.Fatalf("stderr contaminated Git stdout: output=%q err=%v", out, err)
	}
}
