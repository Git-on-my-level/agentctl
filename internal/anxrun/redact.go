package anxrun

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"
)

// SummaryOmittedRedactionFailed marks a summary dropped wholesale because a
// redaction rule matched ambiguously. Never ship a maybe-leak.
const SummaryOmittedRedactionFailed = "redaction_failed"

// DefaultSummaryMaxBytes bounds a shareable outcome summary.
const DefaultSummaryMaxBytes = 2048

// truncationMarker is appended when a summary exceeds its byte bound.
const truncationMarker = "…[truncated]"

// credentialPatterns match whole shapes that must never survive into a
// shareable summary. Any match drops the entire opted-in summary.
var credentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`AKIA[0-9A-Z]{16}`),
	regexp.MustCompile(`sk-[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`ghp_[A-Za-z0-9]{36}`),
	regexp.MustCompile(`github_pat_[A-Za-z0-9_]{22,}`),
	regexp.MustCompile(`xox[bp]-[A-Za-z0-9-]{10,}`),
	regexp.MustCompile(`-----BEGIN [A-Z ]*PRIVATE KEY-----`),
	regexp.MustCompile(`(?i)\bbearer\s+[A-Za-z0-9._~+/-]{16,}`),
	regexp.MustCompile(`ya29\.[A-Za-z0-9_-]{16,}`),
	regexp.MustCompile(`AIza[0-9A-Za-z_-]{35}`),
}

// sensitiveAssignmentPattern matches high-entropy assignments to names that
// denote secrets (token|key|secret|password|credential), e.g.
// `API_TOKEN="a9f3..."` or `db_password: h4x0rz1234567890`.
var sensitiveAssignmentPattern = regexp.MustCompile(`(?i)\b[a-z0-9_-]*(token|key|secret|password|credential)[a-z0-9_-]*\s*[:=]\s*["']?[A-Za-z0-9+/_-]{20,}["']?`)

// ambiguousCredentialPatterns match partial credential shapes — token-looking
// prefixes with a high-entropy tail shorter than the full known shape. A line
// carrying one is not dropped (it may be a coincidence like "task-id-123"),
// but if one survives line-wise redaction the whole summary is omitted:
// never ship a maybe-leak.
var ambiguousCredentialPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\b(?:sk-|ghp_|github_pat_|xox[bp]-|ya29\.|AKIA)[A-Za-z0-9_-]+`),
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{8,}`),
	regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{8,}`),
}

// Redacted is the deterministic result of summarizing a stored result body.
// Exactly one of Summary or OmittedReason is nonempty; both empty means the
// source text was blank.
type Redacted struct {
	Summary       string
	OmittedReason string
}

// MarkedSummary is the only free-text opt-in. Exactly one complete,
// line-delimited block is accepted. Everything outside it stays local.
func MarkedSummary(text string, maxBytes int) Redacted {
	const begin, end = "---anx-summary---", "---end-anx-summary---"
	if strings.Count(text, begin) != 1 || strings.Count(text, end) != 1 {
		return Redacted{OmittedReason: "not_opted_in"}
	}
	lines := strings.Split(text, "\n")
	inside, finished := false, false
	var block []string
	for _, line := range lines {
		switch line {
		case begin:
			if finished {
				return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
			}
			inside = true
		case end:
			if !inside {
				return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
			}
			inside, finished = false, true
		default:
			if inside {
				block = append(block, line)
			}
		}
	}
	if !finished || inside {
		return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
	}
	return RedactSummary(strings.Join(block, "\n"), maxBytes)
}

// RedactSummary is a conservative second layer, never the egress boundary.
// Any credential, assignment, URL userinfo, PEM material, path, host or
// ambiguous token drops the entire opted-in block, including multiline keys.
func RedactSummary(text string, maxBytes int) Redacted {
	if maxBytes <= 0 || maxBytes > DefaultSummaryMaxBytes {
		maxBytes = DefaultSummaryMaxBytes
	}
	scan := text
	for _, candidate := range summaryURLPattern.FindAllString(text, -1) {
		if _, ok := SanitizeLink(candidate); !ok {
			return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
		}
		scan = strings.ReplaceAll(scan, candidate, "<evidence>")
	}
	if !utf8.ValidString(text) || sensitiveText(text) || ambiguousSummary.MatchString(scan) || localPathPattern.MatchString(scan) {
		return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
	}
	return Redacted{Summary: truncateUTF8(strings.TrimSpace(text), maxBytes)}
}

var summaryURLPattern = regexp.MustCompile(`(?i)[a-z][a-z0-9+.-]*://[^\s]+`)

var ambiguousSecretAssignment = regexp.MustCompile(`(?i)\b[a-z0-9_-]*(?:token|key|secret|password|credential)[a-z0-9_-]*\s*[:=]|\bbearer\s+\S+`)
var localPathPattern = regexp.MustCompile("(?:^|[\\s`\"'(\\[=,])/(?:[^/\\s][^\\s]*)|~[A-Za-z0-9_.-]*/|(?i)(?:fd7a:|fe80:|::1\\b)")

var ambiguousSummary = regexp.MustCompile("(?i)(-----|[A-Za-z_][A-Za-z0-9_]*\\s*=|https?://[^\\s]*[@?#]|(?:/Users/|/Volumes/|/home/|/private/|/tmp/|/var/|/opt/|/mnt/|~/)|[a-z0-9.-]+\\.ts\\.net|\\b(?:[0-9]{1,3}\\.){3}[0-9]{1,3}\\b|[A-Za-z0-9+/=_-]{32,})")

func sensitiveText(text string) bool {
	for _, patterns := range [][]*regexp.Regexp{credentialPatterns, ambiguousCredentialPatterns} {
		for _, p := range patterns {
			if p.MatchString(text) {
				return true
			}
		}
	}
	return sensitiveAssignmentPattern.MatchString(text) || ambiguousSecretAssignment.MatchString(text)
}

// truncateUTF8 bounds value to maxBytes without splitting a rune and appends
// the truncation marker when truncation happened.
func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	budget := maxBytes - len(truncationMarker)
	marker := truncationMarker
	if budget < 0 {
		budget = maxBytes
		marker = ""
	}
	cut := value[:budget]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + marker
}

