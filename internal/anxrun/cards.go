package anxrun

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// CardLabelPrefix is the execution-label namespace that opts a run into ANX
// run links. The label is the operator-facing, typed, LLM-friendly contract;
// an explicit --anx-card override is only sugar for adding the same label.
const CardLabelPrefix = "anx.card."

// cardRefPrefix is the ANX card reference form used in registrations.
const cardRefPrefix = "card:"

// cardSlugPattern bounds the card slug carried in an anx.card.<slug> label.
// It accepts the same character class as agentctl execution labels.
var cardSlugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)

// ParseCardSlug validates one card slug (without prefix).
func ParseCardSlug(slug string) error {
	if !cardSlugPattern.MatchString(slug) || sensitiveText(slug) {
		return fmt.Errorf("card slug must be 1-64 lowercase letters, digits or hyphens without credential shapes")
	}
	return nil
}

// ParseCardOverride accepts `card:<slug>` or a bare `<slug>` from --anx-card
// and returns the normalized label form `anx.card.<slug>`.
func ParseCardOverride(value string) (string, error) {
	trimmed := strings.TrimSpace(value)
	slug := strings.TrimPrefix(trimmed, cardRefPrefix)
	if err := ParseCardSlug(slug); err != nil {
		return "", fmt.Errorf("card override: %w", err)
	}
	return CardLabelPrefix + slug, nil
}

// ResolveCards returns the sorted, deduplicated `card:<slug>` references for
// one execution, derived from its anx.card.<slug> labels. Labels stay the
// recorded contract; this projection is deterministic.
func ResolveCards(labels []string) []string {
	seen := map[string]struct{}{}
	cards := []string{}
	for _, label := range labels {
		if !strings.HasPrefix(label, CardLabelPrefix) {
			continue
		}
		slug := strings.TrimPrefix(label, CardLabelPrefix)
		if err := ParseCardSlug(slug); err != nil {
			continue
		}
		ref := cardRefPrefix + slug
		if _, exists := seen[ref]; exists {
			continue
		}
		seen[ref] = struct{}{}
		cards = append(cards, ref)
	}
	sort.Strings(cards)
	return cards
}

// LabeledReports whether any label opts this execution into run links.
func Labeled(labels []string) bool {
	return len(ResolveCards(labels)) > 0
}
