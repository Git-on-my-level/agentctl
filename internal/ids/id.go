package ids

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

const (
	domain          = "agentctl-word-id-v1\x00"
	payloadMask     = uint64(1<<60) - 1
	EncodingVersion = 1
)

var ErrInvalid = errors.New("invalid agentctl word ID")

type ParseError struct{ Value, Reason string }

func (e *ParseError) Error() string { return fmt.Sprintf("%v: %s", ErrInvalid, e.Reason) }
func (e *ParseError) Unwrap() error { return ErrInvalid }

// ID is a validated version-1 typed six-word ID.
type ID struct {
	typ     Type
	payload uint64
	text    string
}

func New(typ Type) (ID, error) { return NewWithReader(typ, rand.Reader) }

func NewWithReader(typ Type, reader io.Reader) (ID, error) {
	var raw [8]byte
	if _, err := io.ReadFull(reader, raw[:]); err != nil {
		return ID{}, fmt.Errorf("generate ID randomness: %w", err)
	}
	payload := binary.BigEndian.Uint64(raw[:]) & payloadMask
	return FromPayload(typ, payload)
}

func FromPayload(typ Type, payload uint64) (ID, error) {
	if !typ.Valid() {
		return ID{}, &ParseError{Reason: "unregistered type prefix"}
	}
	if payload > payloadMask {
		return ID{}, &ParseError{Reason: "payload exceeds 60 bits"}
	}
	indices := [6]uint16{
		uint16((payload >> 49) & 0x7ff), uint16((payload >> 38) & 0x7ff),
		uint16((payload >> 27) & 0x7ff), uint16((payload >> 16) & 0x7ff),
		uint16((payload >> 5) & 0x7ff), uint16((payload&0x1f)<<6) | uint16(checksum(typ, payload)),
	}
	words := make([]string, len(indices))
	for i, index := range indices {
		words[i] = wordList[index]
	}
	text := string(typ) + "-" + strings.Join(words, "-")
	return ID{typ: typ, payload: payload, text: text}, nil
}

func Parse(value string) (ID, error) {
	if value == "" || strings.ToLower(value) != value {
		return ID{}, &ParseError{Value: value, Reason: "ID must be canonical lowercase"}
	}
	parts := strings.Split(value, "-")
	if len(parts) != 7 {
		return ID{}, &ParseError{Value: value, Reason: "ID must contain a type and six words"}
	}
	typ := Type(parts[0])
	if !typ.Valid() {
		return ID{}, &ParseError{Value: value, Reason: "unregistered type prefix"}
	}
	var index [6]uint16
	for i, word := range parts[1:] {
		indexValue, ok := wordIndex[word]
		if !ok {
			return ID{}, &ParseError{Value: value, Reason: fmt.Sprintf("word %q is not in word list v1", word)}
		}
		index[i] = indexValue
	}
	payload := uint64(index[0])<<49 | uint64(index[1])<<38 | uint64(index[2])<<27 |
		uint64(index[3])<<16 | uint64(index[4])<<5 | uint64(index[5]>>6)
	want := byte(index[5] & 0x3f)
	if got := checksum(typ, payload); got != want {
		return ID{}, &ParseError{Value: value, Reason: "type-bound checksum mismatch"}
	}
	canonical, _ := FromPayload(typ, payload)
	if canonical.text != value {
		return ID{}, &ParseError{Value: value, Reason: "noncanonical encoding"}
	}
	return canonical, nil
}

func ParseAs(typ Type, value string) (ID, error) {
	id, err := Parse(value)
	if err != nil {
		return ID{}, err
	}
	if id.typ != typ {
		return ID{}, &ParseError{Value: value, Reason: fmt.Sprintf("expected type %q, got %q", typ, id.typ)}
	}
	return id, nil
}

func checksum(typ Type, payload uint64) byte {
	h := sha256.New()
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte(typ))
	_, _ = h.Write([]byte{0})
	var encoded [8]byte
	binary.BigEndian.PutUint64(encoded[:], payload)
	_, _ = h.Write(encoded[:])
	return h.Sum(nil)[0] >> 2
}

func (id ID) Type() Type      { return id.typ }
func (id ID) Payload() uint64 { return id.payload }
func (id ID) String() string  { return id.text }
func (id ID) IsZero() bool    { return id.text == "" }
func (id ID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return nil, ErrInvalid
	}
	return []byte(id.text), nil
}
func (id *ID) UnmarshalText(data []byte) error {
	parsed, err := Parse(string(data))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
func (id ID) MarshalJSON() ([]byte, error) {
	if id.IsZero() {
		return nil, ErrInvalid
	}
	return json.Marshal(id.text)
}
func (id *ID) UnmarshalJSON(data []byte) error {
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return err
	}
	return id.UnmarshalText([]byte(value))
}

type Generator interface{ New(Type) (ID, error) }
type CryptoGenerator struct{}

func (CryptoGenerator) New(typ Type) (ID, error) { return New(typ) }