// Evidence is limited to GitHub PR/issue coordinates and 40-hex commits.
var (
	gitOIDPattern    = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
	linkRefPattern   = regexp.MustCompile(`^(pr|commit|issue):(.+)$`)
	githubRefPattern = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9][A-Za-z0-9-]{0,38})/([A-Za-z0-9_.-]{1,100})/(pull|issues)/([1-9][0-9]{0,9})$`)
)

// LinkKind enumerates shareable link kinds.
const (
	LinkKindPR     = "pr"
	LinkKindCommit = "commit"
	LinkKindIssue  = "issue"
	LinkKindURL    = "url"
)

// Link is one bounded evidence reference attached to an outcome.
type Link struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

// Link bounds: at most MaxLinks entries, each ref at most MaxLinkRef bytes.
const (
	MaxLinks    = 8
	MaxLinkRef  = 512
	maxLinkScan = 4096
)

// SanitizeLink validates one candidate evidence reference against the link
// allowlist: GitHub owner/repo/pull|issues/number only, or a 40-hex SHA.
// No credentials, queries (including empty ?), fragments, ports, or extra paths.
func SanitizeLink(ref string) (Link, bool) {
	ref = strings.TrimSpace(ref)
	if len(ref) > MaxLinkRef || sensitiveText(ref) {
		return Link{}, false
	}
	kind := ""
	if match := linkRefPattern.FindStringSubmatch(ref); match != nil {
		kind, ref = match[1], strings.TrimSpace(match[2])
	}
	if gitOIDPattern.MatchString(ref) && (kind == "" || kind == LinkKindCommit) {
		return Link{Kind: LinkKindCommit, Ref: strings.ToLower(ref)}, true
	}
	match := githubRefPattern.FindStringSubmatch(ref)
	if match == nil || match[2] == "." || match[2] == ".." || strings.HasSuffix(match[1], "-") {
		return Link{}, false
	}
	want := LinkKindPR
	if match[3] == "issues" {
		want = LinkKindIssue
	}
	if kind != "" && kind != want {
		return Link{}, false
	}
	return Link{Kind: want, Ref: ref}, true
}

// ExtractLinks derives bounded, deduplicated evidence links from a stored
// result body. It recognizes `pr:`/`commit:`/`issue:` evidence lines (the
// PM evidence-block convention) and bare https git-host URLs, preserving
// order, at most MaxLinks entries. Candidates that fail SanitizeLink are
// dropped rather than truncated.
func ExtractLinks(text string) []Link {
	links := []Link{}
	seen := map[string]struct{}{}
	scan := text
	if len(scan) > maxLinkScan {
		scan = scan[:maxLinkScan]
	}
	for _, line := range strings.Split(scan, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var candidate string
		if linkRefPattern.MatchString(line) {
			candidate = line
		} else if strings.HasPrefix(line, "https://") && !strings.ContainsAny(line, " \t") {
			candidate = line
		}
		if candidate == "" {
			continue
		}
		link, ok := SanitizeLink(candidate)
		if !ok {
			continue
		}
		key := link.Kind + "\x00" + link.Ref
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		links = append(links, link)
		if len(links) == MaxLinks {
			break
		}
	}
	return links
}

// headAfterPattern is a bare 40-hex git object id.
var headAfterPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)

// SanitizeHeadAfter accepts a bare 40-hex git object id or empty.
func SanitizeHeadAfter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !headAfterPattern.MatchString(value) {
		return "", fmt.Errorf("head_after must be a bare 40-hex sha")
	}
	return strings.ToLower(value), nil
}
