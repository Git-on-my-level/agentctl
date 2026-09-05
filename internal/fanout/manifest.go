// Package fanout validates explicit delegation batches and bounds their
// same-process execution. It owns no durable state, harness flags, or retries.
package fanout

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

const (
	MaxManifestBytes = 1 << 20
	MaxChildren      = 64
	MaxConcurrency   = 16
	MaxLabels        = 16
)

type Manifest struct {
	SchemaVersion  int      `json:"schema_version"`
	PromptFile     string   `json:"prompt_file,omitempty"`
	PromptDelivery string   `json:"prompt_delivery,omitempty"`
	Concurrency    int      `json:"concurrency,omitempty"`
	Labels         []string `json:"labels,omitempty"`
	Children       []Child  `json:"children"`
}

type Child struct {
	Name                  string   `json:"name,omitempty"`
	ExecutionID           string   `json:"execution_id,omitempty"`
	Adapter               string   `json:"adapter,omitempty"`
	CWD                   string   `json:"cwd,omitempty"`
	Timeout               string   `json:"timeout,omitempty"`
	PromptFile            string   `json:"prompt_file,omitempty"`
	PromptDelivery        string   `json:"prompt_delivery,omitempty"`
	Labels                []string `json:"labels,omitempty"`
	NoStoreResult         bool     `json:"no_store_result,omitempty"`
	AllowMissingResult    bool     `json:"allow_missing_result,omitempty"`
	AllowUnreliableResult bool     `json:"allow_unreliable_result,omitempty"`
	Argv                  []string `json:"argv"`
}

// Decode accepts exactly one bounded UTF-8 JSON document. Nulls and duplicate
// keys are rejected rather than letting different consumers interpret a batch
// differently. Errors never include argv or prompt values.
func Decode(body []byte) (Manifest, error) {
	var m Manifest
	if len(body) > MaxManifestBytes {
		return m, errors.New("fanout manifest exceeds 1 MiB limit")
	}
	if !utf8.Valid(body) {
		return m, errors.New("fanout manifest must be valid UTF-8")
	}
	check := json.NewDecoder(bytes.NewReader(body))
	check.UseNumber()
	if err := checkValue(check, 0, ""); err != nil {
		return m, err
	}
	if _, err := check.Token(); err != io.EOF {
		return m, errors.New("fanout manifest must contain exactly one JSON document")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&m); err != nil {
		return m, fmt.Errorf("decode fanout manifest: %w", err)
	}
	return m, m.Validate()
}

func checkValue(d *json.Decoder, depth int, field string) error {
	if depth > 16 {
		return errors.New("fanout manifest JSON is too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return fmt.Errorf("decode fanout manifest: %w", err)
	}
	if token == nil {
		return errors.New("fanout manifest must not contain null values")
	}
	if value, ok := token.(string); ok && field != "" && value == "" {
		return errors.New("fanout manifest string fields must not be empty")
	}
	switch token {
	case json.Delim('{'):
		seen := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			if err != nil {
				return fmt.Errorf("decode fanout manifest object: %w", err)
			}
			name, ok := key.(string)
			if !ok {
				return errors.New("fanout manifest object key must be a string")
			}
			// Go matches struct fields case-insensitively; JSON Schema does not.
			if name != strings.ToLower(name) {
				return errors.New("fanout manifest keys must use their exact lowercase spelling")
			}
			if seen[name] {
				return errors.New("fanout manifest contains a duplicate object key")
			}
			seen[name] = true
			if err := checkValue(d, depth+1, name); err != nil {
				return err
			}
		}
		_, err = d.Token()
	case json.Delim('['):
		for d.More() {
			if err := checkValue(d, depth+1, ""); err != nil {
				return err
			}
		}
		_, err = d.Token()
	}
	return err
}

