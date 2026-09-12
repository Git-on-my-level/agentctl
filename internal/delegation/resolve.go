package delegation

import (
	"sort"
	"strings"
)

// Resolve intersects selector constraints against configured entries and fills
// omitted harness/version/settings only from a unique remaining tuple or a
// single compatible default. Candidates are sorted; catalog slice order cannot
// change the chosen tuple. Aliases identify family; version matches metadata
// only. Model IDs match exactly after trimming, without case folding.
func Resolve(sel Selector, entries []Entry) (Resolution, error) {
	requested := cloneSelector(sel)
	if !selectorConstraintPresent(sel) {
		return Resolution{}, usageError("selector requires family or model")
	}
	remaining := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		if matches(sel, entry) {
			remaining = append(remaining, cloneEntry(entry))
		}
	}
	collapsed := collapseEntries(remaining)
	if len(collapsed) == 0 {
		candidates := cloneEntries(entries)
		sortEntries(candidates)
		if len(candidates) > 32 {
			candidates = candidates[:32]
		}
		return Resolution{}, &Error{Code: "delegate_no_matching_tuple", Kind: KindUsage, Diagnostic: "selector matched no configured tuple", Candidates: candidates}
	}
	chosen, err := chooseEntry(collapsed)
	if err != nil {
		return Resolution{}, err
	}
	return Resolution{
		Requested: requested,
		Resolved:  chosen,
		Defaulted: defaultedFields(sel, chosen),
	}, nil
}

func matches(sel Selector, entry Entry) bool {
	if constraint := strings.TrimSpace(sel.Harness); constraint != "" && !idEqual(entry.Harness, constraint) {
		return false
	}
	if constraint := strings.TrimSpace(sel.Family); constraint != "" && !matchesFamily(entry, constraint) {
		return false
	}
	if constraint := strings.TrimSpace(sel.Version); constraint != "" {
		if strings.TrimSpace(entry.Version) == "" || !idEqual(entry.Version, constraint) {
			return false
		}
	}
	if constraint := strings.TrimSpace(sel.Model); constraint != "" && strings.TrimSpace(entry.Model) != constraint {
		return false
	}
	if sel.Settings == nil {
		return true
	}
	if constraint := strings.TrimSpace(sel.Settings.Speed); constraint != "" && !idEqual(entry.Speed, constraint) {
		return false
	}
	if constraint := strings.TrimSpace(sel.Settings.Effort); constraint != "" && !idEqual(entry.Effort, constraint) {
		return false
	}
	return true
}

func matchesFamily(entry Entry, family string) bool {
	if idEqual(entry.Family, family) {
		return true
	}
	for _, alias := range entry.Aliases {
		if idEqual(alias, family) {
			return true
		}
	}
	return false
}

func idEqual(value, constraint string) bool {
	normalizedValue := normalizeID(value)
	return normalizedValue != "" && normalizedValue == normalizeID(constraint)
}

func normalizeID(value string) string {
	return strings.ToLower(strings.TrimSpace(value))
}

func chooseEntry(entries []Entry) (Entry, error) {
	if len(entries) == 1 {
		return cloneEntry(entries[0]), nil
	}
	var defaults []Entry
	for _, entry := range entries {
		if entry.Default {
			defaults = append(defaults, entry)
		}
	}
	if len(defaults) == 1 {
		return cloneEntry(defaults[0]), nil
	}
	sortEntries(entries)
	if len(defaults) > 1 {
		sortEntries(defaults)
		return Entry{}, ambiguousError("selector matched multiple configured defaults", defaults)
	}
	return Entry{}, ambiguousError("selector matched multiple configured tuples", entries)
}

func collapseEntries(entries []Entry) []Entry {
	index := make(map[string]int, len(entries))
	out := make([]Entry, 0, len(entries))
	for _, entry := range entries {
		key := entryIdentity(entry)
		if i, ok := index[key]; ok {
			if entry.Default {
				out[i].Default = true
			}
			out[i].Aliases = mergeAliases(out[i].Aliases, entry.Aliases)
			continue
		}
		index[key] = len(out)
		out = append(out, cloneEntry(entry))
	}
	sortEntries(out)
	return out
}

func defaultedFields(sel Selector, got Entry) []string {
	var fields []string
	if strings.TrimSpace(sel.Harness) == "" && strings.TrimSpace(got.Harness) != "" {
		fields = append(fields, "harness")
	}
	if strings.TrimSpace(sel.Family) == "" && strings.TrimSpace(got.Family) != "" {
		fields = append(fields, "family")
	}
	if strings.TrimSpace(sel.Version) == "" && strings.TrimSpace(got.Version) != "" {
		fields = append(fields, "version")
	}
	if strings.TrimSpace(sel.Model) == "" && strings.TrimSpace(got.Model) != "" {
		fields = append(fields, "model")
	}
	if strings.TrimSpace(selectorSpeed(sel)) == "" && strings.TrimSpace(got.Speed) != "" {
		fields = append(fields, "speed")
	}
	if strings.TrimSpace(selectorEffort(sel)) == "" && strings.TrimSpace(got.Effort) != "" {
		fields = append(fields, "effort")
	}
	return fields
}

func selectorSpeed(sel Selector) string {
	if sel.Settings == nil {
		return ""
	}
	return sel.Settings.Speed
}

func selectorEffort(sel Selector) string {
	if sel.Settings == nil {
		return ""
	}
	return sel.Settings.Effort
}

func cloneSelector(sel Selector) Selector {
	out := sel
	if sel.Settings != nil {
		settings := *sel.Settings
		out.Settings = &settings
	}
	return out
}

func cloneEntry(entry Entry) Entry {
	entry.Aliases = append([]string(nil), entry.Aliases...)
	return entry
}

func cloneEntries(entries []Entry) []Entry {
	out := make([]Entry, len(entries))
	for i, entry := range entries {
		out[i] = cloneEntry(entry)
	}
	return out
}

func sortEntries(entries []Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		return entryIdentity(entries[i]) < entryIdentity(entries[j])
	})
}

func entryIdentity(entry Entry) string {
	return strings.Join([]string{
		entry.Harness, entry.Family, entry.Version, entry.Model, entry.Speed, entry.Effort,
	}, "\x00")
}

func mergeAliases(a, b []string) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	out := make([]string, 0, len(a)+len(b))
	for _, alias := range append(append([]string{}, a...), b...) {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			continue
		}
		if _, ok := seen[alias]; ok {
			continue
		}
		seen[alias] = struct{}{}
		out = append(out, alias)
	}
	sort.Strings(out)
	return out
}
