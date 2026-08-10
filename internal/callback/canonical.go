// Package callback contains storage-neutral callback delivery primitives.
package callback

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"reflect"
	"sort"
	"strconv"
)

// ErrUnsupportedJSON is returned when a value cannot be represented by the
// callback canonical JSON profile.
var ErrUnsupportedJSON = errors.New("unsupported JSON value")

// CanonicalJSON returns deterministic JSON suitable for hashing. Its profile
// is RFC-8785-compatible for the JSON values emitted by agentctl:
//
//   - object names are sorted by their UTF-8 byte sequence;
//   - no insignificant whitespace is emitted and arrays retain order;
//   - strings use encoding/json's UTF-8 escaping;
//   - integers are base-10 without leading zeroes, and finite floats use
//     strconv's shortest round-trip representation (negative zero is 0).
//
// Go's json package does not expose ECMAScript's exact exponent thresholds, so
// this number rule is intentionally a documented deterministic profile rather
// than a claim of byte identity with every RFC 8785 implementation. Adapters
// must use the same profile when deriving semantic event keys.
func CanonicalJSON(v any) ([]byte, error) {
	var b bytes.Buffer
	if err := writeCanonical(&b, reflect.ValueOf(v)); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func writeCanonical(b *bytes.Buffer, v reflect.Value) error {
	if !v.IsValid() {
		b.WriteString("null")
		return nil
	}
	if v.Kind() == reflect.Interface || v.Kind() == reflect.Pointer {
		if v.IsNil() {
			b.WriteString("null")
			return nil
		}
		return writeCanonical(b, v.Elem())
	}
	if v.CanInterface() {
		switch x := v.Interface().(type) {
		case json.RawMessage:
			var decoded any
			dec := json.NewDecoder(bytes.NewReader(x))
			dec.UseNumber()
			if err := dec.Decode(&decoded); err != nil {
				return fmt.Errorf("raw JSON: %w", err)
			}
			var extra any
			if err := dec.Decode(&extra); err == nil {
				return fmt.Errorf("raw JSON has trailing value")
			} else if !errors.Is(err, io.EOF) {
				return fmt.Errorf("raw JSON trailing data: %w", err)
			}
			return writeCanonical(b, reflect.ValueOf(decoded))
		case json.Number:
			return writeNumber(b, string(x))
		case float32:
			return writeFloat(b, float64(x), 32)
		case float64:
			return writeFloat(b, x, 64)
		case string:
			enc, _ := json.Marshal(x)
			b.Write(enc)
			return nil
		case bool:
			if x {
				b.WriteString("true")
			} else {
				b.WriteString("false")
			}
			return nil
		}
	}
	switch v.Kind() {
	case reflect.String:
		enc, _ := json.Marshal(v.String())
		b.Write(enc)
		return nil
	case reflect.Bool:
		if v.Bool() {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		b.WriteString(strconv.FormatInt(v.Int(), 10))
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Uintptr:
		b.WriteString(strconv.FormatUint(v.Uint(), 10))
		return nil
	case reflect.Float32, reflect.Float64:
		return writeFloat(b, v.Float(), int(v.Type().Bits()))
	case reflect.Slice, reflect.Array:
		b.WriteByte('[')
		for i := 0; i < v.Len(); i++ {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, v.Index(i)); err != nil {
				return err
			}
		}
		b.WriteByte(']')
		return nil
	case reflect.Map:
		if v.Type().Key().Kind() != reflect.String {
			return fmt.Errorf("%w: map key %s", ErrUnsupportedJSON, v.Type().Key())
		}
		keys := v.MapKeys()
		sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			kb, _ := json.Marshal(k.String())
			b.Write(kb)
			b.WriteByte(':')
			if err := writeCanonical(b, v.MapIndex(k)); err != nil {
				return err
			}
		}
		b.WriteByte('}')
		return nil
	case reflect.Struct:
		// Marshal through encoding/json so tags and embedded fields have their
		// normal meaning, then canonicalize the resulting generic value.
		raw, err := json.Marshal(v.Interface())
		if err != nil {
			return fmt.Errorf("%w: %v", ErrUnsupportedJSON, err)
		}
		var decoded any
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.UseNumber()
		if err := dec.Decode(&decoded); err != nil {
			return err
		}
		return writeCanonical(b, reflect.ValueOf(decoded))
	default:
		return fmt.Errorf("%w: %s", ErrUnsupportedJSON, v.Type())
	}
}

func writeNumber(b *bytes.Buffer, s string) error {
	if s == "" {
		return fmt.Errorf("%w: empty number", ErrUnsupportedJSON)
	}
	if !json.Valid([]byte(s)) {
		return fmt.Errorf("%w: malformed number", ErrUnsupportedJSON)
	}
	// ParseFloat catches NaN/Inf and malformed values. Keep integers exactly as
	// provided; decimal values use shortest round-trip formatting.
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		b.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	if u, err := strconv.ParseUint(s, 10, 64); err == nil {
		b.WriteString(strconv.FormatUint(u, 10))
		return nil
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w: %s", ErrUnsupportedJSON, s)
	}
	return writeFloat(b, f, 64)
}

func writeFloat(b *bytes.Buffer, f float64, bits int) error {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return fmt.Errorf("%w: non-finite number", ErrUnsupportedJSON)
	}
	if f == 0 {
		b.WriteByte('0') // RFC 8785 normalizes negative zero.
		return nil
	}
	b.WriteString(strconv.FormatFloat(f, 'g', -1, bits))
	return nil
}

// SemanticDedupeKey computes the full semantic hash specified by the event
// contract. The returned projection is the exact canonical byte sequence that
// was hashed, useful for collision diagnostics and deterministic fixtures.
func SemanticDedupeKey(adapter string, version uint32, projection any) (key string, canonical []byte, err error) {
	if len(adapter) < 2 || len(adapter) > 32 || adapter[0] < 'a' || adapter[0] > 'z' {
		return "", nil, errors.New("adapter must be a lowercase name")
	}
	for i := 1; i < len(adapter); i++ {
		c := adapter[i]
		if !((c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') || c == '_' || c == '-') {
			return "", nil, errors.New("adapter must be a lowercase name")
		}
	}
	if version == 0 {
		return "", nil, errors.New("dedupe version must be positive")
	}
	canonical, err = CanonicalJSON(projection)
	if err != nil {
		return "", nil, err
	}
	h := sha256.New()
	h.Write([]byte("agentctl-event-v1\x00"))
	h.Write([]byte(adapter))
	h.Write([]byte{0})
	h.Write([]byte{byte(version >> 24), byte(version >> 16), byte(version >> 8), byte(version)})
	h.Write(canonical)
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), canonical, nil
}

// CanonicalizeJSON is a descriptive alias used by adapters and fixtures.
func CanonicalizeJSON(v any) ([]byte, error) { return CanonicalJSON(v) }

// DedupeKey is the key-only form of SemanticDedupeKey.
func DedupeKey(adapter string, version uint32, projection any) (string, error) {
	key, _, err := SemanticDedupeKey(adapter, version, projection)
	return key, err
}