func (m Manifest) Validate() error {
	if m.SchemaVersion != 1 {
		return errors.New("fanout manifest schema_version must be 1")
	}
	if m.Concurrency < 0 || m.Concurrency > MaxConcurrency {
		return errors.New("fanout concurrency must be between 1 and 16 (or 0 for the default)")
	}
	if len(m.Children) < 1 || len(m.Children) > MaxChildren {
		return errors.New("fanout manifest must contain between 1 and 64 children")
	}
	if !validDelivery(m.PromptDelivery) {
		return errors.New("fanout prompt_delivery must be argv or stdin")
	}
	if m.PromptFile != "" && strings.TrimSpace(m.PromptFile) == "" {
		return errors.New("fanout prompt_file must not be blank")
	}
	if err := validateLabels(m.Labels); err != nil {
		return err
	}
	seenIDs, seenNames := map[string]bool{}, map[string]bool{}
	for i, child := range m.Children {
		if err := validateChild(child); err != nil {
			return fmt.Errorf("child %d: %w", i, err)
		}
		if m.PromptFile == "" && child.PromptFile == "" {
			return fmt.Errorf("child %d: requires prompt_file on the child or manifest", i)
		}
		if child.ExecutionID != "" {
			if _, err := ids.ParseExecutionID(child.ExecutionID); err != nil {
				return fmt.Errorf("child %d: execution_id must be a valid exec ID", i)
			}
			if seenIDs[child.ExecutionID] {
				return fmt.Errorf("child %d: execution_id values must be unique", i)
			}
			seenIDs[child.ExecutionID] = true
		}
		if child.Name != "" {
			if seenNames[child.Name] {
				return fmt.Errorf("child %d: name values must be unique", i)
			}
			seenNames[child.Name] = true
		}
		if len(MergeLabels(m.Labels, child.Labels)) > MaxLabels {
			return fmt.Errorf("child %d: at most 16 combined manifest and child labels are allowed", i)
		}
	}
	return nil
}

func validateChild(c Child) error {
	if c.Name != "" && !validLabel(c.Name) {
		return errors.New("name must match ^[a-z][a-z0-9_.-]{0,63}$")
	}
	if len(c.Argv) < 1 || strings.TrimSpace(c.Argv[0]) == "" || len(c.Argv) > 256 {
		return errors.New("requires non-empty argv with at most 256 entries")
	}
	for _, arg := range c.Argv {
		if strings.IndexByte(arg, 0) >= 0 {
			return errors.New("argv must not contain NUL bytes")
		}
	}
	if c.PromptFile != "" && strings.TrimSpace(c.PromptFile) == "" {
		return errors.New("prompt_file must not be blank")
	}
	if !validDelivery(c.PromptDelivery) {
		return errors.New("prompt_delivery must be argv or stdin")
	}
	if c.Timeout != "" {
		value, err := time.ParseDuration(c.Timeout)
		if err != nil || value <= 0 {
			return errors.New("timeout must be a positive Go duration")
		}
	}
	return validateLabels(c.Labels)
}

func validDelivery(v string) bool { return v == "" || v == "argv" || v == "stdin" }

func validateLabels(labels []string) error {
	if len(labels) > MaxLabels {
		return errors.New("at most 16 labels are allowed")
	}
	seen := map[string]bool{}
	for _, label := range labels {
		if !validLabel(label) || seen[label] {
			return errors.New("labels must be unique and match ^[a-z][a-z0-9_.-]{0,63}$")
		}
		seen[label] = true
	}
	return nil
}

func validLabel(label string) bool {
	if len(label) == 0 || len(label) > 64 || label[0] < 'a' || label[0] > 'z' {
		return false
	}
	for _, c := range label[1:] {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '.' || c == '-') {
			return false
		}
	}
	return true
}

// MergeLabels returns a stable union: manifest labels first, child labels next.
// A shared label repeated across the two scopes is retained only once.
func MergeLabels(shared, child []string) []string {
	result := make([]string, 0, len(shared)+len(child))
	seen := map[string]bool{}
	for _, labels := range [][]string{shared, child} {
		for _, label := range labels {
			if !seen[label] {
				seen[label] = true
				result = append(result, label)
			}
		}
	}
	return result
}
