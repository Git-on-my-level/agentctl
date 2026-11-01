// Package capacity reads published plan-usage facts and chooses a pool
// candidate from them. agentctl never measures usage or calls a provider: an
// external producer writes the facts file, and every tool that writes it is
// optional and unknown here.
package capacity

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	// MaxFactsBytes bounds one facts file. Larger files are invalid, not truncated.
	MaxFactsBytes   = 1 << 20
	maxAccounts     = 256
	maxWindows      = 32
	maxIdentifier   = 128
	maxClockSkew    = 5 * time.Minute
	factsSchemaOnly = 1
)

// Status says what agentctl could use from the facts file.
const (
	StatusOK            = "ok"
	StatusNotConfigured = "not_configured"
	StatusMissing       = "missing"
	StatusStale         = "stale"
	StatusInvalid       = "invalid"
)

// Facts is one published usage snapshot. It holds observations only; thresholds
// and order stay in agentctl configuration. Unknown fields are ignored so
// producers can add to it.
type Facts struct {
	SchemaVersion int       `json:"schema_version"`
	GeneratedAt   time.Time `json:"generated_at"`
	Accounts      []Account `json:"accounts"`
}

type Account struct {
	ID      string   `json:"id"`
	Windows []Window `json:"windows"`
}

// Window is one usage limit. Names are opaque and matched exactly. UsedRatio may
// exceed 1 on plans that keep running past their allowance.
type Window struct {
	Name       string     `json:"name"`
	UsedRatio  float64    `json:"used_ratio"`
	ResetsAt   *time.Time `json:"resets_at,omitempty"`
	ObservedAt *time.Time `json:"observed_at,omitempty"`
}

// DecodeFacts accepts one bounded facts document.
func DecodeFacts(body []byte) (Facts, error) {
	if len(body) > MaxFactsBytes {
		return Facts{}, errors.New("facts file exceeds its size limit")
	}
	var facts Facts
	if err := json.Unmarshal(body, &facts); err != nil {
		return Facts{}, errors.New("facts file is not valid JSON")
	}
	if facts.SchemaVersion != factsSchemaOnly {
		return Facts{}, errors.New("facts schema_version must be 1")
	}
	if facts.GeneratedAt.IsZero() {
		return Facts{}, errors.New("facts generated_at is required")
	}
	if len(facts.Accounts) > maxAccounts {
		return Facts{}, errors.New("facts list too many accounts")
	}
	seen := make(map[string]struct{}, len(facts.Accounts))
	for _, account := range facts.Accounts {
		if !validIdentifier(account.ID) {
			return Facts{}, errors.New("facts account id is invalid")
		}
		if _, ok := seen[account.ID]; ok {
			return Facts{}, fmt.Errorf("facts repeat account %q", account.ID)
		}
		seen[account.ID] = struct{}{}
		if len(account.Windows) > maxWindows {
			return Facts{}, errors.New("facts list too many windows")
		}
		for _, window := range account.Windows {
			if !validIdentifier(window.Name) {
				return Facts{}, errors.New("facts window name is invalid")
			}
			if window.UsedRatio < 0 || math.IsNaN(window.UsedRatio) || math.IsInf(window.UsedRatio, 0) {
				return Facts{}, errors.New("facts used_ratio must be a nonnegative number")
			}
		}
	}
	return facts, nil
}

func validIdentifier(value string) bool {
	return value != "" && len(value) <= maxIdentifier && utf8.ValidString(value) && strings.TrimSpace(value) == value &&
		strings.IndexFunc(value, unicode.IsControl) < 0
}

// Load reads the facts file and reports whether it is usable. It never fails a
// launch: every problem comes back as a status, with no facts. A file older than
// maxAge is stale; one dated well in the future can't be trusted either.
func Load(path string, maxAge time.Duration, now time.Time) (*Facts, string) {
	if path == "" {
		return nil, StatusNotConfigured
	}
	// Nonblocking, so a FIFO planted at this path can't hang a launch.
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, StatusMissing
		}
		return nil, StatusInvalid
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, StatusInvalid
	}
	body, err := io.ReadAll(io.LimitReader(file, MaxFactsBytes+1))
	if err != nil {
		return nil, StatusInvalid
	}
	facts, err := DecodeFacts(body)
	if err != nil {
		return nil, StatusInvalid
	}
	if facts.GeneratedAt.After(now.Add(maxClockSkew)) {
		return nil, StatusInvalid
	}
	if now.Sub(facts.GeneratedAt) > maxAge {
		return &facts, StatusStale
	}
	return &facts, StatusOK
}
