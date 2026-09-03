package route

import "strings"

// HasConcretePreferences reports whether the catalog is a reviewed
// adapter+model table rather than the built-in family fallback.
func HasConcretePreferences(catalog Catalog) bool {
	for _, rec := range catalog.Models {
		if strings.TrimSpace(rec.Model) != "" {
			return true
		}
	}
	return false
}

// InPreferredTable reports whether adapter+model is an exact reviewed
// preference or an explicit use_for alias. It never remaps to a different
// adapter or model.
func InPreferredTable(records []ModelRecord, adapter, model string) bool {
	adapter = normalizeKeyword(adapter)
	model = normalizeKeyword(model)
	if adapter == "" || model == "" {
		return false
	}
	for _, rec := range records {
		if strings.TrimSpace(rec.Model) == "" {
			continue
		}
		if normalizeKeyword(rec.Adapter) != adapter {
			continue
		}
		if normalizeKeyword(rec.Model) == model {
			return true
		}
		for _, alias := range rec.Aliases {
			if normalizeKeyword(alias) == model {
				return true
			}
		}
	}
	return false
}

// NativeArgvModel extracts an explicit native model flag from argv. It does
// not invent a default or remap aliases onto a different slug.
func NativeArgvModel(adapter string, argv []string) string {
	allowShort := strings.EqualFold(strings.TrimSpace(adapter), "codex")
	for i := 1; i < len(argv); i++ {
		arg := argv[i]
		if arg == "--" {
			break
		}
		switch {
		case arg == "--model" && i+1 < len(argv):
			return strings.TrimSpace(argv[i+1])
		case strings.HasPrefix(arg, "--model="):
			return strings.TrimSpace(strings.TrimPrefix(arg, "--model="))
		case allowShort && arg == "-m" && i+1 < len(argv):
			return strings.TrimSpace(argv[i+1])
		case allowShort && strings.HasPrefix(arg, "-m=") && len(arg) > 3:
			return strings.TrimSpace(arg[3:])
		}
	}
	return ""
}
