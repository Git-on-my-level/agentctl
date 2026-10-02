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
	ExampleRequest   *Request
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
