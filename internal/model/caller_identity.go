package model

import (
	"errors"
	"regexp"
)

// CallerIdentity correlates launch-time callers. It is evidence, not an
// authenticated principal or a native resume handle. Raw session IDs are absent.
type CallerIdentity struct {
	Provider        string `json:"provider"`
	NativeSessionID string `json:"native_session_id"`
	Provenance      string `json:"provenance"`
}

var callerProviderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func (c CallerIdentity) Validate() error {
	if !callerProviderPattern.MatchString(c.Provider) || !hashPattern.MatchString(c.NativeSessionID) {
		return errors.New("invalid caller identity")
	}
	switch c.Provenance {
	case "explicit", "native_environment", "native_stream":
	default:
		return errors.New("invalid caller identity provenance")
	}
	return nil
}
