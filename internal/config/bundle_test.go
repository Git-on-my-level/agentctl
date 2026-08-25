package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const validBundle = `{
  "schema_version": 1,
  "default_profile": "private",
  "profiles": {
    "private": {
      "adapters": {"codex": {"executable": "/opt/bin/codex"}},
      "agent_preferences": {
        "mode": "advisory",
        "preferred": [
          {"agent": "cursor", "model": "composer-2.5", "speed": "regular", "use_for": "default"},
          {"agent": "cursor", "model": "cursor-grok-4.6-high", "speed": "regular", "use_for": "harder_tasks"}
        ],
        "notes": ["Never select fast model variants."]
      },
      "multica": {
        "executable": "/opt/bin/multica",
        "profile": "operator",
        "workspace_id": "workspace-1",
        "server_url": "https://coord.example",
        "app_url": "https://coord.example"
      }
    }
  }
}`

func writeBundle(t *testing.T, content string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "bundle.json")
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadBundleAcceptsReadOnlyGitFileAndReportsDigest(t *testing.T) {
	path := writeBundle(t, validBundle, 0o644)
	bundle, provenance, err := LoadBundle(path)
	if err != nil {
		t.Fatal(err)
	}
	if bundle.DefaultProfile != "private" || provenance.SHA256 == "" || provenance.SourcePath != path || provenance.Bytes != int64(len(validBundle)) {
		t.Fatalf("unexpected bundle/provenance: %#v %#v", bundle, provenance)
	}
	preferences := bundle.Profiles["private"].AgentPreferences
	if preferences == nil || len(preferences.Preferred) != 2 || preferences.Preferred[1].Model != "cursor-grok-4.6-high" {
		t.Fatalf("agent preferences were not decoded: %#v", preferences)
	}
	_, second, err := LoadBundle(path)
	if err != nil || second.SHA256 != provenance.SHA256 {
		t.Fatalf("digest not deterministic: %#v %v", second, err)
	}
}

func TestAgentPreferencesAreAdvisoryBoundedAndStrict(t *testing.T) {
	valid := AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{{Agent: "cursor", Model: "composer-2.5", Speed: "regular"}}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid advisory preferences rejected: %v", err)
	}
	for name, preferences := range map[string]AgentPreferences{
		"enforced mode": {Mode: "enforced", Preferred: valid.Preferred},
		"empty list":    {Mode: "advisory"},
		"missing speed": {Mode: "advisory", Preferred: []AgentPreference{{Agent: "cursor", Model: "composer-2.5"}}},
		"duplicate":     {Mode: "advisory", Preferred: append(valid.Preferred, valid.Preferred...)},
	} {
		t.Run(name, func(t *testing.T) {
			if err := preferences.Validate(); err == nil {
				t.Fatal("invalid preferences unexpectedly accepted")
			}
		})
	}
}

func TestLiveConfigRoundTripsAgentPreferences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	preferences := &AgentPreferences{Mode: "advisory", Preferred: []AgentPreference{{Agent: "cursor", Model: "composer-2.5", Speed: "regular"}}, Notes: []string{"Never fast."}}
	cfg := Config{SchemaVersion: SchemaVersion, DefaultProfile: "guidance", Profiles: map[string]Profile{"guidance": {AgentPreferences: preferences}}}
	if err := Save(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Profiles["guidance"].AgentPreferences
	if got == nil || got.Preferred[0].Model != "composer-2.5" || got.Notes[0] != "Never fast." {
		t.Fatalf("agent preferences did not round-trip: %#v", got)
	}
}

func TestSkillHubSelectionRoundTripsAndRejectsUnsafePolicy(t *testing.T) {
	selection := &Skills{Source: SkillSource{Remote: "https://git.example.test/david/skill-hub.git", Ref: "main", ManifestPath: "manifests/agentctl/fleet-core.json"}, UpdatePolicy: "auto-clean"}
	path := filepath.Join(t.TempDir(), "config.json")
	cfg := Config{SchemaVersion: SchemaVersion, Skills: selection}
	if err := Save(path, cfg, false); err != nil {
		t.Fatal(err)
	}
	loaded, err := Load(path)
	if err != nil || loaded.Skills == nil || loaded.Skills.Source.ManifestPath != selection.Source.ManifestPath {
		t.Fatalf("skill selection did not round-trip: %#v %v", loaded.Skills, err)
	}
	for name, invalid := range map[string]Skills{
		"credential remote": {Source: SkillSource{Remote: "https://user:secret@git.example.test/hub.git", ManifestPath: "pack.json"}, UpdatePolicy: "manual"},
		"relative remote":   {Source: SkillSource{Remote: "../hub.git", ManifestPath: "pack.json"}, UpdatePolicy: "manual"},
		"malformed remote":  {Source: SkillSource{Remote: "://hub.git", ManifestPath: "pack.json"}, UpdatePolicy: "manual"},
		"escaping manifest": {Source: SkillSource{Remote: "https://git.example.test/hub.git", ManifestPath: "../pack.json"}, UpdatePolicy: "manual"},
		"implicit policy":   {Source: SkillSource{Remote: "https://git.example.test/hub.git", ManifestPath: "pack.json"}, UpdatePolicy: "auto"},
	} {
		t.Run(name, func(t *testing.T) {
			if err := invalid.Validate(); err == nil {
				t.Fatal("unsafe Skill Hub selection was accepted")
			}
		})
	}
}

