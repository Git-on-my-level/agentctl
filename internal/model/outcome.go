package model

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

const (
	OutcomeInlineLimit        = 1 << 20
	OutcomePreviewLimit       = 512
	OutcomeFailureLimit       = 4096
	NativeFailureMessageLimit = 200
)

// nativeFailureCodePattern accepts a provider diagnostic code. Free text stays
// out of native_code so a message cannot be copied into the code field.
var nativeFailureCodePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_]{0,63}$`)

// ValidNativeFailureCode reports whether value is a bounded provider code.
func ValidNativeFailureCode(value string) bool {
	return nativeFailureCodePattern.MatchString(value)
}

type OutcomeAvailability string

const (
	OutcomeStored              OutcomeAvailability = "stored"
	OutcomeOmittedByPolicy     OutcomeAvailability = "omitted_by_policy"
	OutcomeUnavailableAtSource OutcomeAvailability = "unavailable_at_source"
	OutcomeLegacyNotRecorded   OutcomeAvailability = "legacy_not_recorded"
)

type OutcomeContent struct {
	MediaType string `json:"media_type"`
	Source    string `json:"source,omitempty"`
	Text      string `json:"text"`
	Preview   string `json:"preview"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256,omitempty"`
	Truncated bool   `json:"truncated"`
}

type OutcomeFailure struct {
	Code       string `json:"code"`
	Kind       string `json:"kind"`
	Source     string `json:"source"`
	Retryable  bool   `json:"retryable"`
	Message    string `json:"message"`
	Category   string `json:"category,omitempty"`
	NativeCode string `json:"native_code,omitempty"`
}

type Outcome struct {
	SchemaVersion  int                 `json:"schema_version"`
	ExecutionID    ids.ExecutionID     `json:"execution_id"`
	Revision       uint64              `json:"revision"`
	State          State               `json:"state"`
	Availability   OutcomeAvailability `json:"availability"`
	RecordedAt     time.Time           `json:"recorded_at"`
	Source         string              `json:"source"`
	ResultRef      string              `json:"result_ref"`
	Content        *OutcomeContent     `json:"content,omitempty"`
	Failure        *OutcomeFailure     `json:"failure,omitempty"`
	NativeExitCode *int                `json:"native_exit_code,omitempty"`
}

func (o Outcome) Validate() error {
	if o.SchemaVersion != SchemaVersion || o.ExecutionID.IsZero() || o.Revision < 1 {
		return errors.New("invalid outcome identity or schema")
	}
	if !o.State.Terminal() || o.RecordedAt.IsZero() {
		return errors.New("outcome requires terminal state and recorded_at")
	}
	if strings.TrimSpace(o.Source) == "" || strings.TrimSpace(o.ResultRef) == "" {
		return errors.New("outcome source and result_ref are required")
	}
	switch o.Availability {
	case OutcomeStored:
		if o.Content == nil && o.Failure == nil {
			return errors.New("stored outcome requires content or failure")
		}
	case OutcomeOmittedByPolicy, OutcomeUnavailableAtSource, OutcomeLegacyNotRecorded:
		if o.Content != nil {
			return errors.New("unavailable outcome cannot contain content")
		}
	default:
		return fmt.Errorf("invalid outcome availability %q", o.Availability)
	}
	if o.Content != nil {
		if o.Content.MediaType != "text/plain" || !utf8.ValidString(o.Content.Text) || !utf8.ValidString(o.Content.Preview) {
			return errors.New("outcome content must be UTF-8 text/plain")
		}
		if len(o.Content.Source) > 128 {
			return errors.New("outcome content source is too long")
		}
		if len(o.Content.Text) > OutcomeInlineLimit || len(o.Content.Preview) > OutcomePreviewLimit || o.Content.Bytes < len(o.Content.Text) {
			return errors.New("outcome content bounds are invalid")
		}
		if !o.Content.Truncated && o.Content.Bytes != len(o.Content.Text) {
			return errors.New("complete outcome byte count disagrees with text")
		}
		if o.Content.SHA256 != "" && !hashPattern.MatchString(o.Content.SHA256) {
			return errors.New("invalid outcome content digest")
		}
		if o.Content.SHA256 != "" {
			sum := sha256.Sum256([]byte(o.Content.Text))
			if o.Content.SHA256 != "sha256:"+hex.EncodeToString(sum[:]) {
				return errors.New("outcome content digest disagrees with text")
			}
		}
		if o.Content.Truncated && o.Content.SHA256 != "" {
			return errors.New("truncated outcome cannot claim a complete digest")
		}
	}
	if o.Failure != nil {
		limit := OutcomeFailureLimit
		if o.Failure.Category != "" || o.Failure.NativeCode != "" {
			limit = NativeFailureMessageLimit
		}
		if strings.TrimSpace(o.Failure.Code) == "" || strings.TrimSpace(o.Failure.Kind) == "" || strings.TrimSpace(o.Failure.Source) == "" || len(o.Failure.Message) > limit {
			return errors.New("invalid outcome failure")
		}
		if o.Failure.Category != "" && !nativeFailureCodePattern.MatchString(o.Failure.Category) {
			return errors.New("invalid outcome failure category")
		}
		if o.Failure.NativeCode != "" && !nativeFailureCodePattern.MatchString(o.Failure.NativeCode) {
			return errors.New("invalid outcome failure native code")
		}
	}
	return nil
}
