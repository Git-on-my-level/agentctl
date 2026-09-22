package main

import (
	"bytes"
	"encoding/json"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPublishedAdoptionPlanApplyAndDrift(t *testing.T) {
	home := t.TempDir()
	home, _ = filepath.EvalSymlinks(home)
	root := filepath.Join(home, ".hermes", "skills", "agentctl-portable")
	os.MkdirAll(root, 0700)
	legacy, err := os.ReadFile("testdata/portable-v0.4.2.md")
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(root, "SKILL.md"), legacy, 0600)
	var buf bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Writer: &buf}
	a := &app{}
	args := []string{"--home", home, "--harness", "hermes"}
	if problem := a.bootstrapAdopt(renderer, args); problem != nil {
		t.Fatal(problem)
	}
	var doc struct {
		Result adoptionPlan `json:"result"`
	}
	json.Unmarshal(buf.Bytes(), &doc)
	if doc.Result.State != "planned" || doc.Result.Digest == "" {
		t.Fatal(buf.String())
	}
	if _, err := os.Stat(filepath.Join(home, ".local", "share", "agentctl", "adoption-backups")); !os.IsNotExist(err) {
		t.Fatal("plan mutated disk")
	}
	apply := append(append([]string{}, args...), "--apply", "--expected-digest", doc.Result.Digest)
	os.WriteFile(filepath.Join(root, "notes.md"), []byte("custom"), 0600)
	if problem := a.bootstrapAdopt(renderer, apply); problem == nil {
		t.Fatal("adopted custom extra file")
	}
	os.Remove(filepath.Join(root, "notes.md"))
	os.WriteFile(filepath.Join(root, "SKILL.md"), append(append([]byte{}, legacy...), []byte("custom")...), 0600)
	if problem := a.bootstrapAdopt(renderer, apply); problem == nil {
		t.Fatal("adopted modified skill")
	}
	os.WriteFile(filepath.Join(root, "SKILL.md"), legacy, 0600)
	buf.Reset()
	if problem := a.bootstrapAdopt(renderer, apply); problem != nil {
		t.Fatal(problem)
	}
	json.Unmarshal(buf.Bytes(), &doc)
	if !strings.HasPrefix(doc.Result.Backup, filepath.Join(home, ".local", "share", "agentctl", "adoption-backups")+string(filepath.Separator)) {
		t.Fatal("backup is inside a skill discovery root")
	}
	saved, err := os.ReadFile(filepath.Join(doc.Result.Backup, "SKILL.md"))
	if err != nil || !bytes.Equal(saved, legacy) {
		t.Fatal("backup did not preserve legacy bytes", err)
	}
	if present, valid := managedMarkerIntegrity(filepath.Dir(root)); !present || !valid {
		t.Fatal("missing valid ownership")
	}
}

func TestAdoptionMissingHomeIsExplicit(t *testing.T) {
	t.Setenv("HOME", "")
	var buf bytes.Buffer
	a := &app{}
	problem := a.bootstrapAdopt(output.Renderer{Mode: output.JSON, Writer: &buf}, []string{"--harness", "hermes"})
	if problem == nil || problem.Code != output.CodeDependencyUnavailable {
		t.Fatalf("missing home: %v", problem)
	}
}
