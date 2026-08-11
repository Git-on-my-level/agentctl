package portableasset

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestEmbeddedDistributionMatchesCanonicalSources(t *testing.T) {
	assets, err := Distribution()
	if err != nil {
		t.Fatal(err)
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
