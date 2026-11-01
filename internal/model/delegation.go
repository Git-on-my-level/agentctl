package model

import (
	"encoding/json"
	"errors"
	"github.com/Git-on-my-level/agentctl/internal/delegation"
	"strings"
	"unicode"
	"unicode/utf8"
)

// DelegationBinding pins resolution metadata to an admitted execution. It
// exposes no prompt, executable path, native argv, or credentials in JSON.
type DelegationBinding struct {
	NativePlan          *DelegationNativePlan `json:"-"`
	Continuation        *FollowUpContinuation `json:"continuation,omitempty"`
	RequestSHA256       string                `json:"request_sha256"`
	ConfigurationSHA256 string                `json:"configuration_sha256"`
	// RequestKey is the raw delegate request key, the stable retry key. It is
	// recorded verbatim so run-link registrations can key on it without
	// re-deriving the idempotency digest. Empty on legacy records.
	RequestKey string           `json:"request_key,omitempty"`
	Requested  json.RawMessage  `json:"requested"`
	Resolved   DelegationTarget `json:"resolved"`
	Defaulted  []string         `json:"defaulted"`
	// Capacity records a pool's account choice. Older binaries ignore it, and the
	// stored selector stays the resolved harness and model so they can still read
	// the record after a rollback.
	Capacity *DelegationCapacity `json:"capacity,omitempty"`
}

// DelegationCapacity explains which account a pool chose and why. It holds no
// executable path or credentials.
type DelegationCapacity struct {
	Pool            string               `json:"pool"`
	Mode            string               `json:"mode"`
	Account         string               `json:"account"`
	Reason          string               `json:"reason"`
	FactsStatus     string               `json:"facts_status"`
	FactsAgeSeconds *int64               `json:"facts_age_seconds,omitempty"`
	Considered      []CapacityEvaluation `json:"considered"`
}

// CapacityEvaluation is one candidate as it looked at selection time. UsedRatio
// is absent when usage was unknown.
type CapacityEvaluation struct {
	Account        string   `json:"account"`
	Model          string   `json:"model"`
	UsedRatio      *float64 `json:"used_ratio,omitempty"`
	Window         string   `json:"window,omitempty"`
	Threshold      float64  `json:"threshold"`
	CutoffExempt   bool     `json:"cutoff_exempt,omitempty"`
	Eligible       bool     `json:"eligible"`
	RecentLaunches int      `json:"recent_launches"`
}

func (c DelegationCapacity) validate() error {
	if len(c.Considered) == 0 || len(c.Considered) > 16 {
		return errors.New("invalid delegation capacity")
	}
	values := []string{c.Pool, c.Mode, c.Account, c.Reason, c.FactsStatus}
	for _, considered := range c.Considered {
		values = append(values, considered.Account, considered.Model)
		if considered.Window != "" {
			values = append(values, considered.Window)
		}
	}
	for _, v := range values {
		if v == "" || utf8.RuneCountInString(v) > 256 || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return errors.New("invalid delegation capacity")
		}
	}
	return nil
}

// FollowUpContinuation records provenance only, never carried text or instructions.
type FollowUpContinuation struct {
	Mode                string              `json:"mode"`
	NativeSessionReused bool                `json:"native_session_reused"`
	PriorResult         *CarryForwardResult `json:"prior_result,omitempty"`
}

type CarryForwardResult struct {
	Source string `json:"source"`
	SHA256 string `json:"sha256"`
	Bytes  int    `json:"bytes"`
}

// DelegationNativePlan is private operational metadata. The store retains it
// in the immutable idempotency record; normal execution JSON never exposes it.
type DelegationNativePlan struct {
	// Env pins reviewed non-secret launcher selectors alongside the argv.
	Env            []string `json:"env,omitempty"`
	Argv           []string `json:"argv"`
	PromptDelivery string   `json:"prompt_delivery"`
	Permissions    string   `json:"permissions,omitempty"`
	// RecipeArgv is set on a follow-up turn, whose Argv already resumes a
	// native session. It keeps the original reviewed recipe so the next turn
	// is built from that recipe rather than from an argv that resumes.
	RecipeArgv []string `json:"recipe_argv,omitempty"`
}

// Recipe returns the prompt-free, session-free argv a follow-up turn starts from.
func (p DelegationNativePlan) Recipe() []string {
	if len(p.RecipeArgv) != 0 {
		return p.RecipeArgv
	}
	return p.Argv
}

type DelegationSettings struct {
	Speed             string `json:"speed,omitempty"`
	Effort            string `json:"effort,omitempty"`
	EffortWarning     string `json:"effort_warning,omitempty"`
	Access            string `json:"access,omitempty"`
	AutoCompactWindow string `json:"auto_compact_window,omitempty"`
}

