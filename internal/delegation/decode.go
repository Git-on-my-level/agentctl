package delegation

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	MaxRequestBytes = 64 << 10
	MaxKeyChars     = 256
	maxJSONDepth    = 8
)

var allowedKeys = map[string]map[string]struct{}{
	"":         {"schema_version": {}, "request_key": {}, "selector": {}},
	"selector": {"harness": {}, "family": {}, "version": {}, "model": {}, "host": {}, "settings": {}},
	"settings": {"speed": {}, "effort": {}},
}

// DecodeRequest accepts exactly one bounded UTF-8 JSON object. Unknown,
// case-aliased, and duplicate keys, nulls, empty optional strings, control
// characters, invalid UTF-8, and trailing documents are rejected.
func DecodeRequest(body []byte) (Request, error) {
	var req Request
	if len(body) > MaxRequestBytes {
		return Request{}, usageError("delegation request exceeds 64 KiB limit")
	}
	if !utf8.Valid(body) {
		return Request{}, usageError("delegation request must be valid UTF-8")
	}
	check := json.NewDecoder(bytes.NewReader(body))
	check.UseNumber()
	if err := checkValue(check, 0, "", ""); err != nil {
		return Request{}, err
	}
	if _, err := check.Token(); err != io.EOF {
		return Request{}, usageError("delegation request must contain exactly one JSON document")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return Request{}, usageError("delegation request is not valid JSON")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return Request{}, usageError("delegation request must contain exactly one JSON document")
	}
	if err := validateRequest(req); err != nil {
		return Request{}, err
	}
	return req, nil
}

func checkValue(d *json.Decoder, depth int, parent, field string) error {
	if depth > maxJSONDepth {
		return usageError("delegation request JSON is too deeply nested")
	}
	token, err := d.Token()
	if err != nil {
		return usageError("delegation request is not valid JSON")
	}
	if token == nil {
		return usageError("delegation request must not contain null values")
	}
	switch value := token.(type) {
	case json.Delim:
		switch value {
		case '{':
			return checkObject(d, depth, field)
		case '[':
			return usageError("delegation request must not contain arrays")
		default:
			return usageError("delegation request is not valid JSON")
		}
	case string:
		if err := checkString(field, value); err != nil {
			return err
		}
		if parent == "" && field == "schema_version" {
			return usageError("schema_version must be 1")
		}
		if (parent == "selector" && field == "settings") || (parent == "" && field == "selector") {
			return usageError("delegation request field has an invalid type")
		}
		return nil
	case json.Number:
		if parent != "" || field != "schema_version" {
			return usageError("delegation request field has an invalid type")
		}
		return nil
	case bool:
		return usageError("delegation request field has an invalid type")
	default:
		return usageError("delegation request field has an invalid type")
	}
}

func checkObject(d *json.Decoder, depth int, field string) error {
	if _, ok := allowedKeys[field]; !ok && field != "" {
		return usageError("delegation request field has an invalid type")
	}
	if field == "" && depth != 0 {
		return usageError("delegation request field has an invalid type")
	}
	seen := map[string]struct{}{}
	for d.More() {
		keyToken, err := d.Token()
		if err != nil {
			return usageError("delegation request is not valid JSON")
		}
		name, ok := keyToken.(string)
		if !ok {
			return usageError("delegation request object key must be a string")
		}
		if utf8.RuneCountInString(name) > MaxKeyChars {
			return usageError("delegation request object key exceeds 256 characters")
		}
		if containsControl(name) {
			return usageError("delegation request must not contain control characters")
		}
		if name != strings.ToLower(name) {
			return usageError("delegation request keys must use their exact documented spelling")
		}
		allowed := allowedKeys[field]
		if _, ok := allowed[name]; !ok {
			return usageError("delegation request contains an unknown field")
		}
		if _, dup := seen[name]; dup {
			return usageError("delegation request contains a duplicate object key")
		}
		seen[name] = struct{}{}
		if err := checkValue(d, depth+1, field, name); err != nil {
			return err
		}
	}
	if _, err := d.Token(); err != nil {
		return usageError("delegation request is not valid JSON")
	}
	return nil
}

func checkString(field, value string) error {
	if value == "" {
		return usageError("delegation request string fields must not be empty")
	}
	if utf8.RuneCountInString(value) > MaxKeyChars {
		return usageError("delegation request string exceeds 256 characters")
	}
	if containsControl(value) {
		return usageError("delegation request must not contain control characters")
	}
	if field != "" && strings.TrimSpace(value) == "" {
		return usageError("delegation request string fields must not be empty")
	}
	return nil
}

func validateRequest(req Request) error {
	if req.SchemaVersion != 1 {
		return usageError("schema_version must be 1")
	}
	if strings.TrimSpace(req.RequestKey) == "" {
		return usageError("request_key is required")
	}
	if !selectorConstraintPresent(req.Selector) {
		return usageError("selector requires family or model")
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
