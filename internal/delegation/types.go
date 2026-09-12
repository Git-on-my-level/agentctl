// Package delegation decodes structured selector requests and resolves them
// against an explicit configured catalog. Native CLIs and Multica remain the
// execution authorities; this package does not launch work or infer models.
package delegation

// Settings are optional service speed and effort constraints. They are not
// interchangeable; a request may supply either, both, or neither.
type Settings struct {
	Speed  string `json:"speed,omitempty"`
	Effort string `json:"effort,omitempty"`
}

// Selector is the parent-supplied constraint set. Omitted fields stay empty
// so configured defaults can fill them. Family or an exact native Model is
// required; Host is preserved for later placement and is not catalog-matched.
type Selector struct {
	Harness  string    `json:"harness,omitempty"`
	Family   string    `json:"family,omitempty"`
	Version  string    `json:"version,omitempty"`
	Model    string    `json:"model,omitempty"`
	Host     string    `json:"host,omitempty"`
	Settings *Settings `json:"settings,omitempty"`
}

// Request is one bounded structured delegation document. Prompt bytes stay in
// a separate prompt channel.
type Request struct {
	SchemaVersion int      `json:"schema_version"`
	RequestKey    string   `json:"request_key"`
	Selector      Selector `json:"selector"`
}

// Entry is one reviewed harness/model/settings tuple. Aliases identify family
// only; Version must be explicit metadata. CLI adapters may copy preferred[]
// into Entry, using the UseFor alias list as Aliases.
type Entry struct {
	Harness string   `json:"harness,omitempty"`
	Family  string   `json:"family,omitempty"`
	Version string   `json:"version,omitempty"`
	Model   string   `json:"model,omitempty"`
	Speed   string   `json:"speed,omitempty"`
	Effort  string   `json:"effort,omitempty"`
	Aliases []string `json:"aliases,omitempty"`
	Default bool     `json:"default,omitempty"`
}

// Resolution reports the exact catalog tuple that satisfied the request and
// which omitted fields were filled from that tuple or a unique default.
type Resolution struct {
	Requested Selector `json:"requested"`
	Resolved  Entry    `json:"resolved"`
	Defaulted []string `json:"defaulted,omitempty"`
}
