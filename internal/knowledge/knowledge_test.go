package knowledge

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func TestSyncSourceUsesNativeGitForLocalFixture(t *testing.T) {
	origin := t.TempDir()
	run := func(dir string, args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run(origin, "init", "--quiet", "-b", "main")
	if err := os.WriteFile(filepath.Join(origin, "README.md"), []byte("fixture"), 0644); err != nil {
		t.Fatal(err)
	}
	run(origin, "add", "README.md")
	run(origin, "-c", "user.email=fixture@example.test", "-c", "user.name=Fixture", "commit", "--quiet", "-m", "fixture")
	checkout := filepath.Join(t.TempDir(), "checkout")
	reg := fixtureReg()
	reg.Remote.URL = origin
	result, err := SyncSource(context.Background(), reg, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if result.Commit == "" || result.TreeDigest == "" {
		t.Fatalf("missing resolved revision: %#v", result)
	}
	if _, err := os.Stat(filepath.Join(checkout, "README.md")); err != nil {
		t.Fatalf("checkout missing content: %v", err)
	}
}

func fixtureReg() SourceRegistration {
	return SourceRegistration{SchemaVersion: 1, ID: "repo-amber-willow-orbit-tiger-harbor-gentle", Slug: "fixture-knowledge", Mode: ModeLoose, Remote: Remote{Provider: ProviderGeneric, URL: "/tmp/fixture.git", CredentialMode: "native_git"}, Ref: "refs/heads/main", Subpath: ".", Sensitivity: SensitivityProjectConfidential, Ingest: IngestPolicy{Include: []string{"**/*.md"}, Exclude: []string{"**/.git/**", "**/raw/**"}, MaxFileBytes: 1024 * 1024}}
}

func initCleanRepo(t *testing.T, dir string) {
	t.Helper()
	run := func(args ...string) {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "--quiet", "-b", "main")
	run("add", "--all")
	run("-c", "user.email=fixture@example.test", "-c", "user.name=Fixture", "commit", "--quiet", "-m", "fixture")
}

func TestParseValidateYAMLAndGlob(t *testing.T) {
	reg, err := ParseSourceRegistration([]byte("schema_version: 1\nid: repo-amber-willow-orbit-tiger-harbor-gentle\nslug: fixture-knowledge\nmode: loose\nremote:\n  provider: generic\n  url: /tmp/fixture.git\n  credential_mode: native_git\nref: refs/heads/main\nsubpath: .\nsensitivity: project-confidential\ningest:\n  include: [\"**/*.md\"]\n  exclude: [\"**/.git/**\"]\n  max_file_bytes: 100\n"))
	if err != nil {
		t.Fatal(err)
	}
	if !GlobMatch("**/*.md", "nested/readme.md") {
		t.Fatal("recursive glob did not match")
	}
	if GlobMatch("**/*.md", "readme.txt") {
		t.Fatal("glob matched wrong suffix")
	}
	if reg.Ingest.MaxFileBytes != 100 {
		t.Fatalf("max bytes: %d", reg.Ingest.MaxFileBytes)
	}
}

func TestIngestLooseIsReadOnlyAndDeterministic(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "nested"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "nested", "doc.md"), []byte("# Heading\n\nalpha\n## Child\n\nbeta\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "skip.txt"), []byte("skip"), 0644); err != nil {
		t.Fatal(err)
	}
	reg := fixtureReg()
	a, err := IngestSource(reg, d, "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	b, err := IngestSource(reg, d, "commit-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(a) != len(b) || len(a) != 2 {
		t.Fatalf("records: %d %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Provenance.ContentDigest != b[i].Provenance.ContentDigest {
			t.Fatal("non-deterministic ingest")
		}
	}
}

func TestCompileVerifyInstall(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "a.md"), []byte("# Alpha\nhello world\n"), 0644); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	in := SourceInput{Registration: fixtureReg(), CheckoutDir: d, Commit: "commit-a", TreeDigest: "sha256:tree"}
	b, err := CompileSources(context.Background(), []SourceInput{in}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "bundle")
	if err := b.Write(src); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(src); err != nil {
		t.Fatal(err)
	}
	dst := filepath.Join(t.TempDir(), "installed")
	if err := InstallBundle(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := VerifyBundle(dst); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(dst, "manifest.json"))
	if !strings.Contains(string(raw), b.Manifest.BundleRevision) {
		t.Fatal("revision missing")
	}
	if info, err := os.Stat(dst); err != nil || info.Mode().Perm() != privateDirMode {
		t.Fatalf("installed bundle mode: %v (%v)", info.Mode().Perm(), err)
	}
	if info, err := os.Stat(filepath.Join(dst, "manifest.json")); err != nil || info.Mode().Perm() != privateFileMode {
		t.Fatalf("installed manifest mode: %v (%v)", info.Mode().Perm(), err)
	}
	loaded, err := LoadBundle(dst)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Records) != len(b.Records) || len(loaded.Assets) != len(b.Assets) {
		t.Fatalf("loaded bundle lost records/assets: %d/%d, %d/%d", len(loaded.Records), len(b.Records), len(loaded.Assets), len(b.Assets))
	}
}

