package knowledge

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

type SourceInput struct {
	Registration SourceRegistration
	CheckoutDir  string
	Commit       string
	TreeDigest   string
}

type Compiler struct{ Git GitRunner }

func CompileSources(ctx context.Context, inputs []SourceInput, opts CompileOptions) (Bundle, error) {
	return Compiler{Git: NativeGit{}}.Compile(ctx, inputs, opts)
}

// Compile is the package-level spelling used by the publisher command.
func Compile(ctx context.Context, inputs []SourceInput, opts CompileOptions) (Bundle, error) {
	return CompileSources(ctx, inputs, opts)
}

func (c Compiler) Compile(ctx context.Context, inputs []SourceInput, opts CompileOptions) (Bundle, error) {
	if len(inputs) == 0 {
		return Bundle{}, errors.New("at least one knowledge source is required")
	}
	git := c.Git
	if git == nil {
		git = NativeGit{}
	}
	sort.SliceStable(inputs, func(i, j int) bool { return inputs[i].Registration.ID < inputs[j].Registration.ID })
	seen := map[string]bool{}
	records := []Record{}
	bySource := map[string][]Record{}
	regByID := map[string]SourceRegistration{}
	revisions := []SourceRevision{}
	for _, in := range inputs {
		reg := in.Registration
		if err := ValidateSourceRegistration(reg); err != nil {
			return Bundle{}, err
		}
		if seen[reg.ID] {
			return Bundle{}, fmt.Errorf("duplicate source id %s", reg.ID)
		}
		seen[reg.ID] = true
		regByID[reg.ID] = reg
		statusBefore, err := CheckScopedClean(ctx, git, reg, in.CheckoutDir)
		if err != nil {
			return Bundle{}, fmt.Errorf("source %s: %w", reg.ID, err)
		}
		commit, tree := in.Commit, in.TreeDigest
		if commit == "" || tree == "" {
			var err error
			commit, tree, err = ResolveRevision(ctx, git, in.CheckoutDir)
			if err != nil {
				return Bundle{}, fmt.Errorf("source %s revision: %w", reg.ID, err)
			}
		}
		rs, err := IngestSource(reg, in.CheckoutDir, commit)
		if err != nil {
			return Bundle{}, fmt.Errorf("source %s ingest: %w", reg.ID, err)
		}
		statusAfter, err := CheckScopedClean(ctx, git, reg, in.CheckoutDir)
		if err != nil {
			return Bundle{}, fmt.Errorf("source %s: %w", reg.ID, err)
		}
		if statusBefore != statusAfter {
			return Bundle{}, fmt.Errorf("source %s working tree changed during ingestion", reg.ID)
		}
		records = append(records, rs...)
		bySource[reg.ID] = rs
		contentParts := make([]string, 0, len(rs))
		for _, r := range rs {
			contentParts = append(contentParts, r.Provenance.Path+"\x00"+r.Provenance.ContentDigest)
		}
		contentDigest := digestBytes([]byte(strings.Join(contentParts, "\n")))
		revisions = append(revisions, SourceRevision{ID: reg.ID, Slug: reg.Slug, Provider: reg.Remote.Provider, RemoteURL: reg.Remote.URL, Ref: reg.Ref, Commit: commit, TreeDigest: tree, ContentDigest: contentDigest, Subpath: reg.Subpath, IngestDigest: registrationDigest(reg)})
	}
	// External overlays are assembled after both source revisions are resolved.
	// The overlay source remains independently authored; these records are a
	// deterministic view bound to the hybrid registration.
	for _, in := range inputs {
		reg := in.Registration
		if reg.Mode != ModeHybrid || reg.Overlay.Kind != "external" {
			continue
		}
		overlaySource, ok := regByID[reg.Overlay.SourceRepoID]
		if !ok {
			return Bundle{}, fmt.Errorf("source %s overlay references uncompiled source %s", reg.ID, reg.Overlay.SourceRepoID)
		}
		wantPath := filepath.ToSlash(filepath.Join(overlaySource.Subpath, reg.Overlay.Path))
		for _, original := range bySource[overlaySource.ID] {
			if original.Provenance.Path != wantPath && original.Provenance.Path != filepath.ToSlash(reg.Overlay.Path) {
				continue
			}
			clone := original
			clone.SourceRepoID = reg.ID
			clone.SourceSlug = reg.Slug
			clone.Mode = reg.Mode
			clone.Sensitivity = reg.Sensitivity
			clone.Provenance.SourceRepoID = reg.ID
			records = append(records, clone)
		}
	}
	sort.SliceStable(records, func(i, j int) bool {
		if records[i].ID != records[j].ID {
			return records[i].ID < records[j].ID
		}
		return records[i].Provenance.Path < records[j].Provenance.Path
	})
	indexRecords := make([]Record, 0, len(records))
	for _, r := range records {
		reg := regByID[r.SourceRepoID]
		if reg.Mode != ModeStructured && reg.Ingest.Index == "metadata_only" {
			continue
		}
		indexRecords = append(indexRecords, r)
	}
	index := BuildLexicalIndex(indexRecords)
	assets := map[string][]byte{}
	for _, r := range records {
		assetName := "assets/" + safeAssetName(r.ID) + ".json"
		b, _ := json.Marshal(r)
		assets[assetName] = append(b, '\n')
	}
	lock := SourcesLock{SchemaVersion: 1, Sources: revisions}
	readerVersion := opts.ReaderVersion
	if readerVersion == "" {
		readerVersion = CurrentReaderVersion
	}
	manifest := Manifest{SchemaVersion: 1, MinimumReader: readerVersion, Canonicalization: "json-utf8-sha256-v1", WordListDigest: ids.WordListDigest(), Sources: revisions, Assets: map[string]string{}, Features: []string{"knowledge_records", "lexical_index"}}
	for n, b := range assets {
		manifest.Assets[n] = digestBytes(b)
	}
	bundle := Bundle{Manifest: manifest, SourcesLock: lock, Records: records, Index: index, Assets: assets, CreatedAt: opts.CreatedAt}
	bundle.Manifest.BundleRevision = bundleRevision(bundle)
	return bundle, nil
}

