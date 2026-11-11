package store

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
)

// validateChildExpiryJSON refuses ambiguous objects before their values can
// establish expiry. encoding/json deliberately accepts duplicate members;
// last-value-wins is not evidence that a child was legitimately purged.
//
// Keep this policy on expiry evidence, not on unrelated journal records.
// Nonconflicting extension fields remain compatible. The iterative walk avoids
// adding a recursion limit or stack growth to the existing record-byte bounds.
func validateChildExpiryJSON(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return corrupt(errors.New("children expiry must be a JSON object"))
	}
	type objectFrame struct {
		object bool
		key    bool
		seen   map[string]struct{}
	}
	stack := []objectFrame{{object: true, key: true, seen: make(map[string]struct{})}}
	for len(stack) != 0 {
		token, err := decoder.Token()
		if err != nil {
			return corrupt(err)
		}
		frame := &stack[len(stack)-1]
		if delimiter, ok := token.(json.Delim); ok {
			switch delimiter {
			case '{', '[':
				if frame.object {
					if frame.key {
						return corrupt(errors.New("invalid children expiry member"))
					}
					frame.key = true
				}
				next := objectFrame{object: delimiter == '{'}
				if next.object {
					next.key, next.seen = true, make(map[string]struct{})
				}
				stack = append(stack, next)
			case '}', ']':
				if (delimiter == '}') != frame.object || (frame.object && !frame.key) {
					return corrupt(errors.New("invalid children expiry object"))
				}
				stack = stack[:len(stack)-1]
			}
			continue
		}
		if frame.object && frame.key {
			name, ok := token.(string)
			if !ok {
				return corrupt(errors.New("invalid children expiry member name"))
			}
			if len(stack) == 1 {
				// Match encoding/json's case-folded lookup for the fields that
				// authorize expiry, including Unicode aliases and escaped names.
				for _, field := range []string{"scope", "key", "input_digest", "object_type", "object_id"} {
					if strings.EqualFold(name, field) {
						name = field
						break
					}
				}
			}
			if _, duplicate := frame.seen[name]; duplicate {
				return corrupt(errors.New("duplicate children expiry member"))
			}
			frame.seen[name], frame.key = struct{}{}, false
		} else if frame.object {
			frame.key = true
		}
	}
	if _, err := decoder.Token(); err != io.EOF {
		return corrupt(errors.New("trailing children expiry JSON"))
	}
	return nil
}
