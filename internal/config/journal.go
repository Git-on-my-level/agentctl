package config

import (
	"fmt"
	"time"
)

// Journal is host-local maintenance policy. Config bundles cannot turn on
// deletion or override a local kill switch. Omitted fields ship retention on.
type Journal struct {
	Retention *JournalRetention `json:"retention,omitempty"`
}
type JournalRetention struct {
	Enabled *bool `json:"enabled,omitempty"`
	Days    int   `json:"days,omitempty"`
}

const DefaultJournalRetentionDays = 14

func (j *Journal) RetentionPolicy() (enabled bool, age time.Duration) {
	days := DefaultJournalRetentionDays
	enabled = true
	if j != nil && j.Retention != nil {
		if j.Retention.Enabled != nil {
			enabled = *j.Retention.Enabled
		}
		if j.Retention.Days != 0 {
			days = j.Retention.Days
		}
	}
	return enabled, time.Duration(days) * 24 * time.Hour
}
func (j Journal) Validate() error {
	if j.Retention != nil && (j.Retention.Days < 0 || j.Retention.Days > 3650) {
		return fmt.Errorf("retention.days must be 1 through 3650, or 0 for the 14-day default")
	}
	return nil
}
