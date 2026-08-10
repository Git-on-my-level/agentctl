// Package context contains the pure, deterministic context matcher. It only
// consumes an already verified knowledge bundle; it never contacts Git or
// mutates a cache.
package context

import (
	"fmt"
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/knowledge"
)

type SelectionInput struct {
	RepositoryRoot     string
	Remotes            []string
	Project            string
	Issue              string
	Run                string
	Host               string
	HostRoles          []string
	TaskKind           string
	SideEffectBoundary string
	Resources          []string
	Query              string
	Now                time.Time
	MaxRecords         int
}

type SelectedRecord struct {
	Record        knowledge.Record `json:"record"`
	Reason        []string         `json:"reason"`
	SourceCommit  string           `json:"source_commit"`
	ContentDigest string           `json:"content_digest"`
}

type Selection struct {
	BundleRevision string           `json:"bundle_revision"`
	Matches        []SelectedRecord `json:"matches"`
}

type ContextSelection = Selection
type ContextMatch = SelectedRecord

// SelectContext applies only observable, exact predicates. A record with no
// scope is advisory and is eligible only when its source/path/query is
// explicitly selected; it does not become policy by accident.
func SelectContext(bundle knowledge.Bundle, in SelectionInput) Selection {
	now := in.Now
	if now.IsZero() {
		now = time.Now().UTC()
	}
	remoteMap := map[string]string{}
	for _, s := range bundle.Manifest.Sources {
		remoteMap[s.ID] = s.RemoteURL
	}
	queryTokens := map[string]bool{}
	for _, t := range tokenize(in.Query) {
		queryTokens[t] = true
	}
	results := []SelectedRecord{}
	for _, r := range bundle.Records {
		if expired(r, now) {
			continue
		}
		if superseded(r, bundle.Records) {
			continue
		}
		reasons := []string{}
		if sourceMatches(r, remoteMap, in) {
			reasons = append(reasons, "repository")
		}
		for _, kv := range []struct{ key, val string }{{"projects", in.Project}, {"issues", in.Issue}, {"runs", in.Run}, {"task_kinds", in.TaskKind}, {"side_effect_boundaries", in.SideEffectBoundary}, {"hosts", in.Host}} {
			key, val := kv.key, kv.val
			if val != "" && scopeMatches(r.Scope, key, val) {
				reasons = append(reasons, key)
			}
		}
		if anyScopeMatches(r.Scope, "host_roles", in.HostRoles) {
			reasons = append(reasons, "host_role")
		}
		if anyScopeMatches(r.Scope, "resources", in.Resources) {
			reasons = append(reasons, "resource")
		}
		if len(queryTokens) > 0 && queryMatches(r, queryTokens) {
			reasons = append(reasons, "query")
		}
		if len(reasons) == 0 {
			continue
		}
		// Loose records never claim mandatory authority just because they matched.
		if r.Mode == knowledge.ModeLoose && r.Required {
			r.Required = false
		}
		commit := ""
		for _, s := range bundle.Manifest.Sources {
			if s.ID == r.SourceRepoID {
				commit = s.Commit
				break
			}
		}
		results = append(results, SelectedRecord{Record: r, Reason: reasons, SourceCommit: commit, ContentDigest: r.Provenance.ContentDigest})
	}
	sort.SliceStable(results, func(i, j int) bool {
		a, b := results[i], results[j]
		if a.Record.Priority != b.Record.Priority {
			return a.Record.Priority > b.Record.Priority
		}
		if a.Record.Required != b.Record.Required {
			return a.Record.Required
		}
		if a.Record.ID != b.Record.ID {
			return a.Record.ID < b.Record.ID
		}
		return a.Record.Provenance.Path < b.Record.Provenance.Path
	})
	if in.MaxRecords > 0 && len(results) > in.MaxRecords {
		results = results[:in.MaxRecords]
	}
	return Selection{BundleRevision: bundle.Manifest.BundleRevision, Matches: results}
}

func Select(bundle knowledge.Bundle, in SelectionInput) Selection {
	return SelectContext(bundle, in)
}

func sourceMatches(r knowledge.Record, remotes map[string]string, in SelectionInput) bool {
	if in.RepositoryRoot != "" {
		base := filepath.Base(filepath.Clean(in.RepositoryRoot))
		for _, v := range r.Scope["repositories"] {
			if v == base || strings.HasSuffix(strings.TrimSuffix(v, "/"), "/"+base) || strings.EqualFold(v, in.RepositoryRoot) {
				return true
			}
		}
	}
	if len(in.Remotes) == 0 {
		return false
	}
	remote := remotes[r.SourceRepoID]
	for _, candidate := range in.Remotes {
		if sameRemote(remote, candidate) {
			return true
		}
	}
	return false
}
func sameRemote(a, b string) bool {
	if a == b {
		return true
	}
	ua, ea := url.Parse(a)
	ub, eb := url.Parse(b)
	if ea != nil || eb != nil {
		return filepath.Clean(a) == filepath.Clean(b)
	}
	return strings.TrimSuffix(ua.Host+ua.Path, "/") == strings.TrimSuffix(ub.Host+ub.Path, "/")
}
func scopeMatches(scope map[string][]string, key, val string) bool {
	for _, x := range scope[key] {
		if x == val || strings.EqualFold(x, val) {
			return true
		}
	}
	return false
}
func anyScopeMatches(scope map[string][]string, key string, vals []string) bool {
	for _, v := range vals {
		if scopeMatches(scope, key, v) {
			return true
		}
	}
	return false
}
func expired(r knowledge.Record, now time.Time) bool {
	if r.ExpiresAt == "" || r.ExpiresAt == "null" {
		return false
	}
	s := r.ExpiresAt
	t, e := time.Parse(time.RFC3339, s)
	if e != nil {
		t, e = time.Parse("2006-01-02", s)
	}
	return e == nil && !now.Before(t)
}
func superseded(r knowledge.Record, all []knowledge.Record) bool {
	for _, other := range all {
		for _, id := range other.Supersedes {
			if id == r.ID {
				return true
			}
		}
	}
	return false
}
func queryMatches(r knowledge.Record, tokens map[string]bool) bool {
	for _, t := range tokenize(r.Title + " " + r.Text) {
		if tokens[t] {
			return true
		}
	}
	return false
}
func tokenize(s string) []string {
	out := []string{}
	b := strings.Builder{}
	flush := func() {
		if b.Len() > 0 {
			out = append(out, strings.ToLower(b.String()))
			b.Reset()
		}
	}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// Render emits stable Markdown suitable for an adapter context file. It is
// bounded by bytes without ever splitting UTF-8.
func Render(sel Selection, maxBytes int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# agentctl context\n\nBundle: `%s`\n\n", sel.BundleRevision)
	for _, m := range sel.Matches {
		fmt.Fprintf(&b, "## %s\n\n", m.Record.Title)
		fmt.Fprintf(&b, "- record: `%s`\n- source: `%s`\n- path: `%s`\n- lines: %d-%d\n- reason: %s\n- content_digest: `%s`\n\n", m.Record.ID, m.Record.SourceRepoID, m.Record.Provenance.Path, m.Record.Provenance.StartLine, m.Record.Provenance.EndLine, strings.Join(m.Reason, ", "), m.ContentDigest)
		b.WriteString(m.Record.Text)
		b.WriteString("\n\n")
	}
	out := b.String()
	if maxBytes > 0 && len([]byte(out)) > maxBytes {
		raw := []byte(out)
		cut := maxBytes
		for cut > 0 && (raw[cut]&0xc0) == 0x80 {
			cut--
		}
		out = string(raw[:cut]) + "\n"
	}
	return out
}