type DelegationTarget struct {
	Harness   string             `json:"harness"`
	Family    string             `json:"family,omitempty"`
	Version   string             `json:"version,omitempty"`
	Model     string             `json:"model"`
	Host      string             `json:"host"`
	Authority Authority          `json:"authority"`
	Settings  DelegationSettings `json:"settings"`
}

func (b DelegationBinding) Validate() error {
	if !hashPattern.MatchString(b.RequestSHA256) || !hashPattern.MatchString(b.ConfigurationSHA256) {
		return errors.New("invalid delegation digest")
	}
	if b.RequestKey != "" && (utf8.RuneCountInString(b.RequestKey) > 256 || !utf8.ValidString(b.RequestKey) || strings.IndexFunc(b.RequestKey, unicode.IsControl) >= 0) {
		return errors.New("invalid delegation request key")
	}
	if len(b.Requested) == 0 || len(b.Requested) > 65536 || !json.Valid(b.Requested) || b.Requested[0] != '{' {
		return errors.New("invalid delegation selector")
	}
	request := append([]byte(`{"schema_version":1,"request_key":"stored","selector":`), b.Requested...)
	request = append(request, '}')
	if _, err := delegation.DecodeRequest(request); err != nil {
		return errors.New("invalid delegation selector")
	}
	if b.Resolved.Authority != AuthorityNative && b.Resolved.Authority != AuthorityMultica {
		return errors.New("invalid delegation authority")
	}
	if !adapterPattern.MatchString(b.Resolved.Harness) || b.Resolved.Model == "" || b.Resolved.Host == "" {
		return errors.New("incomplete delegation target")
	}
	for _, v := range []string{b.Resolved.Model, b.Resolved.Family, b.Resolved.Version, b.Resolved.Host, b.Resolved.Settings.Speed, b.Resolved.Settings.Effort, b.Resolved.Settings.EffortWarning, b.Resolved.Settings.Access, b.Resolved.Settings.AutoCompactWindow} {
		if utf8.RuneCountInString(v) > 256 || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return errors.New("invalid delegation target value")
		}
	}
	if b.Capacity != nil {
		if err := b.Capacity.validate(); err != nil {
			return err
		}
	}
	if c := b.Continuation; c != nil {
		if c.Mode != "native" && c.Mode != "carry_forward" {
			return errors.New("invalid continuation mode")
		}
		if c.NativeSessionReused != (c.Mode == "native") {
			return errors.New("continuation session claim disagrees with mode")
		}
		if c.Mode == "native" && c.PriorResult != nil {
			return errors.New("native continuation cannot carry a result")
		}
		if c.Mode == "carry_forward" {
			if c.PriorResult == nil || c.PriorResult.Source == "" || len(c.PriorResult.Source) > 128 || !hashPattern.MatchString(c.PriorResult.SHA256) || c.PriorResult.Bytes < 1 || c.PriorResult.Bytes > 64<<10 {
				return errors.New("invalid carry-forward provenance")
			}
		}
	}
	if b.NativePlan != nil {
		if len(b.NativePlan.Argv) == 0 || len(b.NativePlan.Argv) > 64 || (b.NativePlan.PromptDelivery != "argv" && b.NativePlan.PromptDelivery != "stdin" && b.NativePlan.PromptDelivery != "stream") {
			return errors.New("invalid delegation native plan")
		}
		if len(b.NativePlan.Env) > 32 {
			return errors.New("invalid delegation native environment")
		}
		for _, entry := range b.NativePlan.Env {
			key, _, ok := strings.Cut(entry, "=")
			if !ok || key == "" || len(entry) > 4096 || strings.ContainsAny(entry, "\x00\r\n") {
				return errors.New("invalid delegation native environment")
			}
		}
		if len(b.NativePlan.RecipeArgv) > 64 {
			return errors.New("invalid delegation native plan")
		}
		for _, arg := range append(append([]string(nil), b.NativePlan.Argv...), b.NativePlan.RecipeArgv...) {
			if len(arg) > 4096 || strings.ContainsAny(arg, "\x00\r\n") {
				return errors.New("invalid delegation native argument")
			}
		}
	}
	if len(b.Defaulted) > 8 {
		return errors.New("too many delegation defaults")
	}
	for _, field := range b.Defaulted {
		switch field {
		case "harness", "family", "version", "model", "host", "settings.speed", "settings.effort", "settings.access", "speed", "effort":
		default:
			return errors.New("invalid delegation default field")
		}
	}
	return nil
}
