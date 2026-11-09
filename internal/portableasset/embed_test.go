package portableasset

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedDistributionMatchesCanonicalSources(t *testing.T) {
	assets, err := Distribution()
	if err != nil {
		t.Fatal(err)
	}
	// Byte-for-byte copies can agree while the Multica bundle still points
	// at a previous skill. Keep this cross-asset contract in the cheap push gate.
	var bundle struct {
		Revision string `json:"revision"`
		Skill    struct {
			SHA256 string `json:"sha256"`
		} `json:"skill"`
	}
	if err := json.Unmarshal(assets["multica-runtime-bundle"].Bytes, &bundle); err != nil {
		t.Fatal(err)
	}
	skill := assets["portable-skill"]
	if "sha256:"+bundle.Skill.SHA256 != skill.Digest || bundle.Revision != skill.Revision {
		t.Fatalf("Multica bundle does not bind the embedded skill: hash=%s revision=%s", bundle.Skill.SHA256, bundle.Revision)
	}
	root := filepath.Join("..", "..")
	for id, relative := range map[string]string{
		"portable-skill":         filepath.Join(root, "skills", "agentctl-portable", "SKILL.md"),
		"multica-runtime-bundle": filepath.Join(root, "distributions", "multica", "agentctl-portable.bundle.json"),
	} {
		asset, ok := assets[id]
		if !ok {
			t.Fatalf("embedded asset %s missing", id)
		}
		canonical, err := os.ReadFile(relative)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(asset.Bytes, canonical) {
			t.Fatalf("embedded %s differs from %s", id, relative)
		}
	}
	manifest, err := Manifest()
	if err != nil {
		t.Fatal(err)
	}
	canonicalManifest, err := os.ReadFile(filepath.Join(root, "distributions", "revision-manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifest, canonicalManifest) {
		t.Fatal("embedded revision manifest differs from canonical source")
	}
}