func BuildLexicalIndex(records []Record) LexicalIndex {
	idx := LexicalIndex{Tokens: map[string][]string{}}
	for _, r := range records {
		seen := map[string]bool{}
		for _, t := range tokenize(r.Title + "\n" + r.Text) {
			if !seen[t] {
				idx.Tokens[t] = append(idx.Tokens[t], r.ID)
				seen[t] = true
			}
		}
	}
	for t, ids := range idx.Tokens {
		sort.Strings(ids)
		idx.Tokens[t] = uniqueStrings(ids)
	}
	return idx
}
func tokenize(s string) []string {
	out := []string{}
	var buf strings.Builder
	flush := func() {
		if buf.Len() > 0 {
			out = append(out, strings.ToLower(buf.String()))
			buf.Reset()
		}
	}
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			buf.WriteRune(unicode.ToLower(r))
		} else {
			flush()
		}
	}
	flush()
	return out
}
func uniqueStrings(xs []string) []string {
	if len(xs) < 2 {
		return xs
	}
	out := xs[:1]
	for _, x := range xs[1:] {
		if x != out[len(out)-1] {
			out = append(out, x)
		}
	}
	return out
}
func safeAssetName(id string) string {
	var b strings.Builder
	for _, r := range id {
		if unicode.IsLetter(r) || unicode.IsNumber(r) || r == '-' || r == '_' || r == '.' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "record"
	}
	return b.String()
}

func bundleRevision(b Bundle) string {
	type payload struct {
		Manifest     Manifest          `json:"manifest"`
		Lock         SourcesLock       `json:"sources_lock"`
		Index        LexicalIndex      `json:"index"`
		AssetDigests map[string]string `json:"asset_digests"`
	}
	m := b.Manifest
	m.BundleRevision = ""
	p := payload{Manifest: m, Lock: b.SourcesLock, Index: b.Index, AssetDigests: map[string]string{}}
	for n, v := range b.Assets {
		p.AssetDigests[n] = digestBytes(v)
	}
	raw, _ := json.Marshal(p)
	return digestBytes(raw)
}

func (b Bundle) Write(dir string) error { return writeBundle(dir, b) }

func writeBundle(dir string, b Bundle) error {
	if dir == "" {
		return errors.New("bundle directory is required")
	}
	if err := secureMkdirAll(dir); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "manifest.json"), b.Manifest); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "sources.lock.json"), b.SourcesLock); err != nil {
		return err
	}
	if err := writeJSONFile(filepath.Join(dir, "index.json"), b.Index); err != nil {
		return err
	}
	for name, data := range b.Assets {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if !pathWithin(p, dir) {
			return fmt.Errorf("asset path escapes bundle: %s", name)
		}
		if err := secureMkdirAll(filepath.Dir(p)); err != nil {
			return err
		}
		if err := secureWriteFile(p, data); err != nil {
			return err
		}
	}
	return nil
}

func writeJSONFile(filename string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return secureWriteFile(filename, b)
}

