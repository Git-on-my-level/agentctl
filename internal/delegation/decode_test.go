package delegation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"
)

const validRequest = `{"schema_version":1,"request_key":"design-review-01","selector":{"harness":"cursor","family":"grok","version":"4.6","settings":{"speed":"regular"}}}`

func TestDecodeRequestAcceptsDocumentedExample(t *testing.T) {
	req, err := DecodeRequest([]byte(validRequest))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.SchemaVersion != 1 || req.RequestKey != "design-review-01" {
		t.Fatalf("request=%#v", req)
	}
	if req.Selector.Harness != "cursor" || req.Selector.Family != "grok" || req.Selector.Version != "4.6" || req.Selector.Settings == nil || req.Selector.Settings.Speed != "regular" {
		t.Fatalf("selector=%#v", req.Selector)
	}
}

func TestDecodeRequestAcceptsModelWithoutFamily(t *testing.T) {
	body := `{"schema_version":1,"request_key":"k","selector":{"model":"cursor-grok-4.6-high","host":"studio"}}`
	req, err := DecodeRequest([]byte(body))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if req.Selector.Model != "cursor-grok-4.6-high" || req.Selector.Host != "studio" || req.Selector.Family != "" {
		t.Fatalf("selector=%#v", req.Selector)
	}
}

func TestDecodeRequestAcceptsTrailingWhitespace(t *testing.T) {
	if _, err := DecodeRequest([]byte(validRequest + " \n\t")); err != nil {
		t.Fatalf("trailing whitespace: %v", err)
	}
}

func TestDecodeRequestRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	longKey := strings.Repeat("a", MaxKeyChars+1)
	longValue := strings.Repeat("b", MaxKeyChars+1)
	cases := map[string]string{
		"trailing_document":     validRequest + `{}`,
		"trailing_garbage":      validRequest + `garbage`,
		"unknown_field":         strings.Replace(validRequest, `"schema_version":1`, `"schema_version":1,"typo":true`, 1),
		"duplicate_key":         strings.Replace(validRequest, `"schema_version":1`, `"schema_version":2,"schema_version":1`, 1),
		"duplicate_case_alias":  strings.Replace(validRequest, `"schema_version":1`, `"Schema_Version":2,"schema_version":1`, 1),
		"nested_duplicate":      strings.Replace(validRequest, `"speed":"regular"`, `"speed":"fast","speed":"regular"`, 1),
		"case_alias":            strings.Replace(validRequest, `"schema_version"`, `"SCHEMA_VERSION"`, 1),
		"selector_case_alias":   strings.Replace(validRequest, `"family"`, `"Family"`, 1),
		"empty_optional_string": strings.Replace(validRequest, `"version":"4.6"`, `"version":""`, 1),
		"whitespace_optional":   strings.Replace(validRequest, `"version":"4.6"`, `"version":"   "`, 1),
		"null_request":          `null`,
		"null_selector":         strings.Replace(validRequest, `{"harness":"cursor"`, `null`, 1),
		"null_settings":         strings.Replace(validRequest, `{"speed":"regular"}`, `null`, 1),
		"null_speed":            strings.Replace(validRequest, `"regular"`, `null`, 1),
		"invalid_utf8":          validRequest[:len(validRequest)-1] + string([]byte{0xff}) + `}`,
		"oversized":             strings.Repeat(" ", MaxRequestBytes+1),
		"excessive_depth":       strings.Repeat("[", 20) + `1` + strings.Repeat("]", 20),
		"non_object":            `[]`,
		"malformed":             `{"schema_version":1`,
		"array_selector":        `{"schema_version":1,"request_key":"k","selector":[]}`,
		"numeric_family":        `{"schema_version":1,"request_key":"k","selector":{"family":4.6}}`,
		"string_schema":         `{"schema_version":"1","request_key":"k","selector":{"family":"grok"}}`,
		"missing_constraint":    `{"schema_version":1,"request_key":"k","selector":{"harness":"cursor"}}`,
		"missing_selector":      `{"schema_version":1,"request_key":"k"}`,
		"control_in_key":        strings.Replace(validRequest, `"request_key"`, `"request\u0001key"`, 1),
		"control_in_value":      strings.Replace(validRequest, `"design-review-01"`, `"design\u0000review"`, 1),
		"long_object_key":       `{"schema_version":1,"request_key":"k","` + longKey + `":true}`,
		"long_string":           `{"schema_version":1,"request_key":"` + longValue + `","selector":{"family":"grok"}}`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := DecodeRequest([]byte(body))
			if err == nil {
				t.Fatal("accepted invalid or ambiguous JSON")
			}
			var typed *Error
			if !errors.As(err, &typed) || typed.Kind != KindUsage || typed.Diagnostic == "" {
				t.Fatalf("error=%v", err)
			}
			if strings.Contains(typed.Error(), "{") || strings.Contains(typed.Error(), validRequest) {
				t.Fatalf("error leaked JSON: %q", typed.Error())
			}
		})
	}
}

