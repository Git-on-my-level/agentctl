package route

import (
	"sort"
	"strings"
	"unicode"
)

const (
	scoreExact     = 100
	scorePrefix    = 70
	scoreSubstring = 40
	minFuzzyToken  = 3
)

// Catalog is the reviewed keyword set. Empty catalogs are valid: the matcher
// then only knows built-in adapter family aliases.
type Catalog struct {
	ThisHost      string
	Hosts         []HostRecord
	Models        []ModelRecord
	PlacementKind string
}

type HostRecord struct {
	ID      string
	Aliases []string
}

type ModelRecord struct {
	Adapter string
	Model   string
	Speed   string
	Aliases []string
}

type HostHit struct {
	ID    string `json:"id"`
	Score int    `json:"-"`
	Hit   string `json:"-"`
	Kind  string `json:"-"`
}

type ModelHit struct {
	Adapter string `json:"adapter"`
	Model   string `json:"model,omitempty"`
	Speed   string `json:"speed,omitempty"`
	Score   int    `json:"-"`
	Hit     string `json:"-"`
	Kind    string `json:"-"`
}

type PlacementAdvice struct {
	Mode   string `json:"mode"`
	Kind   string `json:"kind,omitempty"`
	Host   string `json:"host,omitempty"`
	Reason string `json:"-"`
}

// MatchResult is a ranked explanation. Absent lists mean "nothing recognized",
// not an error.
type MatchResult struct {
	Query     string          `json:"-"`
	Tokens    []string        `json:"-"`
	Hosts     []HostHit       `json:"hosts,omitempty"`
	Models    []ModelHit      `json:"models,omitempty"`
	Unmatched []string        `json:"unmatched,omitempty"`
	Placement PlacementAdvice `json:"placement"`
}

var glueWords = map[string]struct{}{
	"on": {}, "with": {}, "to": {}, "via": {}, "using": {},
	"the": {}, "a": {}, "an": {}, "for": {}, "and": {}, "or": {},
	"please": {}, "at": {}, "in": {}, "from": {},
}

// BuiltinModelCatalog is the reviewed family→adapter map used when config
// has no preferred[] entries. Model slugs stay empty so callers still pick
// the native default.
func BuiltinModelCatalog() []ModelRecord {
	return []ModelRecord{
		{Adapter: "codex", Aliases: []string{"gpt", "openai", "openai-gpt", "codex"}},
		{Adapter: "claude", Aliases: []string{"claude", "anthropic", "anthropic-claude"}},
		{Adapter: "cursor", Aliases: []string{"cursor", "composer", "grok", "cursor-composer", "cursor-grok"}},
		{Adapter: "omp", Aliases: []string{"glm", "omp", "open-weight", "open_weight", "openweight"}},
	}
}

func Tokenize(query string) []string {
	raw := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return unicode.IsSpace(r) || r == ',' || r == '/'
	})
	var tokens []string
	for _, tok := range raw {
		tok = normalizeKeyword(tok)
		if tok == "" {
			continue
		}
		if _, glue := glueWords[tok]; glue {
			continue
		}
		tokens = append(tokens, tok)
	}
	return tokens
}

func Match(query string, catalog Catalog) MatchResult {
	tokens := Tokenize(query)
	hosts := matchHosts(tokens, catalog.Hosts)
	modelHits := matchModels(tokens, catalog.Models)
	consumed := map[string]struct{}{}
	for _, hit := range hosts {
		consumed[hit.Hit] = struct{}{}
	}
	// A generic adapter hit can be collapsed when an exact concrete model for
	// that adapter is also present. It was still a recognized selector and must
	// not reappear as unmatched merely because the compact result omits it.
	for _, hit := range modelHits {
		consumed[hit.Hit] = struct{}{}
	}
	models := collapseModels(modelHits)
	var unmatched []string
	for _, tok := range tokens {
		if _, ok := consumed[tok]; !ok {
			unmatched = append(unmatched, tok)
		}
	}
	return MatchResult{
		Query:     query,
		Tokens:    tokens,
		Hosts:     hosts,
		Models:    models,
		Unmatched: unmatched,
		Placement: advisePlacement(hosts, catalog.ThisHost, catalog.PlacementKind),
	}
}