// VerifyBundle validates every manifest asset and recomputes the immutable
// revision before a caller can consume it.
func VerifyBundle(dir string) (Manifest, error) {
	if err := rejectSymlinkComponents(dir, false); err != nil {
		return Manifest{}, err
	}
	var m Manifest
	if err := readJSONFile(filepath.Join(dir, "manifest.json"), &m); err != nil {
		return Manifest{}, err
	}
	if m.SchemaVersion != 1 {
		return Manifest{}, errors.New("unsupported bundle schema")
	}
	if err := validateManifestCompatibility(m); err != nil {
		return Manifest{}, err
	}
	if m.WordListDigest != "" && m.WordListDigest != ids.WordListDigest() {
		return Manifest{}, errors.New("bundle word list is incompatible")
	}
	for name, want := range m.Assets {
		if strings.Contains(name, "..") || filepath.IsAbs(name) {
			return Manifest{}, fmt.Errorf("invalid asset name %s", name)
		}
		b, err := secureReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		if err != nil {
			return Manifest{}, fmt.Errorf("asset %s: %w", name, err)
		}
		if got := digestBytes(b); got != want {
			return Manifest{}, fmt.Errorf("asset %s digest mismatch", name)
		}
	}
	var lock SourcesLock
	if err := readJSONFile(filepath.Join(dir, "sources.lock.json"), &lock); err != nil {
		return Manifest{}, err
	}
	var idx LexicalIndex
	if err := readJSONFile(filepath.Join(dir, "index.json"), &idx); err != nil {
		return Manifest{}, err
	}
	if !sameJSON(m.Sources, lock.Sources) {
		return Manifest{}, errors.New("manifest and sources lock differ")
	}
	assetMap := map[string][]byte{}
	for name := range m.Assets {
		b, _ := secureReadFile(filepath.Join(dir, filepath.FromSlash(name)))
		assetMap[name] = b
	}
	actual := bundleRevision(Bundle{Manifest: m, SourcesLock: lock, Index: idx, Assets: assetMap})
	if actual != m.BundleRevision {
		return Manifest{}, errors.New("bundle revision mismatch")
	}
	return m, nil
}

func Verify(dir string) (Manifest, error) { return VerifyBundle(dir) }
func sameJSON(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
func readJSONFile(name string, v any) error {
	data, err := secureReadFile(name)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, v)
}

// InstallBundle validates a complete bundle and then replaces destination in
// one rename. The previous valid directory is restored if replacement fails.
func InstallBundle(bundleDir, destination string) error {
	if _, err := VerifyBundle(bundleDir); err != nil {
		return fmt.Errorf("verify bundle: %w", err)
	}
	parent := filepath.Dir(destination)
	if err := rejectSymlinkComponents(parent, true); err != nil {
		return err
	}
	if err := os.MkdirAll(parent, privateDirMode); err != nil {
		return err
	}
	if err := rejectSymlinkComponents(destination, true); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".agentctl-bundle-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := copyTree(bundleDir, tmp); err != nil {
		return err
	}
	if _, err := VerifyBundle(tmp); err != nil {
		return fmt.Errorf("verify staged bundle: %w", err)
	}
	backup := ""
	if info, err := os.Lstat(destination); err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink destination rejected: %s", destination)
		}
		backup = destination + ".previous-tmp"
		if err := securePathAbsent(backup); err != nil {
			return fmt.Errorf("backup path is not available: %w", err)
		}
		if err := os.Rename(destination, backup); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		if backup != "" {
			if _, statErr := os.Lstat(destination); os.IsNotExist(statErr) {
				_ = os.Rename(backup, destination)
			}
		}
		return err
	}
	if backup != "" {
		if info, statErr := os.Lstat(backup); statErr == nil && info.Mode().IsDir() && info.Mode()&os.ModeSymlink == 0 {
			_ = os.RemoveAll(backup)
		}
	}
	return nil
}
func copyTree(src, dst string) error {
	if err := rejectSymlinkComponents(src, false); err != nil {
		return err
	}
	if err := secureMkdirAll(dst); err != nil {
		return err
	}
	return filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		out := filepath.Join(dst, rel)
		if info.IsDir() {
			if err := rejectSymlinkComponents(p, false); err != nil {
				return err
			}
			return secureMkdirAll(out)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("unsupported bundle entry %s", rel)
		}
		if e := rejectSymlinkComponents(p, false); e != nil {
			return e
		}
		in, e := os.Open(p)
		if e != nil {
			return e
		}
		defer in.Close()
		f, e := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, privateFileMode)
		if e != nil {
			return e
		}
		_, e = io.Copy(f, in)
		ce := f.Close()
		if e != nil {
			return e
		}
		if ce != nil {
			return ce
		}
		return os.Chmod(out, privateFileMode)
	})
}
