package delegation

const (
	KindUsage     = "usage"
	KindAmbiguous = "ambiguous"
)

// Error is the typed failure for decode and resolution. Error() never includes
// raw request JSON, prompt bytes, or credentials.
type Error struct {
	Code             string
	Kind             string
	Diagnostic       string
	Candidates       []Entry
	CandidateCount   int
	ConstraintFields []string
	Violation        string
	FieldPath        string
	AllowedKeys      []string
	// Suggestion names the documented location for a field that was supplied
	// at a plausible but wrong path. It is static text, never request content.
	Suggestion string
	// SuggestedPath is the documented JSON path for that field.
	SuggestedPath string
	// RepairFlag is the delegate CLI flag spelling for a field that is
	// accepted on the command line instead of in the request.
	RepairFlag string
	// RepairFlagValue is the placeholder value for RepairFlag; empty for a
	// boolean flag.
	RepairFlagValue string
	ExampleRequest  *Request
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Diagnostic
}

var _ error = (*Error)(nil)

func ambiguousError(diagnostic string, candidates []Entry) *Error {
	return selectionError("delegate_ambiguous_selector", KindAmbiguous, diagnostic, candidates)
}

// selectionError exposes only reviewed catalog tuples, never request values.
func selectionError(code, kind, diagnostic string, candidates []Entry) *Error {
	candidates = collapseEntries(candidates)
	count := len(candidates)
	if len(candidates) > 32 {
		candidates = candidates[:32]
	}
	return &Error{Code: code, Kind: kind, Diagnostic: diagnostic, Candidates: candidates, CandidateCount: count}
}