func matchHosts(tokens []string, hosts []HostRecord) []HostHit {
	best := map[string]HostHit{}
	for _, host := range hosts {
		keywords := append([]string{host.ID}, host.Aliases...)
		for _, tok := range tokens {
			score, kind := scoreToken(tok, keywords)
			if score == 0 {
				continue
			}
			prev, ok := best[host.ID]
			if !ok || score > prev.Score {
				best[host.ID] = HostHit{ID: host.ID, Score: score, Hit: tok, Kind: kind}
			}
		}
	}
	out := make([]HostHit, 0, len(best))
	for _, hit := range best {
		out = append(out, hit)
	}
	out = filterHostHitsByTokenQuality(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}

func matchModels(tokens []string, models []ModelRecord) []ModelHit {
	type key struct{ adapter, model string }
	best := map[key]ModelHit{}
	for _, model := range models {
		keywords := append([]string(nil), model.Aliases...)
		if model.Model == "" {
			// Family records own adapter/family vocabulary. Keep that reviewed
			// vocabulary exact so ordinary prose such as "open" or "code" does
			// not become an OpenAI/Codex selector.
			keywords = append([]string{model.Adapter}, keywords...)
		} else {
			// A concrete preference is selected by its model slug or explicit
			// aliases, never merely because it shares an adapter with another
			// preferred model.
			keywords = append([]string{model.Model}, keywords...)
		}
		for _, tok := range tokens {
			score, kind := scoreToken(tok, keywords)
			if model.Model == "" && kind != "exact" {
				continue
			}
			if score == 0 {
				continue
			}
			k := key{model.Adapter, model.Model}
			prev, ok := best[k]
			if !ok || score > prev.Score {
				best[k] = ModelHit{Adapter: model.Adapter, Model: model.Model, Speed: emitSpeed(model.Speed), Score: score, Hit: tok, Kind: kind}
			}
		}
	}
	out := make([]ModelHit, 0, len(best))
	for _, hit := range best {
		out = append(out, hit)
	}
	out = filterModelHitsByTokenQuality(out)
	sort.Slice(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Adapter != out[j].Adapter {
			return out[i].Adapter < out[j].Adapter
		}
		return out[i].Model < out[j].Model
	})
	return out
}

func filterHostHitsByTokenQuality(hits []HostHit) []HostHit {
	bestByToken := map[string]int{}
	for _, hit := range hits {
		if hit.Score > bestByToken[hit.Hit] {
			bestByToken[hit.Hit] = hit.Score
		}
	}
	out := hits[:0]
	for _, hit := range hits {
		if hit.Score == bestByToken[hit.Hit] {
			out = append(out, hit)
		}
	}
	return out
}

func filterModelHitsByTokenQuality(hits []ModelHit) []ModelHit {
	bestByToken := map[string]int{}
	for _, hit := range hits {
		if hit.Score > bestByToken[hit.Hit] {
			bestByToken[hit.Hit] = hit.Score
		}
	}
	out := hits[:0]
	for _, hit := range hits {
		if hit.Score == bestByToken[hit.Hit] {
			out = append(out, hit)
		}
	}
	return out
}

func scoreToken(token string, keywords []string) (int, string) {
	best, kind := 0, ""
	for _, raw := range keywords {
		kw := normalizeKeyword(raw)
		if kw == "" {
			continue
		}
		switch {
		case token == kw:
			return scoreExact, "exact"
		case len(token) >= minFuzzyToken && strings.HasPrefix(kw, token):
			if scorePrefix > best {
				best, kind = scorePrefix, "prefix"
			}
		case len(token) >= minFuzzyToken && strings.Contains(kw, token):
			if scoreSubstring > best {
				best, kind = scoreSubstring, "substring"
			}
		}
	}
	return best, kind
}

func normalizeKeyword(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	value = strings.ReplaceAll(value, "_", "-")
	return strings.Trim(value, ".:;!?\"'`")
}

