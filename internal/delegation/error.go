package delegation

const (
	KindUsage     = "usage"
	KindAmbiguous = "ambiguous"
)

// Error is the typed failure for decode and resolution. Error() never includes
// raw request JSON, prompt bytes, or credentials.
type Error struct {
	Code       string
	Kind       string
	Diagnostic string
	Candidates []Entry
}

func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Diagnostic
}

var _ error = (*Error)(nil)

func usageError(diagnostic string) *Error {
	return &Error{Code: "delegate_invalid_request", Kind: KindUsage, Diagnostic: diagnostic}
}

func ambiguousError(diagnostic string, candidates []Entry) *Error {
	return &Error{Code: "delegate_ambiguous_selector", Kind: KindAmbiguous, Diagnostic: diagnostic, Candidates: cloneEntries(candidates)}
}
