package knowledge

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
)

var (
	repoIDRE = regexp.MustCompile(`^repo-[a-z]{3,10}(?:-[a-z]{3,10}){5}$`)
	slugRE   = regexp.MustCompile(`^[a-z][a-z0-9._-]{1,63}$`)
)

// ParseSourceRegistration parses JSON or the intentionally small YAML subset
// used by the registry schema. Keeping YAML decoding local avoids bringing a
// mutable parser dependency into the portable compiler.
func ParseSourceRegistration(data []byte) (SourceRegistration, error) {
	raw, jsonErr := decodeJSONSingle(data)
	if jsonErr != nil {
		if looksLikeJSON(data) {
			return SourceRegistration{}, fmt.Errorf("knowledge source: parse JSON: %w", jsonErr)
		}
		var errY error
		raw, errY = parseYAML(string(data))
		if errY != nil {
			return SourceRegistration{}, fmt.Errorf("knowledge source: parse JSON/YAML (JSON: %v): %w", jsonErr, errY)
		}
	}
	canonical, err := json.Marshal(raw)
	if err != nil {
		return SourceRegistration{}, err
	}
	var reg SourceRegistration
	if err := json.Unmarshal(canonical, &reg); err != nil {
		return SourceRegistration{}, fmt.Errorf("knowledge source: decode: %w", err)
	}
	if err := ValidateSourceRegistration(reg); err != nil {
		return SourceRegistration{}, err
	}
	return reg, nil
}

func decodeJSONSingle(data []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var raw any
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	var trailing any
	if err := dec.Decode(&trailing); err != io.EOF {
		if err == nil {
			return nil, errors.New("trailing JSON document")
		}
		return nil, fmt.Errorf("trailing JSON data: %w", err)
	}
	return raw, nil
}

