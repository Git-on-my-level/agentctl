package anxrun

import (
	"fmt"
	"net/url"
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
// shareable summary. A line containing any of them is dropped.
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
	regexp.MustCompile(`\bsk-[A-Za-z0-9_-]{8,}`),
	regexp.MustCompile(`\bghp_[A-Za-z0-9]{16,}`),
	regexp.MustCompile(`\bAKIA[0-9A-Z]{8,}`),
	regexp.MustCompile(`\bya29\.[A-Za-z0-9_-]{8,}`),
}

// envAssignmentPattern matches wholesale ENV=VALUE assignment lines. Values
// are never negotiated into a shareable summary.
var envAssignmentPattern = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=\S*$`)

// pathDelim anchors a path so `(/Users/x)` or `=/home/y` are matched whole
// while keeping the delimiter out of the replacement.
const pathDelim = `(^|[\s"'(\[=,])`

// absolutePathPattern matches absolute local paths under the common local
// roots (/Users, /Volumes, /home, and friends).
var absolutePathPattern = regexp.MustCompile(pathDelim + `(/(?:Users|Volumes|home|private|tmp|var|opt|mnt)/[^\s"'()\[\]]*)`)

// tildePathPattern matches a ~ or ~user relative path reference.
var tildePathPattern = regexp.MustCompile(pathDelim + `(~[A-Za-z0-9_.-]*/[^\s"'()\[\]]*)`)

// tailnetHostPattern matches *.ts.net hostnames; privateIPPattern matches
// private-use and loopback IPv4 addresses used on the fleet Tailnet.
var (
	tailnetHostPattern = regexp.MustCompile(`\b(?:[a-z0-9-]+\.)+ts\.net\b`)
	privateIPPattern   = regexp.MustCompile(`\b(?:10\.\d{1,3}\.\d{1,3}\.\d{1,3}|192\.168\.\d{1,3}\.\d{1,3}|172\.(?:1[6-9]|2\d|3[01])\.\d{1,3}\.\d{1,3}|127\.0\.0\.1)\b`)
)

// urlQueryPattern matches the query string of an http(s) URL; query strings
// may carry tokens, so only scheme, host, and path survive.
var urlQueryPattern = regexp.MustCompile(`(https?://[^\s?]+)\?[^\s]+`)

// Redacted is the deterministic result of summarizing a stored result body.
// Exactly one of Summary or OmittedReason is nonempty; both empty means the
// source text was blank.
type Redacted struct {
	Summary       string
	OmittedReason string
}

// RedactSummary derives the shareable summary from a stored result body.
// Rules are applied in order, then the result is truncated to maxBytes
// UTF-8 bytes with a trailing truncation marker:
//
//  1. Drop any line matching a credential shape, a high-entropy secret-name
//     assignment, or an ENV=VALUE assignment.
//  2. Replace absolute and ~-relative local paths with <path>, Tailnet
//     hostnames and private IPs with <host>, and strip URL query strings.
//  3. If any credential shape still matches after replacement — an ambiguous
//     partial overlap — the whole summary is omitted with redaction_failed.
func RedactSummary(text string, maxBytes int) Redacted {
	if maxBytes <= 0 {
		maxBytes = DefaultSummaryMaxBytes
	}
	if strings.TrimSpace(text) == "" {
		return Redacted{}
	}
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if dropLine(line) {
			continue
		}
		kept = append(kept, replaceSensitive(line))
	}
	joined := strings.Trim(strings.Join(kept, "\n"), "\n")
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(joined) {
			return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
		}
	}
	if sensitiveAssignmentPattern.MatchString(joined) {
		return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
	}
	for _, pattern := range ambiguousCredentialPatterns {
		if pattern.MatchString(joined) {
			return Redacted{OmittedReason: SummaryOmittedRedactionFailed}
		}
	}
	return Redacted{Summary: truncateUTF8(joined, maxBytes)}
}

// dropLine reports whether a whole line must not survive redaction.
func dropLine(line string) bool {
	for _, pattern := range credentialPatterns {
		if pattern.MatchString(line) {
			return true
		}
	}
	if sensitiveAssignmentPattern.MatchString(line) {
		return true
	}
	return envAssignmentPattern.MatchString(strings.TrimSpace(line))
}

// replaceSensitive rewrites paths, hosts, and URL queries in one line.
func replaceSensitive(line string) string {
	line = urlQueryPattern.ReplaceAllString(line, "$1")
	line = tildePathPattern.ReplaceAllString(line, "${1}<path>")
	line = absolutePathPattern.ReplaceAllString(line, "${1}<path>")
	line = privateIPPattern.ReplaceAllString(line, "<host>")
	line = tailnetHostPattern.ReplaceAllString(line, "<host>")
	return line
}

// truncateUTF8 bounds value to maxBytes without splitting a rune and appends
// the truncation marker when truncation happened.
func truncateUTF8(value string, maxBytes int) string {
	if len(value) <= maxBytes {
		return value
	}
	budget := maxBytes - len(truncationMarker)
	if budget < 0 {
		budget = 0
	}
	cut := value[:budget]
	for !utf8.ValidString(cut) && len(cut) > 0 {
		cut = cut[:len(cut)-1]
	}
	return cut + truncationMarker
}

// Link allowlists. Links pass the same redaction filter plus a scheme/host
// allowlist (https git hosts) or a pr:/commit:/issue: ref form.
var (
	gitOIDPattern  = regexp.MustCompile(`^[a-f0-9]{40}(?:[a-f0-9]{24})?$`)
	gitHostPattern = regexp.MustCompile(`^(?:github\.com|gitlab\.com|bitbucket\.org|[\w-]+\.forgejo\.[a-z]+|[\w-]+\.gitea\.[a-z]+)$`)
	linkRefPattern = regexp.MustCompile(`^(pr|commit|issue):(.+)$`)
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
// allowlist. Accepted forms are `pr:`/`issue:` with an https git-host URL,
// `commit:` with a bare git object id, and a bare https git-host URL.
func SanitizeLink(ref string) (Link, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" || len(ref) > MaxLinkRef {
		return Link{}, false
	}
	if match := linkRefPattern.FindStringSubmatch(ref); match != nil {
		kind, rest := match[1], strings.TrimSpace(match[2])
		switch kind {
		case LinkKindPR, LinkKindIssue:
			link, ok := SanitizeLink(rest)
			if !ok || link.Kind != LinkKindURL {
				return Link{}, false
			}
			return Link{Kind: kind, Ref: rest}, true
		case LinkKindCommit:
			if !gitOIDPattern.MatchString(rest) {
				return Link{}, false
			}
			return Link{Kind: kind, Ref: rest}, true
		}
		return Link{}, false
	}
	parsed, err := url.Parse(ref)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.User != nil {
		return Link{}, false
	}
	if !gitHostPattern.MatchString(parsed.Hostname()) {
		return Link{}, false
	}
	if strings.ContainsAny(ref, "<>\"' ") {
		return Link{}, false
	}
	return Link{Kind: LinkKindURL, Ref: ref}, true
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
var headAfterPattern = regexp.MustCompile(`^[a-f0-9]{40}$`)

// SanitizeHeadAfter accepts a bare 40-hex git object id or empty.
func SanitizeHeadAfter(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", nil
	}
	if !headAfterPattern.MatchString(value) {
		return "", fmt.Errorf("head_after must be a bare 40-hex sha")
	}
	return value, nil
}