func TestLoadBundleRejectsDuplicateInvalidAndSymlinkAssets(t *testing.T) {
	d := t.TempDir()
	reg := fixtureReg()
	if err := os.WriteFile(filepath.Join(d, "a.md"), []byte("safe"), 0644); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	b, err := CompileSources(context.Background(), []SourceInput{{Registration: reg, CheckoutDir: d, Commit: "commit-a", TreeDigest: "sha256:tree"}}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assetNames := make([]string, 0, len(b.Assets))
	for name := range b.Assets {
		assetNames = append(assetNames, name)
	}
	sort.Strings(assetNames)
	if len(assetNames) == 0 {
		t.Fatal("fixture did not compile an asset")
	}
	base := filepath.Join(t.TempDir(), "base")
	if err := b.Write(base); err != nil {
		t.Fatal(err)
	}
	data, err := secureReadFile(filepath.Join(base, filepath.FromSlash(assetNames[0])))
	if err != nil {
		t.Fatal(err)
	}
	var record Record
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	// Duplicate ID is rejected even when all enclosing hashes are valid.
	secondAsset := ""
	var secondData []byte
	if len(assetNames) == 1 {
		second := Record{ID: record.ID, Title: "duplicate", Text: record.Text, SourceRepoID: record.SourceRepoID, Mode: record.Mode, Sensitivity: record.Sensitivity, Provenance: record.Provenance}
		secondAsset = "assets/duplicate.json"
		secondData, _ = json.Marshal(second)
		secondData = append(secondData, '\n')
		if err := secureWriteFile(filepath.Join(base, filepath.FromSlash(secondAsset)), secondData); err != nil {
			t.Fatal(err)
		}
	}
	var manifest Manifest
	if err := readJSONFile(filepath.Join(base, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if secondAsset != "" {
		manifest.Assets[secondAsset] = digestBytes(secondData)
	}
	var lock SourcesLock
	if err := readJSONFile(filepath.Join(base, "sources.lock.json"), &lock); err != nil {
		t.Fatal(err)
	}
	var index LexicalIndex
	if err := readJSONFile(filepath.Join(base, "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	assetMap := map[string][]byte{}
	for name := range manifest.Assets {
		assetMap[name], _ = secureReadFile(filepath.Join(base, filepath.FromSlash(name)))
	}
	manifest.BundleRevision = bundleRevision(Bundle{Manifest: manifest, SourcesLock: lock, Index: index, Assets: assetMap})
	if err := writeJSONFile(filepath.Join(base, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(base); err == nil {
		t.Fatal("duplicate record ID was accepted")
	}

	invalid := filepath.Join(t.TempDir(), "invalid")
	if err := b.Write(invalid); err != nil {
		t.Fatal(err)
	}
	data, _ = secureReadFile(filepath.Join(invalid, filepath.FromSlash(assetNames[0])))
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	record.Provenance.Path = "../escape.md"
	data, _ = json.Marshal(record)
	data = append(data, '\n')
	if err := secureWriteFile(filepath.Join(invalid, filepath.FromSlash(assetNames[0])), data); err != nil {
		t.Fatal(err)
	}
	if err := readJSONFile(filepath.Join(invalid, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Assets[assetNames[0]] = digestBytes(data)
	if err := readJSONFile(filepath.Join(invalid, "sources.lock.json"), &lock); err != nil {
		t.Fatal(err)
	}
	if err := readJSONFile(filepath.Join(invalid, "index.json"), &index); err != nil {
		t.Fatal(err)
	}
	assetMap = map[string][]byte{}
	for name := range manifest.Assets {
		assetMap[name], _ = secureReadFile(filepath.Join(invalid, filepath.FromSlash(name)))
	}
	manifest.BundleRevision = bundleRevision(Bundle{Manifest: manifest, SourcesLock: lock, Index: index, Assets: assetMap})
	if err := writeJSONFile(filepath.Join(invalid, "manifest.json"), manifest); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(invalid); err == nil {
		t.Fatal("invalid provenance was accepted")
	}

	linked := filepath.Join(t.TempDir(), "linked")
	if err := b.Write(linked); err != nil {
		t.Fatal(err)
	}
	assetPath := filepath.Join(linked, filepath.FromSlash(assetNames[0]))
	outside := filepath.Join(t.TempDir(), "outside.json")
	if err := os.Rename(assetPath, outside); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, assetPath); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadBundle(linked); err == nil {
		t.Fatal("symlink asset was followed")
	}
}

func TestAuditValidationAndStrictParsing(t *testing.T) {
	for _, remote := range []string{"https://user:pass@example.test/knowledge.git", "https://example.test/knowledge.git?token=redacted", "https://example.test/knowledge.git#fragment"} {
		reg := fixtureReg()
		reg.Remote.URL = remote
		if err := ValidateSourceRegistration(reg); err == nil {
			t.Fatalf("unsafe remote accepted")
		}
	}
	for _, subpath := range []string{".git", "docs/.git/objects"} {
		reg := fixtureReg()
		reg.Subpath = subpath
		if err := ValidateSourceRegistration(reg); err == nil {
			t.Fatalf(".git subpath accepted: %s", subpath)
		}
	}
	reg := fixtureReg()
	reg.Ingest.Include = []string{"**/.git/**"}
	if err := ValidateSourceRegistration(reg); err == nil {
		t.Fatal(".git include accepted")
	}
	reg = fixtureReg()
	reg.Remote.WebURL = "https://example.test/knowledge.git#fragment"
	if err := ValidateSourceRegistration(reg); err == nil {
		t.Fatal("unsafe web_url accepted")
	}

	validJSON := `{"schema_version":1,"id":"repo-amber-willow-orbit-tiger-harbor-gentle","slug":"fixture-knowledge","mode":"loose","remote":{"provider":"generic","url":"/tmp/fixture.git","credential_mode":"native_git"},"ref":"refs/heads/main","subpath":".","sensitivity":"project-confidential","ingest":{"include":["**/*.md"],"exclude":["**/.git/**"],"max_file_bytes":100}}`
	if _, err := ParseSourceRegistration([]byte(validJSON + "\n{}")); err == nil {
		t.Fatal("trailing JSON document accepted")
	}

	d := t.TempDir()
	manifest := `{"records":[{"id":"knowledge-secret","title":"bad","text":"AKIA1234567890ABCDEF"}]}`
	if err := os.WriteFile(filepath.Join(d, "records.json"), []byte(manifest), 0600); err != nil {
		t.Fatal(err)
	}
	structured := fixtureReg()
	structured.Mode = ModeStructured
	structured.StructuredManifest = "records.json"
	structured.Ingest = IngestPolicy{}
	if _, err := IngestSource(structured, d, "commit-a"); err == nil {
		t.Fatal("secret in structured record accepted")
	}

	for _, entry := range gitEnvironment() {
		if strings.HasPrefix(entry, "GIT_TERMINAL_PROMPT=") && entry != "GIT_TERMINAL_PROMPT=0" {
			t.Fatalf("terminal prompt was not disabled: %q", entry)
		}
	}
	if got := redactGitText("fatal: could not read https://user:pass@example.test/repo.git token=secret"); strings.Contains(got, "https://") || strings.Contains(got, "user:pass") || strings.Contains(got, "secret") {
		t.Fatalf("Git diagnostic was not redacted: %q", got)
	}
}

func TestCompileRejectsDirtyIncludedAndUntrackedFilesButAllowsExcludedDirt(t *testing.T) {
	d := t.TempDir()
	if err := os.MkdirAll(filepath.Join(d, "raw"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "doc.md"), []byte("clean"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "raw", "secret.md"), []byte("excluded"), 0600); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	reg := fixtureReg()
	compile := func() error {
		_, err := CompileSources(context.Background(), []SourceInput{{Registration: reg, CheckoutDir: d}}, CompileOptions{})
		return err
	}
	if err := compile(); err != nil {
		t.Fatalf("clean source rejected: %v", err)
	}
	if err := os.WriteFile(filepath.Join(d, "doc.md"), []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := compile(); err == nil {
		t.Fatal("modified included file was accepted")
	}
	if err := os.WriteFile(filepath.Join(d, "doc.md"), []byte("clean"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "new.md"), []byte("untracked"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := compile(); err == nil {
		t.Fatal("untracked included file was accepted")
	}
	if err := os.Remove(filepath.Join(d, "new.md")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(d, "raw", "secret.md"), []byte("excluded modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := compile(); err != nil {
		t.Fatalf("excluded dirty file rejected: %v", err)
	}
}

func TestVerifyRejectsIncompatibleReaderAndRequiredFeatures(t *testing.T) {
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "doc.md"), []byte("compatible"), 0600); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	b, err := CompileSources(context.Background(), []SourceInput{{Registration: fixtureReg(), CheckoutDir: d}}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rewrite := func(t *testing.T, dir string, mutate func(*Manifest)) {
		t.Helper()
		var manifest Manifest
		if err := readJSONFile(filepath.Join(dir, "manifest.json"), &manifest); err != nil {
			t.Fatal(err)
		}
		mutate(&manifest)
		var lock SourcesLock
		if err := readJSONFile(filepath.Join(dir, "sources.lock.json"), &lock); err != nil {
			t.Fatal(err)
		}
		var index LexicalIndex
		if err := readJSONFile(filepath.Join(dir, "index.json"), &index); err != nil {
			t.Fatal(err)
		}
		assets := map[string][]byte{}
		for name := range manifest.Assets {
			assets[name], _ = secureReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		}
		manifest.BundleRevision = bundleRevision(Bundle{Manifest: manifest, SourcesLock: lock, Index: index, Assets: assets})
		if err := writeJSONFile(filepath.Join(dir, "manifest.json"), manifest); err != nil {
			t.Fatal(err)
		}
	}
	readerDir := filepath.Join(t.TempDir(), "reader")
	if err := b.Write(readerDir); err != nil {
		t.Fatal(err)
	}
	rewrite(t, readerDir, func(manifest *Manifest) { manifest.MinimumReader = "9.0.0" })
	if _, err := VerifyBundle(readerDir); err == nil || !strings.Contains(err.Error(), "requires reader") {
		t.Fatalf("incompatible minimum_reader was not rejected: %v", err)
	}
	featureDir := filepath.Join(t.TempDir(), "feature")
	if err := b.Write(featureDir); err != nil {
		t.Fatal(err)
	}
	rewrite(t, featureDir, func(manifest *Manifest) { manifest.Features = append(manifest.Features, "future_required_feature") })
	if _, err := VerifyBundle(featureDir); err == nil || !strings.Contains(err.Error(), "unsupported feature") {
		t.Fatalf("unsupported required feature was not rejected: %v", err)
	}
}

func TestBundleRejectsSymlinkPathComponents(t *testing.T) {
	d := t.TempDir()
	reg := fixtureReg()
	if err := os.WriteFile(filepath.Join(d, "a.md"), []byte("safe"), 0644); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	b, err := CompileSources(context.Background(), []SourceInput{{Registration: reg, CheckoutDir: d, Commit: "commit-a", TreeDigest: "sha256:tree"}}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	realDir := filepath.Join(t.TempDir(), "real")
	if err := os.Mkdir(realDir, 0700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(t.TempDir(), "link-parent")
	if err := os.Symlink(realDir, linkParent); err != nil {
		t.Fatal(err)
	}
	if err := b.Write(filepath.Join(linkParent, "bundle")); err == nil {
		t.Fatal("bundle write followed symlink parent")
	}
	checkoutLink := filepath.Join(t.TempDir(), "checkout-link")
	if err := os.Symlink(d, checkoutLink); err != nil {
		t.Fatal(err)
	}
	if _, err := IngestSource(reg, checkoutLink, "commit-a"); err == nil {
		t.Fatal("ingest followed symlink checkout")
	}
}

func TestInstallRejectsSymlinkDestinationAndUnrelatedBackup(t *testing.T) {
	d := t.TempDir()
	reg := fixtureReg()
	if err := os.WriteFile(filepath.Join(d, "a.md"), []byte("safe"), 0644); err != nil {
		t.Fatal(err)
	}
	initCleanRepo(t, d)
	b, err := CompileSources(context.Background(), []SourceInput{{Registration: reg, CheckoutDir: d, Commit: "commit-a", TreeDigest: "sha256:tree"}}, CompileOptions{})
	if err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(t.TempDir(), "bundle")
	if err := b.Write(src); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "target")
	targetReal := filepath.Join(t.TempDir(), "target-real")
	if err := os.Mkdir(targetReal, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(targetReal, target); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundle(src, target); err == nil {
		t.Fatal("install followed symlink destination")
	}
	if _, err := os.Stat(filepath.Join(targetReal, "manifest.json")); !os.IsNotExist(err) {
		t.Fatal("symlink target was modified")
	}
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	backup := target + ".previous-tmp"
	if err := os.WriteFile(backup, []byte("unrelated"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := InstallBundle(src, target); err == nil {
		t.Fatal("install overwrote unrelated backup")
	}
	if got, err := os.ReadFile(backup); err != nil || string(got) != "unrelated" {
		t.Fatalf("backup changed: %q (%v)", got, err)
	}
}