func TestLoadBundleFailsClosedForArgumentsCredentialsAndUnknownFields(t *testing.T) {
	cases := map[string]string{
		"arguments":    strings.Replace(validBundle, `"executable": "/opt/bin/codex"`, `"executable": "/opt/bin/codex", "arguments": ["--danger"]`, 1),
		"credential":   strings.Replace(validBundle, `"server_url": "https://coord.example"`, `"server_url": "https://user:secret@coord.example"`, 1),
		"token":        strings.Replace(validBundle, `"schema_version": 1`, `"schema_version": 1, "token": "secret"`, 1),
		"insecure_url": strings.Replace(validBundle, `https://coord.example`, `http://coord.example`, 1),
	}
	for name, document := range cases {
		t.Run(name, func(t *testing.T) {
			if _, _, err := LoadBundle(writeBundle(t, document, 0o600)); err == nil {
				t.Fatal("unsafe bundle unexpectedly accepted")
			}
		})
	}
}

func TestLoadBundleRejectsWritableOrSymlinkSource(t *testing.T) {
	writable := writeBundle(t, validBundle, 0o600)
	if err := os.Chmod(writable, 0o666); err != nil {
		t.Fatal(err)
	}
	if _, _, err := LoadBundle(writable); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("writable source error=%v, want ErrUnmanaged", err)
	}
	root := t.TempDir()
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte(validBundle), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "bundle.json")
	if err := os.Symlink(target, link); err != nil {
		t.Skip(err)
	}
	if _, _, err := LoadBundle(link); !errors.Is(err, ErrUnmanaged) {
		t.Fatalf("symlink source error=%v, want ErrUnmanaged", err)
	}
}

func TestResolveBundleIsAdditiveAndWorksWithoutBaseConfig(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.json")
	path := writeBundle(t, validBundle, 0o644)
	resolution, err := Resolve(missing, path)
	if err != nil {
		t.Fatal(err)
	}
	if resolution.BasePresent || resolution.Bundle == nil || len(resolution.Composition) != 3 {
		t.Fatalf("unexpected resolution: %#v", resolution)
	}
	name, _, err := resolution.Config.ResolveProfile("")
	if err != nil || name != "private" {
		t.Fatalf("resolved name=%q err=%v", name, err)
	}
	if _, err := Resolve(missing, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("implicit discovery occurred: %v", err)
	}
}

func TestResolveBundleCannotReplaceUserConfig(t *testing.T) {
	basePath := filepath.Join(t.TempDir(), "config.json")
	base := Config{SchemaVersion: SchemaVersion, DefaultProfile: "local", Profiles: map[string]Profile{"local": {Adapters: map[string]Adapter{"codex": {Executable: "/usr/bin/codex"}}}}}
	if err := Save(basePath, base, false); err != nil {
		t.Fatal(err)
	}
	bundlePath := writeBundle(t, strings.ReplaceAll(validBundle, "private", "local"), 0o644)
	if _, err := Resolve(basePath, bundlePath); !errors.Is(err, ErrConflict) {
		t.Fatalf("replacement error=%v, want ErrConflict", err)
	}
}

func TestResolveBundleAllowsIdenticalIdempotentProfile(t *testing.T) {
	bundlePath := writeBundle(t, validBundle, 0o644)
	bundle, _, err := LoadBundle(bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	basePath := filepath.Join(t.TempDir(), "config.json")
	base := Config{SchemaVersion: SchemaVersion, DefaultProfile: bundle.DefaultProfile, Profiles: map[string]Profile{"private": bundle.Profiles["private"].profile()}}
	if err := Save(basePath, base, false); err != nil {
		t.Fatal(err)
	}
	resolution, err := Resolve(basePath, bundlePath)
	if err != nil {
		t.Fatal(err)
	}
	if !resolution.BasePresent || len(resolution.Config.Profiles) != 1 {
		t.Fatalf("unexpected idempotent resolution: %#v", resolution)
	}
	plan, err := PlanBundle(basePath, bundlePath)
	if err != nil || plan.Mutates || !plan.Valid || len(plan.AddProfiles) != 0 || len(plan.Existing) != 1 {
		t.Fatalf("unexpected read-only plan: %#v %v", plan, err)
	}
}

func TestConfigStrictDecodeRejectsUnknownFields(t *testing.T) {
	for name, document := range map[string]string{
		"credential": `{"schema_version":1,"profiles":{},"access_token":"secret"}`,
		"arguments":  `{"schema_version":1,"profiles":{"local":{"adapters":{"codex":{"executable":"/bin/echo","arguments":["--danger"]}}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("unknown config field unexpectedly accepted")
			}
		})
	}
}

func TestMulticaURLPolicyAllowsHTTPSAndLoopbackHTTPOnly(t *testing.T) {
	base := Multica{Executable: "multica", Profile: "p", WorkspaceID: "w", ServerURL: "http://127.0.0.1:8080", AppURL: "http://localhost:8081"}
	if err := base.Validate(); err != nil {
		t.Fatalf("loopback HTTP rejected: %v", err)
	}
	base.ServerURL = "http://service.internal"
	if err := base.Validate(); err == nil {
		t.Fatal("non-loopback HTTP unexpectedly accepted")
	}
}
