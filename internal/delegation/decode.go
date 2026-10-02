package delegation

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRequestBytes       = 64 << 10
	MaxKeyChars           = 256
	maxJSONDepth          = 8
	maxDiagnosticKeyChars = 64
)

var allowedKeys = map[string]map[string]struct{}{
	"$":                   {"schema_version": {}, "request_key": {}, "selector": {}},
	"$.selector":          {"harness": {}, "family": {}, "version": {}, "model": {}, "host": {}, "settings": {}},
	"$.selector.settings": {"speed": {}, "effort": {}, "access": {}},
}

// DecodeRequest accepts exactly one bounded UTF-8 JSON object. Unknown,
// case-aliased, and duplicate keys, nulls, empty optional strings, control
// characters, invalid UTF-8, and trailing documents are rejected.
func DecodeRequest(body []byte) (Request, error) {
	var req Request
	if len(body) > MaxRequestBytes {
		return Request{}, requestError("size_limit", "delegation request exceeds 64 KiB limit", "$", "$")
	}
	if !utf8.Valid(body) {
		return Request{}, requestError("invalid_utf8", "delegation request must be valid UTF-8", "$", "$")
	}
	check := json.NewDecoder(bytes.NewReader(body))
	check.UseNumber()
	if err := checkValue(check, 0, "$", "$"); err != nil {
		return Request{}, err
	}
	if _, err := check.Token(); err != io.EOF {
		return Request{}, requestError("trailing_document", "delegation request must contain exactly one JSON document", "$", "$")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return Request{}, requestError("invalid_json", "delegation request is not valid JSON", "$", "$")
	}
	if err := validateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

func checkValue(d *json.Decoder, depth int, path, scope string) error {
	fail := func(violation, diagnostic string) error { return requestError(violation, diagnostic, path, scope) }
	if depth > maxJSONDepth {
		return fail("depth_limit", "delegation request JSON is too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return fail("invalid_json", "delegation request is not valid JSON")
	}
	if token == nil {
		return fail("null_value", "delegation request must not contain null values")
	}
	_, objectExpected := allowedKeys[path]
	if value, ok := token.(json.Delim); ok {
		if value == '{' && objectExpected {
			return checkObject(d, depth, path)
		}
		if value == '[' {
			return fail("invalid_type", "delegation request must not contain arrays")
		}
		return fail("invalid_type", "delegation request field has an invalid type")
	}
	if objectExpected {
		return fail("invalid_type", "delegation request field must be an object")
	}
	if path == "$.schema_version" {
		if number, ok := token.(json.Number); !ok || string(number) != "1" {
			return fail("schema_version", "schema_version must be the integer 1")
		}
		return nil
	}
	value, ok := token.(string)
	if !ok {
		return fail("invalid_type", "delegation request field must be a string")
	}
	if strings.TrimSpace(value) == "" {
		return fail("empty_string", "delegation request string fields must not be empty")
	}
	if utf8.RuneCountInString(value) > MaxKeyChars {
		return fail("string_limit", "delegation request string exceeds 256 characters")
	}
	if containsControl(value) {
		return fail("control_character", "delegation request must not contain control characters")
	}
	return nil
}

func checkObject(d *json.Decoder, depth int, path string) error {
	seen := map[string]struct{}{}
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return requestError("invalid_json", "delegation request is not valid JSON", path, path)
		}
		name, ok := keyToken.(string)
		if !ok {
			return requestError("invalid_type", "delegation request object key must be a string", path, path)
		}
		fieldPath := path + "." + diagnosticKey(name)
		if utf8.RuneCountInString(name) > MaxKeyChars {
			return requestError("key_limit", "delegation request object key exceeds 256 characters", fieldPath, path)
		}
		if containsControl(name) {
			return requestError("control_character", "delegation request must not contain control characters", fieldPath, path)
		}
		if name != strings.ToLower(name) {
			return requestError("key_spelling", "delegation request keys must use their exact documented spelling", fieldPath, path)
		}
		if _, ok := allowedKeys[path][name]; !ok {
			return requestError("unknown_field", "delegation request contains an unknown field", fieldPath, path)
		}
		if _, dup := seen[name]; dup {
			return requestError("duplicate_key", "delegation request contains a duplicate object key", fieldPath, path)
		}
		seen[name] = struct{}{}
		if err := checkValue(d, depth+1, fieldPath, path); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return requestError("invalid_json", "delegation request is not valid JSON", path, path)
	}
	return nil
}

// Only short ASCII identifier-shaped unknown keys are diagnostic metadata.
// Arbitrary text, punctuation, Unicode, and long keys are never echoed.
func diagnosticKey(name string) string {
	if len(name) == 0 || len(name) > maxDiagnosticKeyChars {
		return "<unknown>"
	}
	for i, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_' || (i > 0 && r >= '0' && r <= '9') {
			continue
		}
		return "<unknown>"
	}
	return name
}

func requestError(violation, diagnostic, path, scope string) *Error {
	keys := make([]string, 0, len(allowedKeys[scope]))
	for key := range allowedKeys[scope] {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return &Error{Code: "delegate_invalid_request", Kind: KindUsage, Diagnostic: diagnostic + " at " + path,
		Violation: violation, FieldPath: path, AllowedKeys: keys, ExampleRequest: &Request{
			SchemaVersion: 1, RequestKey: "<unique-request-key>", Selector: Selector{Model: "<configured-model>"},
		}}
}

func validateRequest(req Request) error {
	if req.SchemaVersion != 1 {
		return requestError("schema_version", "schema_version must be the integer 1", "$.schema_version", "$")
	}
	if strings.TrimSpace(req.RequestKey) == "" {
		return requestError("required_field", "request_key is required", "$.request_key", "$")
	}
	if !selectorConstraintPresent(req.Selector) {
		return requestError("required_constraint", "selector requires family or model", "$.selector", "$.selector")
	}
	if req.Selector.Settings != nil {
		switch strings.ToLower(strings.TrimSpace(req.Selector.Settings.Access)) {
		case "", "coding", "read_only":
		default:
			return requestError("invalid_access", "settings.access must be coding or read_only", "$.selector.settings.access", "$.selector.settings")
		}
	}
	return nil
}

func selectorConstraintPresent(sel Selector) bool {
	return strings.TrimSpace(sel.Family) != "" || strings.TrimSpace(sel.Model) != ""
}

func containsControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
