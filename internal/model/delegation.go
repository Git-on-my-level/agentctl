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
	RequestSHA256       string                `json:"request_sha256"`
	ConfigurationSHA256 string                `json:"configuration_sha256"`
	Requested           json.RawMessage       `json:"requested"`
	Resolved            DelegationTarget      `json:"resolved"`
	Defaulted           []string              `json:"defaulted"`
}

// DelegationNativePlan is private operational metadata. The store retains it
// in the immutable idempotency record; normal execution JSON never exposes it.
type DelegationNativePlan struct {
	Argv           []string `json:"argv"`
	PromptDelivery string   `json:"prompt_delivery"`
}

type DelegationSettings struct {
	Speed  string `json:"speed,omitempty"`
	Effort string `json:"effort,omitempty"`
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
	for _, v := range []string{b.Resolved.Model, b.Resolved.Family, b.Resolved.Version, b.Resolved.Host, b.Resolved.Settings.Speed, b.Resolved.Settings.Effort} {
		if utf8.RuneCountInString(v) > 256 || !utf8.ValidString(v) || strings.IndexFunc(v, unicode.IsControl) >= 0 {
			return errors.New("invalid delegation target value")
		}
	}
	if b.NativePlan != nil {
		if len(b.NativePlan.Argv) == 0 || len(b.NativePlan.Argv) > 64 || (b.NativePlan.PromptDelivery != "argv" && b.NativePlan.PromptDelivery != "stdin") {
			return errors.New("invalid delegation native plan")
		}
		for _, arg := range b.NativePlan.Argv {
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
		case "harness", "family", "version", "model", "host", "settings.speed", "settings.effort", "speed", "effort":
		default:
			return errors.New("invalid delegation default field")
		}
	}
	return nil
}
