// Package portableasset contains the immutable portable agentctl distribution
// assets used by bootstrap reconciliation.  The assets are embedded so an
// installed binary can repair a harness without access to its release tree.
//
// The files under assets/ are generated distribution copies.  The repository
// checks their digests against skills/agentctl-portable and distributions at
// build/test time; they are not an independent authoring surface.
package portableasset

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"path"
	"strings"
)

//go:embed assets/SKILL.md assets/revision-manifest.json assets/agentctl-portable.bundle.json
var files embed.FS

const (
	skillPath    = "assets/SKILL.md"
	manifestPath = "assets/revision-manifest.json"
	bundlePath   = "assets/agentctl-portable.bundle.json"
)

// Asset is one immutable distribution asset.
type Asset struct {
	ID       string
	Name     string
	Kind     string
	Bytes    []byte
	Digest   string
	Revision string
}

// Distribution returns the embedded portable skill and manifests.  Returned
// byte slices are copies so callers cannot mutate the package's source data.
func Distribution() (map[string]Asset, error) {
	manifest, err := files.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded revision manifest: %w", err)
	}
	var metadata struct {
		Revision string `json:"revision"`
		Assets   []struct {
			ID   string `json:"id"`
			Path string `json:"path"`
			Hash string `json:"sha256"`
			Name string `json:"install_name"`
			Kind string `json:"kind"`
		} `json:"assets"`
	}
	if err := json.Unmarshal(manifest, &metadata); err != nil {
		return nil, fmt.Errorf("decode embedded revision manifest: %w", err)
	}
	if strings.TrimSpace(metadata.Revision) == "" {
		return nil, fmt.Errorf("embedded revision manifest has no revision")
	}
	byID := make(map[string]Asset, len(metadata.Assets))
	for _, item := range metadata.Assets {
		if item.ID == "" || item.Path == "" || item.Hash == "" {
			continue
		}
		assetPath := ""
		if strings.HasPrefix(item.Path, "skills/agentctl-portable/") {
			assetPath = skillPath
		} else if strings.HasPrefix(item.Path, "distributions/multica/") {
			assetPath = bundlePath
		}
		// Control manifests remain in the release tree but are not needed by
		// bootstrap reconciliation.  They are deliberately not embedded.
		if assetPath == "" {
			continue
		}
		data, err := files.ReadFile(path.Join("assets", path.Base(assetPath)))
		if err != nil {
			return nil, fmt.Errorf("read embedded asset %s: %w", item.ID, err)
		}
		digest := sha256.Sum256(data)
		actual := hex.EncodeToString(digest[:])
		if item.Hash != actual {
			return nil, fmt.Errorf("embedded asset %s digest mismatch: manifest=%s actual=%s", item.ID, item.Hash, actual)
		}
		byID[item.ID] = Asset{ID: item.ID, Name: item.Name, Kind: item.Kind, Bytes: append([]byte(nil), data...), Digest: "sha256:" + actual, Revision: metadata.Revision}
	}
	if _, ok := byID["portable-skill"]; !ok {
		return nil, fmt.Errorf("embedded revision manifest omits portable-skill")
	}
	return byID, nil
}

// Skill returns the embedded portable skill and its revision metadata.
func Skill() (Asset, error) {
	distribution, err := Distribution()
	if err != nil {
		return Asset{}, err
	}
	return distribution["portable-skill"], nil
}

// Manifest returns a copy of the embedded distribution revision manifest.
func Manifest() ([]byte, error) {
	data, err := files.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("read embedded revision manifest: %w", err)
	}
	return append([]byte(nil), data...), nil
}
