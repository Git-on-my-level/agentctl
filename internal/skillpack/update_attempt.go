package skillpack

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const updateAttemptSchemaVersion = 1

// maxAttemptErrorBytes bounds the stored message so a verbose Git failure
// cannot grow owner state without limit.
const maxAttemptErrorBytes = 512

// UpdateAttempt is the outcome of the most recent automatic or explicit Skill
// Hub update. The daily worker has no terminal, so this file is the only place
// its failure is visible; status and doctor read it back.
type UpdateAttempt struct {
	SchemaVersion int       `json:"schema_version"`
	AttemptedAt   time.Time `json:"attempted_at"`
	Trigger       string    `json:"trigger"`
	Succeeded     bool      `json:"succeeded"`
	Healthy       bool      `json:"healthy"`
	Conflicts     int       `json:"conflicts"`
	ErrorCode     string    `json:"error_code,omitempty"`
	Error         string    `json:"error,omitempty"`
	Drift         []string  `json:"drift,omitempty"`
	// FirstFailedAt and ConsecutiveFailures survive repeated failures so a
	// retry on every invocation does not hide how long the update has failed.
	FirstFailedAt       *time.Time `json:"first_failed_at,omitempty"`
	ConsecutiveFailures int        `json:"consecutive_failures,omitempty"`
}

// UpdateAttemptPath is beside skill-source.json, which it never modifies.
func UpdateAttemptPath(home string) (string, error) {
	_, statePath, err := DefaultHubPaths(home)
	if err != nil {
		return "", err
	}
	return filepath.Join(filepath.Dir(statePath), "skill-update-last.json"), nil
}

// RecordUpdateAttempt stores one outcome. A failure carries forward the first
// failure time from a previous failed record; a success clears it.
func RecordUpdateAttempt(home string, attempt UpdateAttempt) error {
	path, err := UpdateAttemptPath(home)
	if err != nil {
		return err
	}
	attempt.SchemaVersion = updateAttemptSchemaVersion
	attempt.AttemptedAt = attempt.AttemptedAt.UTC()
	attempt.Error = boundAttemptError(attempt.Error)
	attempt.FirstFailedAt, attempt.ConsecutiveFailures = nil, 0
	if !attempt.Succeeded {
		first, count := attempt.AttemptedAt, 1
		if previous, err := ReadUpdateAttempt(home); err == nil && previous != nil && !previous.Succeeded {
			if previous.FirstFailedAt != nil {
				first = previous.FirstFailedAt.UTC()
			}
			count = previous.ConsecutiveFailures + 1
		}
		attempt.FirstFailedAt, attempt.ConsecutiveFailures = &first, count
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := validateNoSymlinkComponents(filepath.Dir(path)); err != nil {
		return err
	}
	data, err := json.MarshalIndent(attempt, "", "  ")
	if err != nil {
		return err
	}
	return writePrivateFileAtomic(path, ".skill-update-last-", append(data, '\n'))
}

// ReadUpdateAttempt returns nil without error when no attempt was recorded.
func ReadUpdateAttempt(home string) (*UpdateAttempt, error) {
	path, err := UpdateAttemptPath(home)
	if err != nil {
		return nil, err
	}
	data, err := readRegularBounded(path, 16<<10)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var attempt UpdateAttempt
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&attempt); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return nil, errors.New("Skill Hub update record has trailing content")
	}
	if attempt.SchemaVersion != updateAttemptSchemaVersion {
		return nil, errors.New("unsupported Skill Hub update record")
	}
	return &attempt, nil
}

func boundAttemptError(message string) string {
	message = strings.ToValidUTF8(strings.TrimSpace(message), "")
	if len(message) <= maxAttemptErrorBytes {
		return message
	}
	cut := maxAttemptErrorBytes
	for cut > 0 && (message[cut]&0xC0) == 0x80 {
		cut--
	}
	return message[:cut] + "..."
}
