package knowledge

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

var (
	privateKeyRE = regexp.MustCompile(`-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----`)
	cloudKeyRE   = regexp.MustCompile(`(?:AKIA[0-9A-Z]{16}|gh[pousr]_[A-Za-z0-9_]{20,}|xox[baprs]-[A-Za-z0-9-]{16,})`)
)

// IngestSource reads a previously synced checkout. It intentionally takes a
// directory rather than a remote and therefore cannot fetch or write Git
// state. The returned records are sorted by path, heading, and chunk index.
func IngestSource(reg SourceRegistration, checkoutDir, sourceCommit string) ([]Record, error) {
	if err := ValidateSourceRegistration(reg); err != nil {
		return nil, err
	}
	root := checkoutDir
	if reg.Subpath != "." {
		root = filepath.Join(checkoutDir, filepath.FromSlash(reg.Subpath))
	}
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := rejectSymlinkComponents(rootAbs, false); err != nil {
		return nil, err
	}
	if !pathWithin(rootAbs, checkoutDir) {
		return nil, errors.New("source subpath escapes checkout")
	}
	if reg.Mode == ModeStructured {
		return ingestStructured(reg, rootAbs, sourceCommit)
	}
	records, err := ingestLoose(reg, rootAbs, sourceCommit)
	if err != nil || reg.Mode != ModeHybrid {
		return records, err
	}
	if reg.Overlay.Kind == "in_repo" {
		overlayReg := reg
		overlayReg.Mode = ModeStructured
		overlayReg.StructuredManifest = reg.Overlay.Path
		overlay, overlayErr := ingestStructured(overlayReg, rootAbs, sourceCommit)
		if overlayErr != nil {
			return nil, fmt.Errorf("hybrid overlay: %w", overlayErr)
		}
		records = append(records, overlay...)
		sort.SliceStable(records, func(i, j int) bool {
			if records[i].ID != records[j].ID {
				return records[i].ID < records[j].ID
			}
			return records[i].Provenance.Path < records[j].Provenance.Path
		})
	}
	return records, nil
}

// IngestLoose is a convenience wrapper for callers onboarding an existing
// corpus without migration. It does not inspect or modify Git metadata.
func IngestLoose(reg SourceRegistration, checkoutDir, sourceCommit string) ([]Record, error) {
	if reg.Mode == ModeStructured {
		return nil, errors.New("IngestLoose requires a loose or hybrid registration")
	}
	return IngestSource(reg, checkoutDir, sourceCommit)
}

// Ingest is retained as a short, discoverable alias for IngestSource.
func Ingest(reg SourceRegistration, checkoutDir, sourceCommit string) ([]Record, error) {
	return IngestSource(reg, checkoutDir, sourceCommit)
}