func looksLikeJSON(data []byte) bool {
	trimmed := strings.TrimSpace(string(data))
	return strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[")
}

func ParseSourceRegistrationFile(filename string) (SourceRegistration, error) {
	b, err := os.ReadFile(filename)
	if err != nil {
		return SourceRegistration{}, err
	}
	return ParseSourceRegistration(b)
}

// ParseSource and ValidateSource are concise aliases used by CLI adapters.
func ParseSource(data []byte) (SourceRegistration, error) { return ParseSourceRegistration(data) }
func ValidateSource(reg SourceRegistration) error         { return ValidateSourceRegistration(reg) }

// ValidateSourceRegistration enforces the schema and the safety invariants
// which JSON Schema alone cannot express (relative subpaths, explicit globs,
// and provider URL sanity).
func ValidateSourceRegistration(reg SourceRegistration) error {
	if reg.SchemaVersion != 1 {
		return fmt.Errorf("knowledge source %q: schema_version must be 1", reg.ID)
	}
	if !repoIDRE.MatchString(reg.ID) {
		return fmt.Errorf("knowledge source %q: invalid id", reg.ID)
	}
	if !slugRE.MatchString(reg.Slug) {
		return fmt.Errorf("knowledge source %q: invalid slug", reg.ID)
	}
	if reg.Mode != ModeStructured && reg.Mode != ModeLoose && reg.Mode != ModeHybrid {
		return fmt.Errorf("knowledge source %q: invalid mode %q", reg.ID, reg.Mode)
	}
	if reg.Remote.Provider != ProviderGitHub && reg.Remote.Provider != ProviderForgejo && reg.Remote.Provider != ProviderGeneric {
		return fmt.Errorf("knowledge source %q: invalid remote.provider", reg.ID)
	}
	if strings.TrimSpace(reg.Remote.URL) == "" {
		return fmt.Errorf("knowledge source %q: remote.url is required", reg.ID)
	}
	if reg.Remote.CredentialMode != "native_git" {
		return fmt.Errorf("knowledge source %q: credential_mode must be native_git", reg.ID)
	}
	if err := validateRemoteURL(reg.Remote.URL); err != nil {
		return fmt.Errorf("knowledge source %q: %w", reg.ID, err)
	}
	if reg.Remote.WebURL != "" {
		if err := validateRemoteURL(reg.Remote.WebURL); err != nil {
			return fmt.Errorf("knowledge source %q: web_url: %w", reg.ID, err)
		}
	}
	if reg.Ref == "" || len(reg.Ref) > 512 {
		return fmt.Errorf("knowledge source %q: ref is required", reg.ID)
	}
	if strings.HasPrefix(reg.Ref, "-") || strings.ContainsAny(reg.Ref, "\x00\r\n") {
		return fmt.Errorf("knowledge source %q: invalid ref", reg.ID)
	}
	if err := validateSubpath(reg.Subpath); err != nil {
		return fmt.Errorf("knowledge source %q: %w", reg.ID, err)
	}
	switch reg.Sensitivity {
	case SensitivityPublic, SensitivityFleetInternal, SensitivityOperatorPrivate, SensitivityProjectConfidential:
	default:
		return fmt.Errorf("knowledge source %q: invalid sensitivity", reg.ID)
	}
	if reg.Mode == ModeStructured && strings.TrimSpace(reg.StructuredManifest) == "" {
		return fmt.Errorf("knowledge source %q: structured_manifest is required", reg.ID)
	}
	if reg.StructuredManifest != "" {
		if err := validateSubpath(reg.StructuredManifest); err != nil {
			return fmt.Errorf("knowledge source %q: structured_manifest: %w", reg.ID, err)
		}
	}
	if reg.Mode == ModeLoose || reg.Mode == ModeHybrid {
		if err := validateIngest(reg.Ingest); err != nil {
			return fmt.Errorf("knowledge source %q: %w", reg.ID, err)
		}
	}
	if reg.Mode == ModeHybrid {
		if reg.Overlay.Kind != "in_repo" && reg.Overlay.Kind != "external" {
			return fmt.Errorf("knowledge source %q: overlay.kind must be in_repo or external", reg.ID)
		}
		if err := validateSubpath(reg.Overlay.Path); err != nil {
			return fmt.Errorf("knowledge source %q: overlay.path: %w", reg.ID, err)
		}
		if reg.Overlay.Kind == "external" && !repoIDRE.MatchString(reg.Overlay.SourceRepoID) {
			return fmt.Errorf("knowledge source %q: overlay.source_repo_id is required", reg.ID)
		}
	}
	return nil
}

func validateIngest(policy IngestPolicy) error {
	if len(policy.Include) == 0 {
		return errors.New("ingest.include must contain at least one glob (ingestion is deny-by-default)")
	}
	if len(policy.Exclude) == 0 {
		return errors.New("ingest.exclude must contain at least one glob")
	}
	if policy.MaxFileBytes < 1 || policy.MaxFileBytes > 16*1024*1024 {
		return errors.New("ingest.max_file_bytes is outside 1..16777216")
	}
	if policy.Encoding != "" && policy.Encoding != "utf-8" {
		return errors.New("ingest.encoding must be utf-8")
	}
	if policy.Chunking != "" && policy.Chunking != "document" && policy.Chunking != "headings_and_bytes" {
		return errors.New("ingest.chunking is invalid")
	}
	if policy.Index != "" && policy.Index != "lexical" && policy.Index != "metadata_only" {
		return errors.New("ingest.index is invalid")
	}
	for listIndex, list := range [][]string{policy.Include, policy.Exclude} {
		seen := map[string]bool{}
		for _, g := range list {
			if strings.TrimSpace(g) == "" || strings.HasPrefix(g, "/") || strings.Contains(g, "\\") || strings.Contains(g, "\x00") {
				return errors.New("ingest globs must be non-empty relative slash paths")
			}
			if listIndex == 0 && globContainsGitDir(g) {
				return errors.New("ingest.include cannot target .git")
			}
			if seen[g] {
				return fmt.Errorf("duplicate ingest glob %q", g)
			}
			seen[g] = true
		}
	}
	return nil
}

func validateSubpath(p string) error {
	if p == "" || len(p) > 1024 {
		return errors.New("subpath is required")
	}
	clean := path.Clean(strings.ReplaceAll(p, "\\", "/"))
	if clean == ".." || strings.HasPrefix(clean, "../") || globContainsGitDir(clean) || strings.HasPrefix(p, "/") || strings.Contains(p, "\x00") {
		return errors.New("subpath must stay inside checkout")
	}
	return nil
}

func validateRemoteURL(raw string) error {
	// Local paths are useful for offline fixture repos and are accepted by Git.
	if !strings.Contains(raw, "://") {
		if strings.HasPrefix(raw, "-") {
			return errors.New("remote.url is invalid")
		}
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" {
		return errors.New("remote.url is invalid")
	}
	if u.Scheme != "ssh" && u.Scheme != "https" && u.Scheme != "http" && u.Scheme != "git" {
		return fmt.Errorf("remote.url uses unsupported scheme %q", u.Scheme)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("remote.url must not contain userinfo, query, or fragment")
	}
	return nil
}

func globContainsGitDir(glob string) bool {
	for _, component := range strings.Split(strings.TrimPrefix(glob, "./"), "/") {
		if component == ".git" {
			return true
		}
	}
	return false
}

// registrationDigest is used in sources.lock and is independent of map order.
func registrationDigest(reg SourceRegistration) string {
	b, _ := json.Marshal(reg)
	return digestBytes(b)
}

// --- minimal YAML decoder -------------------------------------------------

// parseYAML supports maps, arrays, quoted/scalar values, and inline arrays.
// It deliberately rejects anchors, tags, and multiline directives rather than
// silently interpreting a richer YAML language differently from CI.
func parseYAML(src string) (any, error) {
	lines := make([]yamlLine, 0)
	for n, line := range strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.Contains(line[:indent], "\t") {
			return nil, fmt.Errorf("line %d: tabs are not supported", n+1)
		}
		content := strings.TrimSpace(line)
		content = stripYAMLComment(content)
		if content == "" {
			continue
		}
		lines = append(lines, yamlLine{indent: indent, text: content, number: n + 1})
	}
	if len(lines) == 0 {
		return nil, errors.New("empty YAML")
	}
	i := 0
	v, err := parseYAMLBlock(lines, &i, lines[0].indent)
	if err != nil {
		return nil, err
	}
	if i != len(lines) {
		return nil, fmt.Errorf("line %d: unexpected content", lines[i].number)
	}
	return v, nil
}