func TestDecodeRequestErrorOmitsRawJSON(t *testing.T) {
	secret := "secret-value-xyz"
	body := []byte(`{"schema_version":1,"request_key":"` + secret + `","selector":{"family":"grok"},"unknown":true}`)
	_, err := DecodeRequest(body)
	if err == nil {
		t.Fatal("accepted unknown field")
	}
	text := err.Error()
	if strings.Contains(text, secret) || strings.Contains(text, string(body)) {
		t.Fatalf("error leaked request contents: %q", text)
	}
}

func TestDecodeRequestAcceptsMaxKeyChars(t *testing.T) {
	key := strings.Repeat("k", MaxKeyChars)
	body := `{"schema_version":1,"request_key":"` + key + `","selector":{"family":"grok"}}`
	if utf8.RuneCountInString(key) != MaxKeyChars {
		t.Fatal("fixture")
	}
	req, err := DecodeRequest([]byte(body))
	if err != nil || req.RequestKey != key {
		t.Fatalf("max key: %#v %v", req, err)
	}
}

func TestDecodeRequestPreciseSafeDiagnostics(t *testing.T) {
	cases := []struct{ body, path, violation, keys string }{
		{`{"schema_version":1,"request_key":"private-key","selector":{"family":"private-family","provider":"private-value"}}`, "$.selector.provider", "unknown_field", "family,harness,host,model,settings,version"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"model":"private-model","settings":{"sped":"private-speed"}}}`, "$.selector.settings.sped", "unknown_field", "access,effort,speed"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"Family":"private-family"}}`, "$.selector.Family", "key_spelling", "family,harness,host,model,settings,version"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"family":"private-family","family":"other-private-family"}}`, "$.selector.family", "duplicate_key", "family,harness,host,model,settings,version"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"model":null}}`, "$.selector.model", "null_value", "family,harness,host,model,settings,version"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"settings":{"speed":17}}}`, "$.selector.settings.speed", "invalid_type", "access,effort,speed"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"family":"private-family","settings":{"access":"private-access"}}}`, "$.selector.settings.access", "invalid_access", "access,effort,speed"},
		{`{"schema_version":1,"selector":{"family":"private-family"}}`, "$.request_key", "required_field", "request_key,schema_version,selector"},
		{`{"schema_version":1,"request_key":"private-key","selector":{}}`, "$.selector", "required_constraint", "family,harness,host,model,settings,version"},
		{`{"schema_version":1,"request_key":"private-key","selector":{"family":{"private-key":true}}}`, "$.selector.family", "invalid_type", "family,harness,host,model,settings,version"},
	}
	for _, tc := range cases {
		t.Run(tc.path+tc.violation, func(t *testing.T) {
			_, err := DecodeRequest([]byte(tc.body))
			var issue *Error
			if !errors.As(err, &issue) {
				t.Fatalf("error=%v", err)
			}
			if issue.FieldPath != tc.path || issue.Violation != tc.violation || strings.Join(issue.AllowedKeys, ",") != tc.keys {
				t.Fatalf("diagnostic=%#v", issue)
			}
			if !strings.Contains(issue.Error(), tc.path) {
				t.Fatalf("missing human field path: %v", issue)
			}
			body, err := json.Marshal(issue)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Contains(body, []byte("private-")) {
				t.Fatalf("leaked values: %s", body)
			}
			if issue.ExampleRequest == nil {
				t.Fatal("no schema example")
			}
			example, _ := json.Marshal(issue.ExampleRequest)
			if _, err := DecodeRequest(example); err != nil {
				t.Fatalf("example violates schema: %s: %v", example, err)
			}
		})
	}
}

func TestDecodeRequestRedactsUnsafeUnknownKeys(t *testing.T) {
	for _, key := range []string{"private token", "secret.value", "credential/abc", "秘密", strings.Repeat("k", 65), strings.Repeat("k", 257), "token\nsecret", ""} {
		t.Run(fmt.Sprintf("bytes-%d", len(key)), func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"schema_version": 1, "request_key": "k", "selector": map[string]any{"family": "f", key: "secret-value"}})
			_, err := DecodeRequest(body)
			var issue *Error
			if !errors.As(err, &issue) {
				t.Fatalf("error=%v", err)
			}
			if issue.FieldPath != "$.selector.<unknown>" {
				t.Fatalf("unbounded key echoed: %#v", issue)
			}
			encoded, _ := json.Marshal(issue)
			if key != "" && bytes.Contains(encoded, []byte(key)) {
				t.Fatalf("unsafe key echoed: %s", encoded)
			}
			if bytes.Contains(encoded, []byte("secret-value")) {
				t.Fatalf("value echoed: %s", encoded)
			}
		})
	}
}

func TestDecodeRequestRejectsWrongSchemaVersionTypes(t *testing.T) {
	for _, value := range []string{`"1"`, `1.0`, `1e0`, `true`, `{}`, `2`, `null`} {
		_, err := DecodeRequest([]byte(`{"schema_version":` + value + `,"request_key":"k","selector":{"model":"m"}}`))
		var issue *Error
		if !errors.As(err, &issue) || issue.FieldPath != "$.schema_version" {
			t.Fatalf("value=%s issue=%#v err=%v", value, issue, err)
		}
	}
}