func ingestLoose(reg SourceRegistration, root, commit string) ([]Record, error) {
	max := reg.Ingest.MaxFileBytes
	if max <= 0 {
		max = 1 << 20
	}
	chunkLimit := 8192
	chunking := reg.Ingest.Chunking
	if chunking == "" {
		chunking = "headings_and_bytes"
	}
	var out []Record
	err := filepath.WalkDir(root, func(filename string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if filename == root {
			return nil
		}
		rel, err := filepath.Rel(root, filename)
		if err != nil {
			return err
		}
		relSlash := filepath.ToSlash(rel)
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink rejected: %s", relSlash)
		}
		if d.IsDir() {
			if relSlash == ".git" || strings.HasPrefix(relSlash, ".git/") {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if reg.Mode == ModeHybrid && reg.Overlay.Kind == "in_repo" && filepath.ToSlash(reg.Overlay.Path) == relSlash {
			return nil
		}
		if !matchesAny(reg.Ingest.Include, relSlash) || matchesAny(reg.Ingest.Exclude, relSlash) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() > max {
			return fmt.Errorf("file %s exceeds max_file_bytes", relSlash)
		}
		b, err := os.ReadFile(filename)
		if err != nil {
			return err
		}
		if !utf8.Valid(b) {
			return fmt.Errorf("file %s is not UTF-8", relSlash)
		}
		if hasSecret(relSlash, b) {
			return fmt.Errorf("secret-policy violation in %s", relSlash)
		}
		sections := []textSection{{Title: strings.TrimSuffix(filepath.Base(relSlash), filepath.Ext(relSlash)), Text: string(b), StartLine: 1, EndLine: lineCount(string(b))}}
		if chunking == "headings_and_bytes" {
			sections = headingSections(string(b))
		}
		for _, section := range sections {
			chunks := boundedChunks(section, chunkLimit)
			for _, c := range chunks {
				digest := digestBytes([]byte(c.Text))
				id := deterministicKnowledgeID(relSlash + "\x00" + strconvI(c.StartLine) + "\x00" + digest)
				scope := cloneScope(reg.DefaultScope)
				out = append(out, Record{ID: id, Title: c.Title, Text: c.Text, SourceRepoID: reg.ID, SourceSlug: reg.Slug, Mode: reg.Mode, Sensitivity: reg.Sensitivity, Scope: scope, Provenance: Provenance{SourceRepoID: reg.ID, SourceCommit: commit, Path: relSlash, StartLine: c.StartLine, EndLine: c.EndLine, ContentDigest: digest}})
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Provenance.Path != b.Provenance.Path {
			return a.Provenance.Path < b.Provenance.Path
		}
		if a.Provenance.StartLine != b.Provenance.StartLine {
			return a.Provenance.StartLine < b.Provenance.StartLine
		}
		return a.ID < b.ID
	})
	return out, nil
}

func ingestStructured(reg SourceRegistration, root, commit string) ([]Record, error) {
	manifest := filepath.Join(root, filepath.FromSlash(reg.StructuredManifest))
	if !pathWithin(manifest, root) {
		return nil, errors.New("structured manifest escapes source subpath")
	}
	b, err := os.ReadFile(manifest)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, errors.New("structured manifest is not UTF-8")
	}
	if hasSecret("", b) {
		return nil, errors.New("secret-policy violation in structured manifest")
	}
	raw, err := parseData(b)
	if err != nil {
		return nil, err
	}
	var values []any
	switch x := raw.(type) {
	case []any:
		values = x
	case map[string]any:
		if v, ok := x["records"].([]any); ok {
			values = v
		} else {
			return nil, errors.New("structured manifest must contain records")
		}
	default:
		return nil, errors.New("structured manifest must be an array or records object")
	}
	out := make([]Record, 0, len(values))
	ids := map[string]bool{}
	for idx, v := range values {
		m, ok := v.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("structured record %d is not an object", idx)
		}
		rb, _ := json.Marshal(m)
		var r Record
		if err := json.Unmarshal(rb, &r); err != nil {
			return nil, fmt.Errorf("structured record %d: %w", idx, err)
		}
		if r.ID == "" {
			r.ID = deterministicKnowledgeID(string(rb))
		}
		if ids[r.ID] {
			return nil, fmt.Errorf("duplicate structured record id %s", r.ID)
		}
		ids[r.ID] = true
		if r.Text == "" {
			r.Text = mString(m, "content")
		}
		if r.Title == "" {
			r.Title = mString(m, "title")
		}
		if r.Text == "" {
			return nil, fmt.Errorf("structured record %s has no text", r.ID)
		}
		if !utf8.ValidString(r.Text) {
			return nil, fmt.Errorf("structured record %s is not UTF-8", r.ID)
		}
		if hasSecret("", []byte(r.Text)) {
			return nil, fmt.Errorf("secret-policy violation in structured record %s", r.ID)
		}
		r.SourceRepoID = reg.ID
		r.SourceSlug = reg.Slug
		r.Mode = reg.Mode
		if r.Sensitivity != "" && sensitivityRank(r.Sensitivity) < 0 {
			return nil, fmt.Errorf("structured record %s has invalid sensitivity", r.ID)
		}
		if r.Sensitivity == "" || sensitivityRank(r.Sensitivity) < sensitivityRank(reg.Sensitivity) {
			r.Sensitivity = reg.Sensitivity
		}
		if r.ReviewedAt != "" {
			if _, dateErr := parseDate(r.ReviewedAt); dateErr != nil {
				return nil, fmt.Errorf("structured record %s reviewed_at: %w", r.ID, dateErr)
			}
		}
		if r.ExpiresAt != "" && r.ExpiresAt != "null" {
			if _, dateErr := parseDate(r.ExpiresAt); dateErr != nil {
				return nil, fmt.Errorf("structured record %s expires_at: %w", r.ID, dateErr)
			}
		}
		if len(r.Scope) == 0 {
			r.Scope = cloneScope(reg.DefaultScope)
		}
		r.Provenance = Provenance{SourceRepoID: reg.ID, SourceCommit: commit, Path: filepath.ToSlash(filepath.Join(reg.Subpath, reg.StructuredManifest)), StartLine: 1, EndLine: lineCount(string(b)), ContentDigest: digestBytes([]byte(r.Text))}
		out = append(out, r)
	}
	for _, r := range out {
		for _, supersededID := range r.Supersedes {
			found := false
			for _, candidate := range out {
				if candidate.ID == supersededID {
					found = true
					break
				}
			}
			if !found {
				return nil, fmt.Errorf("structured record %s supersedes unknown record %s", r.ID, supersededID)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

func parseData(b []byte) (any, error) {
	raw, jsonErr := decodeJSONSingle(b)
	if jsonErr == nil {
		return raw, nil
	}
	if looksLikeJSON(b) {
		return nil, jsonErr
	}
	return parseYAML(string(b))
}

func sensitivityRank(s Sensitivity) int {
	switch s {
	case SensitivityPublic:
		return 0
	case SensitivityFleetInternal:
		return 1
	case SensitivityOperatorPrivate:
		return 2
	case SensitivityProjectConfidential:
		return 3
	default:
		return -1
	}
}

func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Parse("2006-01-02", s)
}
func mString(m map[string]any, k string) string {
	if s, ok := m[k].(string); ok {
		return s
	}
	return ""
}
func cloneScope(in map[string][]string) map[string][]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string][]string, len(in))
	for k, v := range in {
		out[k] = append([]string(nil), v...)
	}
	return out
}
func pathWithin(child, root string) bool {
	ca, e1 := filepath.Abs(child)
	ra, e2 := filepath.Abs(root)
	if e1 != nil || e2 != nil {
		return false
	}
	rel, e := filepath.Rel(ra, ca)
	return e == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func hasSecret(path string, b []byte) bool {
	lower := strings.ToLower(path)
	for _, needle := range []string{".env", "id_rsa", "private.key", "credentials", "secret"} {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return privateKeyRE.Match(b) || cloudKeyRE.Match(b)
}
func lineCount(s string) int {
	if s == "" {
		return 1
	}
	return strings.Count(s, "\n") + 1
}
func strconvI(v int) string { return fmt.Sprintf("%d", v) }

type textSection struct {
	Title, Text        string
	StartLine, EndLine int
}

// Chunk is the public form of a deterministic text chunk. StartLine and
// EndLine are one-based and inclusive.
type Chunk struct {
	Title     string
	Text      string
	StartLine int
	EndLine   int
}

// ChunkText splits UTF-8 text by headings and a byte bound. The mode is
// "headings_and_bytes" (the default) or "document". A maxBytes <= 0 uses the
// stable 8192-byte default.
func ChunkText(text string, maxBytes int, mode string) []Chunk {
	if mode == "" {
		mode = "headings_and_bytes"
	}
	sections := []textSection{{Text: text, StartLine: 1, EndLine: lineCount(text)}}
	if mode == "headings_and_bytes" {
		sections = headingSections(text)
	}
	out := []Chunk{}
	for _, section := range sections {
		for _, chunk := range boundedChunks(section, maxBytes) {
			out = append(out, Chunk{Title: chunk.Title, Text: chunk.Text, StartLine: chunk.StartLine, EndLine: chunk.EndLine})
		}
	}
	return out
}

func headingSections(s string) []textSection {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) == 0 {
		lines = []string{""}
	}
	type marker struct {
		i, line int
		title   string
	}
	markers := []marker{}
	for i, l := range lines {
		trim := strings.TrimSpace(strings.TrimSuffix(l, "\n"))
		if strings.HasPrefix(trim, "#") && len(trim) > 1 {
			n := 0
			for n < len(trim) && trim[n] == '#' {
				n++
			}
			if n < len(trim) && trim[n] == ' ' {
				markers = append(markers, marker{i, n + 1, strings.TrimSpace(trim[n:])})
			}
		}
	}
	if len(markers) == 0 {
		return []textSection{{Title: "", Text: s, StartLine: 1, EndLine: lineCount(s)}}
	}
	out := []textSection{}
	if markers[0].i > 0 {
		prefix := strings.Join(lines[:markers[0].i], "")
		if strings.TrimSpace(prefix) != "" {
			out = append(out, textSection{Text: prefix, StartLine: 1, EndLine: markers[0].line - 1})
		}
	}
	for x, m := range markers {
		end := len(lines)
		if x+1 < len(markers) {
			end = markers[x+1].i
		}
		txt := strings.Join(lines[m.i:end], "")
		out = append(out, textSection{Title: m.title, Text: txt, StartLine: m.line, EndLine: m.line + lineCount(txt) - 1})
	}
	return out
}

func boundedChunks(section textSection, max int) []textSection {
	if max <= 0 {
		max = 8192
	}
	if len([]byte(section.Text)) <= max {
		return []textSection{section}
	}
	lines := strings.SplitAfter(section.Text, "\n")
	out := []textSection{}
	buf := ""
	start := section.StartLine
	line := section.StartLine
	flush := func() {
		if buf != "" {
			out = append(out, textSection{Title: section.Title, Text: buf, StartLine: start, EndLine: line - 1})
			buf = ""
			start = line
		}
	}
	for _, part := range lines {
		partBytes := []byte(part)
		if len(partBytes) > max {
			flush()
			for len(partBytes) > max {
				cut := max
				for cut > 0 && (partBytes[cut]&0xc0) == 0x80 {
					cut--
				}
				if cut <= 0 {
					cut = max
				}
				piece := string(partBytes[:cut])
				out = append(out, textSection{Title: section.Title, Text: piece, StartLine: line, EndLine: line})
				partBytes = partBytes[cut:]
			}
			if len(partBytes) > 0 {
				buf = string(partBytes)
				line++
			}
			continue
		}
		if len([]byte(buf))+len(partBytes) > max {
			flush()
		}
		buf += string(partBytes)
		line++
	}
	flush()
	return out
}

// GlobMatch implements repository globs with the common ** recursive segment
// extension (path.Match treats ** as one segment). Paths are slash-normalized.
func GlobMatch(pattern, name string) bool { return globMatch(pattern, name) }
func matchesAny(globs []string, name string) bool {
	for _, g := range globs {
		if globMatch(g, name) {
			return true
		}
	}
	return false
}
func globMatch(pattern, name string) bool {
	pattern = strings.TrimPrefix(filepath.ToSlash(pattern), "./")
	name = filepath.ToSlash(name)
	var rec func(int, int) bool
	rec = func(i, j int) bool {
		for i < len(pattern) {
			if pattern[i] == '*' {
				if i+1 < len(pattern) && pattern[i+1] == '*' {
					for i+1 < len(pattern) && pattern[i+1] == '*' {
						i++
					}
					if i+1 < len(pattern) && pattern[i+1] == '/' {
						i++
						for k := j; k <= len(name); k++ {
							if rec(i+1, k) {
								return true
							}
						}
						return false
					}
					for k := j; k <= len(name); k++ {
						if rec(i+1, k) {
							return true
						}
					}
					return false
				}
				if j >= len(name) || name[j] == '/' {
					return false
				}
				k := j
				for k < len(name) && name[k] != '/' {
					if rec(i+1, k+1) {
						return true
					}
					k++
				}
				return false
			}
			if pattern[i] == '?' {
				if j >= len(name) || name[j] == '/' {
					return false
				}
				i++
				j++
				continue
			}
			if j >= len(name) || pattern[i] != name[j] {
				return false
			}
			i++
			j++
		}
		return j == len(name)
	}
	return rec(0, 0)
}

func digestBytes(b []byte) string { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }
func shortDigest(s string) string {
	d := digestBytes([]byte(s))
	return strings.TrimPrefix(d, "sha256:")[:24]
}

func deterministicKnowledgeID(seed string) string {
	h := sha256.Sum256([]byte(seed))
	payload := binary.BigEndian.Uint64(h[:]) & ((uint64(1) << 60) - 1)
	if id, err := ids.FromPayload(ids.TypeKnowledge, payload); err == nil {
		return id.String()
	}
	return "knowledge-" + shortDigest(seed)
}