type yamlLine struct {
	indent int
	text   string
	number int
}

func parseYAMLBlock(lines []yamlLine, i *int, indent int) (any, error) {
	if *i >= len(lines) || lines[*i].indent != indent {
		return nil, errors.New("invalid YAML indentation")
	}
	list := strings.HasPrefix(lines[*i].text, "-")
	if list {
		out := []any{}
		for *i < len(lines) && lines[*i].indent == indent && strings.HasPrefix(lines[*i].text, "-") {
			t := strings.TrimSpace(strings.TrimPrefix(lines[*i].text, "-"))
			*i++
			if t == "" {
				if *i >= len(lines) || lines[*i].indent <= indent {
					return nil, errors.New("empty YAML list item")
				}
				v, err := parseYAMLBlock(lines, i, lines[*i].indent)
				if err != nil {
					return nil, err
				}
				out = append(out, v)
				continue
			}
			if k, val, ok := splitYAMLMap(t); ok {
				m := map[string]any{}
				m[k] = parseYAMLScalar(val)
				if *i < len(lines) && lines[*i].indent > indent {
					child, err := parseYAMLBlock(lines, i, lines[*i].indent)
					if err != nil {
						return nil, err
					}
					if cm, ok := child.(map[string]any); ok {
						for ck, cv := range cm {
							m[ck] = cv
						}
					}
				}
				out = append(out, m)
			} else {
				out = append(out, parseYAMLScalar(t))
			}
		}
		return out, nil
	}
	m := map[string]any{}
	for *i < len(lines) && lines[*i].indent == indent && !strings.HasPrefix(lines[*i].text, "-") {
		line := lines[*i]
		*i++
		k, val, ok := splitYAMLMap(line.text)
		if !ok {
			return nil, fmt.Errorf("line %d: expected key: value", line.number)
		}
		if val == "" {
			if *i >= len(lines) || lines[*i].indent <= indent {
				m[k] = map[string]any{}
				continue
			}
			child, err := parseYAMLBlock(lines, i, lines[*i].indent)
			if err != nil {
				return nil, err
			}
			m[k] = child
		} else {
			m[k] = parseYAMLScalar(val)
		}
	}
	return m, nil
}

func splitYAMLMap(s string) (string, string, bool) {
	i := strings.IndexByte(s, ':')
	if i <= 0 {
		return "", "", false
	}
	key := strings.TrimSpace(s[:i])
	if strings.ContainsAny(key, "{}[]&*!|") {
		return "", "", false
	}
	return strings.Trim(key, "\"'"), strings.TrimSpace(s[i+1:]), true
}

func stripYAMLComment(s string) string {
	inSingle, inDouble := false, false
	for i, r := range s {
		switch r {
		case '\'':
			if !inDouble {
				inSingle = !inSingle
			}
		case '"':
			if !inSingle {
				inDouble = !inDouble
			}
		case '#':
			if !inSingle && !inDouble && (i == 0 || s[i-1] == ' ') {
				return strings.TrimSpace(s[:i])
			}
		}
	}
	return s
}

func parseYAMLScalar(s string) any {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && ((s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'')) {
		if s[0] == '"' {
			if v, e := strconv.Unquote(s); e == nil {
				return v
			}
		}
		return s[1 : len(s)-1]
	}
	if s == "null" || s == "~" {
		return nil
	}
	if s == "true" {
		return true
	}
	if s == "false" {
		return false
	}
	if strings.HasPrefix(s, "[") && strings.HasSuffix(s, "]") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		if inner == "" {
			return []any{}
		}
		vals := []any{}
		for _, p := range splitInline(inner) {
			vals = append(vals, parseYAMLScalar(p))
		}
		return vals
	}
	if strings.HasPrefix(s, "{") && strings.HasSuffix(s, "}") {
		inner := strings.TrimSpace(s[1 : len(s)-1])
		m := map[string]any{}
		for _, p := range splitInline(inner) {
			k, v, ok := splitYAMLMap(p)
			if ok {
				m[k] = parseYAMLScalar(v)
			}
		}
		return m
	}
	if i, e := strconv.ParseInt(s, 10, 64); e == nil {
		return i
	}
	return s
}
func splitInline(s string) []string {
	out := []string{}
	start := 0
	depth := 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == quote && (i == 0 || s[i-1] != '\\') {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '[' || c == '{' {
			depth++
		}
		if c == ']' || c == '}' {
			depth--
		}
		if c == ',' && depth == 0 {
			out = append(out, strings.TrimSpace(s[start:i]))
			start = i + 1
		}
	}
	out = append(out, strings.TrimSpace(s[start:]))
	return out
}
