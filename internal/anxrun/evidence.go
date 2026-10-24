package anxrun

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	LinkKindPR     = "pr"
	LinkKindIssue  = "issue"
	LinkKindCommit = "commit"
	MaxLinks       = 8
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

// Repository components retain only structural checks and credential checks.
func validateLink(link Link) (Link, int) {
	for _, field := range []struct {
		value  string
		max    int
		crypto bool
	}{
		{link.Kind, 6, false}, {link.Owner, len(link.Owner), false}, {link.Repo, len(link.Repo), false}, {link.SHA, 40, true},
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
		if !coordinate(link.Owner) || !coordinate(link.Repo) || link.Number < 1 || link.Number > 2147483647 || link.SHA != "" {
			return Link{}, EgressFormat
		}
		return Link{Kind: link.Kind, Owner: link.Owner, Repo: link.Repo, Number: link.Number}, 0
	}
	return Link{}, EgressFormat
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
