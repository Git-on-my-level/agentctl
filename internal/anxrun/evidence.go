package anxrun

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"
)

const (
	LinkKindPR     = "pr"
	LinkKindIssue  = "issue"
	LinkKindCommit = "commit"
	MaxLinks       = 8
	MaxLinkRef     = 512
	maxLinkScan    = 4096
)

// Link is structured evidence. URLs and arbitrary ref strings have no field.
type Link struct {
	Kind   string `json:"kind"`
	Owner  string `json:"owner,omitempty"`
	Repo   string `json:"repo,omitempty"`
	Number int    `json:"number,omitempty"`
	SHA    string `json:"sha,omitempty"`
}

var gitOIDPattern = regexp.MustCompile(`^[a-fA-F0-9]{40}$`)
var githubOwner = regexp.MustCompile(`^[A-Za-z0-9]+(?:-[A-Za-z0-9]+)*$`)
var githubRepo = regexp.MustCompile(`^[A-Za-z0-9_.-]+$`)

// GitHub owners: 1-39 alphanumerics/single hyphens, no edge hyphen.
// Repositories: 1-100 ASCII alphanumerics, '.', '_' and '-'. Dot paths and
// reserved clone/wiki suffixes are conservatively rejected.
func validateLink(link Link) (Link, int) {
	for _, field := range []struct {
		value  string
		max    int
		crypto bool
	}{
		{link.Kind, 6, false}, {link.Owner, 39, false}, {link.Repo, 100, false}, {link.SHA, 40, true},
	} {
		if code := stringCode(field.value, field.max, field.crypto); code != 0 {
			return Link{}, code
		}
	}
	switch link.Kind {
	case LinkKindCommit:
		if !gitOIDPattern.MatchString(link.SHA) || link.Owner != "" || link.Repo != "" || link.Number != 0 {
			return Link{}, EgressFormat
		}
		return Link{Kind: LinkKindCommit, SHA: strings.ToLower(link.SHA)}, 0
	case LinkKindPR, LinkKindIssue:
		repo := strings.ToLower(link.Repo)
		if !githubOwner.MatchString(link.Owner) || !githubRepo.MatchString(link.Repo) || repo == "." || repo == ".." || strings.HasSuffix(repo, ".git") || strings.HasSuffix(repo, ".wiki") || link.Number < 1 || link.Number > 2147483647 || link.SHA != "" {
			return Link{}, EgressFormat
		}
		return Link{Kind: link.Kind, Owner: link.Owner, Repo: link.Repo, Number: link.Number}, 0
	}
	return Link{}, EgressFormat
}

// SanitizeLink accepts a complete JSON evidence object or a 40-hex commit
// (optionally 'commit:'). URL input is deliberately unsupported.
func SanitizeLink(text string) (Link, bool) {
	link, code := parseLink(text)
	return link, code == 0
}

func parseLink(text string) (Link, int) {
	text = strings.TrimSpace(text)
	if len(text) > MaxLinkRef {
		return Link{}, EgressLength
	}
	if strings.HasPrefix(text, "commit:") {
		text = strings.TrimSpace(strings.TrimPrefix(text, "commit:"))
	}
	if gitOIDPattern.MatchString(text) {
		return validateLink(Link{Kind: LinkKindCommit, SHA: text})
	}
	var link Link
	decoder := json.NewDecoder(strings.NewReader(text))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&link) != nil {
		return Link{}, EgressFormat
	}
	var extra any
	if decoder.Decode(&extra) != io.EOF {
		return Link{}, EgressFormat
	}
	return validateLink(link)
}

func ExtractLinks(text string) []Link { links, _ := extractLinks(text); return links }

func extractLinks(text string) ([]Link, int) {
	var links []Link
	seen := map[Link]bool{}
	code := 0
	if len(text) > maxLinkScan {
		text = text[:maxLinkScan]
	}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		// Never scan prose for evidence. Only an explicit complete object or
		// commit line is considered; all other result content remains private.
		if !strings.HasPrefix(line, "{") && !strings.HasPrefix(line, "commit:") && !gitOIDPattern.MatchString(line) {
			continue
		}
		link, rejection := parseLink(line)
		if rejection != 0 {
			code = rejection
			continue
		}
		if !seen[link] {
			links = append(links, link)
			seen[link] = true
		}
		if len(links) == MaxLinks {
			break
		}
	}
	return links, code
}

func SanitizeHeadAfter(value string) (string, error) {
	if value == "" {
		return "", nil
	}
	if stringCode(value, 40, true) != 0 || !gitOIDPattern.MatchString(value) {
		return "", fmt.Errorf("egress_rejected:%d", EgressFormat)
	}
	return strings.ToLower(value), nil
}
