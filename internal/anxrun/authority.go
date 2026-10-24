package anxrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type Repository struct {
	Owner string
	Name  string
}
type ConfirmedCard struct {
	Ref      string `json:"ref"`
	ThreadID string `json:"thread_id"`
}

// Verification is supplied only by the local policy/authoritative catalog
// loader. It is never decoded from a registration or supplied by result text.
type Verification struct {
	Cards       map[string]ConfirmedCard
	AllowedOrgs []string
	Origin      *Repository
}

const EgressUnverified = 1004

var ipv4Substring = regexp.MustCompile(`[0-9]{1,3}(?:\.[0-9]{1,3}){3}`)

func networkCoordinate(value string) bool {
	lower := strings.ToLower(value)
	if strings.Contains(lower, ".ts.net") || strings.Contains(lower, ".tailnet.") || strings.HasSuffix(lower, ".local") || net.ParseIP(value) != nil {
		return true
	}
	for _, candidate := range ipv4Substring.FindAllString(value, -1) {
		if net.ParseIP(candidate) != nil {
			return true
		}
	}
	return false
}

func ValidOrg(value string) bool {
	return len(value) <= 39 && githubOwner.MatchString(value) && stringCode(value, 39, false) == 0 && !networkCoordinate(value)
}

func (v Verification) repository(link Link) (Link, bool) {
	if origin := v.Origin; origin != nil && strings.EqualFold(origin.Owner, link.Owner) && strings.EqualFold(origin.Name, link.Repo) {
		link.Owner, link.Repo = origin.Owner, origin.Name
		if _, code := validateLink(link); code == 0 {
			return link, true
		}
	}
	for _, org := range v.AllowedOrgs {
		if ValidOrg(org) && strings.EqualFold(org, link.Owner) {
			link.Owner = org
			return link, true
		}
	}
	return Link{}, false
}

var httpsOrigin = regexp.MustCompile(`^https://github\.com/([^/]+)/([^/]+)$`)
var sshOrigin = regexp.MustCompile(`^(?:git@github\.com:|ssh://git@github\.com/)([^/]+)/([^/]+)$`)

// Parsing an SSH-form remote never executes SSH. This command reads only the
// run repository's local origin configuration: no includes, hooks or network.
func OriginRepository(ctx context.Context, cwd string) *Repository {
	if cwd == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--local", "--no-includes", "--get", "remote.origin.url")
	command.WaitDelay = 100 * time.Millisecond
	raw, err := command.Output()
	if err != nil || len(raw) > 512 {
		return nil
	}
	return ParseOrigin(strings.TrimSpace(string(raw)))
}

func ParseOrigin(value string) *Repository {
	match := httpsOrigin.FindStringSubmatch(value)
	if match == nil {
		match = sshOrigin.FindStringSubmatch(value)
	}
	if match == nil {
		return nil
	}
	owner, repo := match[1], strings.TrimSuffix(match[2], ".git")
	if _, code := validateLink(Link{Kind: LinkKindPR, Owner: owner, Repo: repo, Number: 1}); code != 0 {
		return nil
	}
	return &Repository{Owner: owner, Name: repo}
}

// loadCards requests the entire canonical workspace catalog without any
// user-provided slug or search string. Unknown/paginated/incomplete responses
// fail closed. The cache contains only verified coordinates, never card prose.
func (s *CLISender) loadCards(ctx context.Context) (map[string]ConfirmedCard, error) {
	result, err := s.invokeResult(ctx, "cards.list", nil, nil)
	if err != nil {
		return nil, err
	}
	var response struct {
		Cards []struct {
			Ref      string `json:"ref"`
			Handle   string `json:"handle"`
			ThreadID string `json:"thread_id"`
		} `json:"cards"`
		NextCursor string `json:"next_cursor"`
		HasMore    bool   `json:"has_more"`
	}
	if json.Unmarshal(result, &response) != nil || response.Cards == nil || len(response.Cards) > 4096 || response.NextCursor != "" || response.HasMore {
		return nil, &SendError{Code: "protocol_error"}
	}
	cards := map[string]ConfirmedCard{}
	for _, item := range response.Cards {
		ref := item.Ref
		if !strings.HasPrefix(ref, cardRefPrefix) || ParseCardSlug(strings.TrimPrefix(ref, cardRefPrefix)) != nil || !safeServerCoordinate(item.ThreadID) {
			continue
		}
		if item.Handle != "" && ref != cardRefPrefix+item.Handle {
			continue
		}
		if old, exists := cards[ref]; exists && old.ThreadID != item.ThreadID {
			return nil, &SendError{Code: "protocol_error"}
		}
		cards[ref] = ConfirmedCard{Ref: ref, ThreadID: item.ThreadID}
	}
	if s.options.CacheDir != "" {
		options := s.options.normalized()
		sum := sha256.Sum256([]byte(options.Executable + "\x00" + options.WorkspaceAlias + "\x00" + options.BaseURL + "\x00" + options.ConfigDir))
		cache := struct {
			Scope     string                   `json:"scope"`
			CheckedAt time.Time                `json:"checked_at"`
			Cards     map[string]ConfirmedCard `json:"cards"`
		}{"sha256:" + hex.EncodeToString(sum[:]), time.Now().UTC(), cards}
		raw, marshalErr := json.Marshal(cache)
		if marshalErr != nil {
			return nil, egressError(EgressFormat)
		}
		if err := writeFileAtomic(filepath.Join(s.options.CacheDir, "confirmed-cards.json"), raw); err != nil {
			return nil, err
		}
	}
	return cards, nil
}
