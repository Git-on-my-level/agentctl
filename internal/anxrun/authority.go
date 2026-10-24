package anxrun

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

type Repository struct {
	Host  string `json:"host,omitempty"`
	Owner string `json:"owner,omitempty"`
	Name  string `json:"name,omitempty"`
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

// Coordinates are path components, not GitHub-specific names. Disclosure of
// hostnames, IPs and user/repo names is permitted (David, 2026-10-24).
func coordinate(value string) bool {
	return value != "" && value != "." && value != ".." && utf8.ValidString(value) &&
		!strings.ContainsAny(value, "/\\?#@:") && !strings.ContainsFunc(value, unicode.IsControl)
}

func ValidOrg(value string) bool {
	return coordinate(value) && stringCode(value, len(value), false) == 0
}

func (v Verification) allowsOwner(owner string) bool {
	if len(v.AllowedOrgs) == 0 {
		return true
	}
	for _, org := range v.AllowedOrgs {
		if ValidOrg(org) && strings.EqualFold(org, owner) {
			return true
		}
	}
	return false
}

func (v Verification) repository(link Link) (Link, bool) {
	origin := v.Origin
	if origin == nil || !strings.EqualFold(origin.Owner, link.Owner) || !strings.EqualFold(origin.Name, link.Repo) {
		return Link{}, false
	}
	if v.allowsOwner(origin.Owner) {
		link.Owner, link.Repo = origin.Owner, origin.Name
		if safe, code := validateLink(link); code == 0 {
			return safe, true
		}
	}
	return Link{}, false
}

// Parsing an SSH-form remote never executes SSH. This command reads only the
// run repository's local origin configuration: no includes, hooks or network.
func OriginRepository(ctx context.Context, cwd string) *Repository {
	if cwd == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	command := exec.CommandContext(ctx, "git", "-C", cwd, "config", "--local", "--no-includes", "--get", "remote.origin.url")
	command.Env = originGitEnvironment()
	command.WaitDelay = 100 * time.Millisecond
	raw, err := command.Output()
	if err != nil {
		return nil
	}
	return ParseOrigin(strings.TrimSpace(string(raw)))
}

// No inherited Git setting may select another repository or inject config.
// Keep ordinary process settings (PATH, locale), then add read-only defaults.
func originGitEnvironment() []string {
	env := make([]string, 0, len(os.Environ())+4)
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(strings.ToUpper(key), "GIT_") {
			env = append(env, entry)
		}
	}
	return append(env, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL="+os.DevNull, "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0")
}

func ParseOrigin(value string) *Repository {
	// Strip userinfo before parsing coordinates, including malformed or
	// credential-shaped userinfo. It is never a repository field.
	if scheme := strings.Index(value, "://"); scheme >= 0 {
		authority := scheme + 3
		end := strings.IndexAny(value[authority:], "/?#")
		if end < 0 {
			end = len(value) - authority
		}
		if at := strings.LastIndex(value[authority:authority+end], "@"); at >= 0 {
			value = value[:authority] + value[authority+at+1:]
		}
	} else {
		if at := strings.LastIndex(value, "@"); at >= 0 {
			value = value[at+1:]
		}
		// SCP-style remotes are parsed as URLs locally; never execute SSH.
		host, path, ok := strings.Cut(value, ":")
		if !ok {
			return nil
		}
		value = "ssh://" + host + "/" + path
	}
	u, err := url.Parse(value)
	if err != nil || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Opaque != "" {
		return nil
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || !coordinate(parts[0]) {
		return nil
	}
	name := strings.TrimSuffix(parts[1], ".git")
	if !coordinate(name) || strings.ContainsFunc(u.Host, unicode.IsControl) {
		return nil
	}
	return &Repository{Host: strings.ToLower(u.Host), Owner: parts[0], Name: name}
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
