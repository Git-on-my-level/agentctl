package output

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

type Mode string

const (
	Text Mode = "text"
	JSON Mode = "json"
)

func ParseMode(value string) (Mode, error) {
	mode := Mode(value)
	if mode != Text && mode != JSON {
		return "", fmt.Errorf("output must be text or json")
	}
	return mode, nil
}

type SideEffectClass string

const (
	ReadOnly                SideEffectClass = "read_only"
	LocalOperationalWrite   SideEffectClass = "local_operational_write"
	RemoteCoordinationWrite SideEffectClass = "remote_coordination_write"
	ExternalSideEffect      SideEffectClass = "external_side_effect"
)

type NextAction struct {
	Label           string          `json:"label"`
	Argv            []string        `json:"argv"`
	Mutates         bool            `json:"mutates"`
	SideEffectClass SideEffectClass `json:"side_effect_class"`
	Preconditions   []string        `json:"preconditions"`
}
type Warning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
type SuccessDocument struct {
	OK            bool         `json:"ok"`
	SchemaVersion int          `json:"schema_version"`
	Result        any          `json:"result"`
	Warnings      []Warning    `json:"warnings"`
	NextActions   []NextAction `json:"next_actions"`
}

type Field struct {
	Name  string
	Value any
}
type Line struct {
	Lead   string
	Fields []Field
}
type Success struct {
	Result      any
	Warnings    []Warning
	NextActions []NextAction
	Lines       []Line
}
type Renderer struct {
	Mode   Mode
	Writer io.Writer
}

func (r Renderer) Success(value Success) error {
	if r.Writer == nil {
		return errors.New("output writer is nil")
	}
	if r.Mode == JSON {
		return writeJSON(r.Writer, SuccessDocument{OK: true, SchemaVersion: 1, Result: value.Result, Warnings: nonNilWarnings(value.Warnings), NextActions: nonNilActions(value.NextActions)})
	}
	for _, line := range value.Lines {
		if _, err := fmt.Fprintln(r.Writer, RenderLine(line)); err != nil {
			return err
		}
	}
	for _, action := range value.NextActions {
		if _, err := fmt.Fprintln(r.Writer, "next "+renderArgv(action.Argv)); err != nil {
			return err
		}
	}
	return nil
}

func (r Renderer) Failure(err *Error) error {
	if r.Writer == nil {
		return errors.New("output writer is nil")
	}
	if err == nil {
		err = NewError(CodeInternal, "internal error", false)
	}
	if r.Mode == JSON {
		return writeJSON(r.Writer, ErrorDocument{OK: false, SchemaVersion: 1, Error: err})
	}
	fields := []Field{{"code", err.Code}, {"exit", err.ExitCode}, {"retryable", err.Retryable}, {"message", err.Message}}
	keys := make([]string, 0, len(err.Details))
	for key := range err.Details {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		fields = append(fields, Field{key, err.Details[key]})
	}
	if _, writeErr := fmt.Fprintln(r.Writer, RenderLine(Line{Lead: "error", Fields: fields})); writeErr != nil {
		return writeErr
	}
	for _, action := range err.NextActions {
		if _, writeErr := fmt.Fprintln(r.Writer, "next "+renderArgv(action.Argv)); writeErr != nil {
			return writeErr
		}
	}
	return nil
}

var bareValue = regexp.MustCompile(`^[a-zA-Z0-9_./:@+-]+$`)

func RenderLine(line Line) string {
	var b strings.Builder
	b.WriteString(line.Lead)
	for _, field := range line.Fields {
		if field.Value == nil {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(field.Name)
		b.WriteByte('=')
		b.WriteString(renderValue(field.Value))
	}
	return b.String()
}
func renderValue(value any) string {
	switch v := value.(type) {
	case string:
		if bareValue.MatchString(v) {
			return v
		}
		return marshalCompact(v)
	case fmt.Stringer:
		return renderValue(v.String())
	case bool:
		return strconv.FormatBool(v)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	case uint64:
		return strconv.FormatUint(v, 10)
	default:
		reflected := reflect.ValueOf(value)
		if reflected.IsValid() && reflected.Kind() == reflect.String {
			return renderValue(reflected.String())
		}
		return marshalCompact(v)
	}
}

func marshalCompact(value any) string {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return `"<unrenderable>"`
	}
	return strings.TrimSuffix(buffer.String(), "\n")
}
func renderArgv(argv []string) string {
	parts := make([]string, len(argv))
	for i, arg := range argv {
		parts[i] = renderValue(arg)
	}
	return strings.Join(parts, " ")
}
func writeJSON(w io.Writer, value any) error {
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	return enc.Encode(value)
}
func nonNilWarnings(v []Warning) []Warning {
	if v == nil {
		return []Warning{}
	}
	return v
}
func nonNilActions(v []NextAction) []NextAction {
	if v == nil {
		return []NextAction{}
	}
	return v
}