func emitSpeed(speed string) string {
	speed = strings.TrimSpace(speed)
	if strings.EqualFold(speed, "regular") {
		return ""
	}
	return speed
}

func advisePlacement(hosts []HostHit, thisHost, kind string) PlacementAdvice {
	thisHost = normalizeKeyword(thisHost)
	kind = strings.TrimSpace(kind)
	if len(hosts) == 0 {
		return PlacementAdvice{Mode: "no_host", Reason: "no_host_token"}
	}
	top := hosts[0].Score
	ids := uniqueTopHostIDs(hosts, top)
	if len(ids) != 1 {
		return PlacementAdvice{Mode: "ambiguous_host", Reason: "multiple_top_hosts"}
	}
	id := ids[0]
	if thisHost == "" {
		return PlacementAdvice{Mode: "need_this_host", Host: id, Reason: "this_host_unset"}
	}
	if normalizeKeyword(id) == thisHost {
		return PlacementAdvice{Mode: "local", Host: id, Reason: "host_is_this_machine"}
	}
	if kind == "" {
		return PlacementAdvice{Mode: "need_placement", Host: id, Reason: "remote_host_without_placement"}
	}
	return PlacementAdvice{Mode: "remote", Kind: kind, Host: id, Reason: "host_is_not_this_machine"}
}

func uniqueTopHostIDs(hosts []HostHit, top int) []string {
	seen := map[string]struct{}{}
	var ids []string
	for _, hit := range hosts {
		if hit.Score != top {
			break
		}
		if _, ok := seen[hit.ID]; ok {
			continue
		}
		seen[hit.ID] = struct{}{}
		ids = append(ids, hit.ID)
	}
	return ids
}

func collapseModels(hits []ModelHit) []ModelHit {
	bestSlug := map[string]int{}
	for _, hit := range hits {
		if hit.Model != "" && hit.Score >= bestSlug[hit.Adapter] {
			bestSlug[hit.Adapter] = hit.Score
		}
	}
	out := hits[:0]
	for _, hit := range hits {
		if hit.Model == "" {
			if score, ok := bestSlug[hit.Adapter]; ok && score >= hit.Score {
				continue
			}
		}
		out = append(out, hit)
	}
	return out
}

// NewCatalog builds a catalog from optional config. Built-in family aliases
// are always included; preferred[] adds concrete model slugs.
func NewCatalog(thisHost string, hosts map[string]string, preferred []ModelRecord, placementKind string) Catalog {
	catalog := Catalog{ThisHost: strings.TrimSpace(thisHost), PlacementKind: strings.TrimSpace(placementKind)}
	byID := map[string]*HostRecord{}
	addHost := func(id string, alias string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		rec := byID[id]
		if rec == nil {
			rec = &HostRecord{ID: id}
			byID[id] = rec
		}
		if alias = strings.TrimSpace(alias); alias != "" && alias != id {
			rec.Aliases = appendUnique(rec.Aliases, alias)
		}
	}
	if thisHost != "" {
		addHost(thisHost, "")
	}
	for alias, id := range hosts {
		if strings.TrimSpace(id) == "" {
			addHost(alias, "")
			continue
		}
		addHost(id, alias)
	}
	for _, rec := range byID {
		catalog.Hosts = append(catalog.Hosts, *rec)
	}
	sort.Slice(catalog.Hosts, func(i, j int) bool { return catalog.Hosts[i].ID < catalog.Hosts[j].ID })

	catalog.Models = append(catalog.Models, BuiltinModelCatalog()...)
	catalog.Models = append(catalog.Models, preferred...)
	return catalog
}

func ParseUseForAliases(useFor string) []string {
	useFor = strings.TrimSpace(useFor)
	if !strings.HasPrefix(useFor, "alias:") {
		return nil
	}
	useFor = strings.TrimPrefix(useFor, "alias:")
	var out []string
	for _, part := range strings.Split(useFor, ",") {
		part = strings.TrimSpace(part)
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

func appendUnique(in []string, value string) []string {
	for _, existing := range in {
		if existing == value {
			return in
		}
	}
	return append(in, value)
}
