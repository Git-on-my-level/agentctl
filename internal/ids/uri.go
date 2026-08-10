package ids

import (
	"fmt"
	"net/url"
	"strings"
)

type PortableURI struct {
	Scheme   string
	Host     HostID
	Segments []ID
}

var uriTypes = map[string][]Type{
	"agentctl": {TypeExecution},
	"codex":    {TypeSource}, "cursor": {TypeSource}, "claude": {TypeSource}, "omp": {TypeSource},
	"multica": {TypeProject, TypeIssue, TypeRun},
}

func ParseURI(value string) (PortableURI, error) {
	parsed, err := url.Parse(value)
	if err != nil {
		return PortableURI{}, &ParseError{Value: value, Reason: "invalid portable URI"}
	}
	expected, ok := uriTypes[parsed.Scheme]
	if !ok {
		return PortableURI{}, &ParseError{Value: value, Reason: "unsupported URI scheme"}
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || parsed.Port() != "" {
		return PortableURI{}, &ParseError{Value: value, Reason: "credentials, ports, query, and fragment are forbidden"}
	}
	host, err := ParseHostID(parsed.Hostname())
	if err != nil {
		return PortableURI{}, fmt.Errorf("portable URI host: %w", err)
	}
	parts := strings.Split(strings.Trim(parsed.EscapedPath(), "/"), "/")
	if len(parts) != len(expected) {
		return PortableURI{}, &ParseError{Value: value, Reason: fmt.Sprintf("scheme requires %d typed path segment(s)", len(expected))}
	}
	segments := make([]ID, len(parts))
	for i, part := range parts {
		decoded, err := url.PathUnescape(part)
		if err != nil || decoded != part {
			return PortableURI{}, &ParseError{Value: value, Reason: "path segments must be unescaped canonical IDs"}
		}
		id, err := ParseAs(expected[i], decoded)
		if err != nil {
			return PortableURI{}, fmt.Errorf("portable URI segment %d: %w", i+1, err)
		}
		segments[i] = id
	}
	result := PortableURI{Scheme: parsed.Scheme, Host: host, Segments: segments}
	if result.String() != value {
		return PortableURI{}, &ParseError{Value: value, Reason: "noncanonical portable URI"}
	}
	return result, nil
}

func (u PortableURI) String() string {
	parts := make([]string, len(u.Segments))
	for i, id := range u.Segments {
		parts[i] = id.String()
	}
	return u.Scheme + "://" + u.Host.String() + "/" + strings.Join(parts, "/")
}
