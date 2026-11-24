package admission

import (
	"fmt"
	"regexp"
	"strings"
	"time"
)

const maxSampleWindowSeconds = 3600

const (
	PriorityEnv = "AGENTCTL_ADMISSION_PRIORITY"
	Interactive = "interactive"
	Background  = "background"
)

var bucketName = regexp.MustCompile("^[a-z][a-z0-9_-]{0,63}$")
var holderID = regexp.MustCompile("^[a-zA-Z0-9][a-zA-Z0-9._:/-]{0,127}$")

func ValidBucket(name string) bool       { return bucketName.MatchString(name) }
func ValidHolder(holder string) bool     { return holderID.MatchString(holder) }
func ValidPriority(priority string) bool { return priority == Interactive || priority == Background }

type Config struct {
	Buckets map[string]Bucket `json:"buckets"`
}

type Bucket struct {
	Harness                 string   `json:"harness"`
	Model                   string   `json:"model,omitempty"`
	ModelPrefix             string   `json:"model_prefix,omitempty"`
	Enabled                 *bool    `json:"enabled,omitempty"`
	InitialWindow           int      `json:"initial_window"`
	MinWindow               int      `json:"min_window"`
	MaxWindow               int      `json:"max_window"`
	StaticFallbackWindow    int      `json:"static_fallback_window,omitempty"`
	TargetThrottleRate      *float64 `json:"target_throttle_rate,omitempty"`
	DecreaseFactor          float64  `json:"decrease_factor,omitempty"`
	IncreaseIntervalSeconds int      `json:"increase_interval_seconds,omitempty"`
	SampleWindowSeconds     int      `json:"sample_window_seconds,omitempty"`
	MaxWait                 string   `json:"max_wait,omitempty"`
}

func (b Bucket) Active() bool { return b.Enabled == nil || *b.Enabled }

func (b Bucket) fallback() int {
	if b.StaticFallbackWindow == 0 {
		return b.InitialWindow
	}
	return b.StaticFallbackWindow
}

func (b Bucket) target() float64 {
	if b.TargetThrottleRate == nil {
		return 0.08
	}
	return *b.TargetThrottleRate
}

func (b Bucket) decrease() float64 {
	if b.DecreaseFactor == 0 {
		return 0.7
	}
	return b.DecreaseFactor
}

func (b Bucket) interval() time.Duration {
	if b.IncreaseIntervalSeconds == 0 {
		return time.Minute
	}
	return time.Duration(b.IncreaseIntervalSeconds) * time.Second
}

func (b Bucket) lookback() time.Duration {
	if b.SampleWindowSeconds == 0 {
		return 5 * time.Minute
	}
	return time.Duration(b.SampleWindowSeconds) * time.Second
}

func (b Bucket) Wait() time.Duration {
	if b.MaxWait == "" {
		return 5 * time.Minute
	}
	value, _ := time.ParseDuration(b.MaxWait)
	return value
}

func (c *Config) Validate() error {
	if c == nil {
		return nil
	}
	if len(c.Buckets) == 0 || len(c.Buckets) > 32 {
		return fmt.Errorf("admission.buckets needs 1 to 32 buckets")
	}
	selectors := map[string]bool{}
	for name, b := range c.Buckets {
		model := b.Model + b.ModelPrefix
		if !bucketName.MatchString(name) || b.Harness != "opencode" ||
			(b.Model == "") == (b.ModelPrefix == "") || len(model) > 192 ||
			!strings.Contains(model, "/") || strings.ContainsAny(model, " \t\r\n") {
			return fmt.Errorf("invalid admission bucket name or OpenCode model selector")
		}
		if b.MinWindow < 1 || b.InitialWindow < b.MinWindow || b.MaxWindow < b.InitialWindow || b.MaxWindow > 256 ||
			b.fallback() < b.MinWindow || b.fallback() > b.MaxWindow {
			return fmt.Errorf("invalid window bounds for admission bucket %q", name)
		}
		if b.target() < 0 || b.target() > 1 || b.decrease() <= 0 || b.decrease() >= 1 ||
			b.IncreaseIntervalSeconds < 0 || b.IncreaseIntervalSeconds > 3600 ||
			b.SampleWindowSeconds < 0 || b.SampleWindowSeconds > maxSampleWindowSeconds || b.Wait() <= 0 || b.Wait() > time.Hour {
			return fmt.Errorf("invalid controller settings for admission bucket %q", name)
		}
		key := b.Model + ":" + b.ModelPrefix
		if selectors[key] {
			return fmt.Errorf("duplicate admission bucket model selector")
		}
		selectors[key] = true
	}
	return nil
}

// Exact selectors win over prefixes, then the longest prefix wins. A disabled
// selector still shadows broader rules so it can serve as a targeted switch.
func (c *Config) Match(harness, model string) (string, Bucket) {
	var name string
	var found Bucket
	if c == nil || model == "" {
		return name, found
	}
	for key, b := range c.Buckets {
		if b.Harness != harness {
			continue
		}
		if b.Model == model {
			return key, b
		}
		if b.ModelPrefix != "" && strings.HasPrefix(model, b.ModelPrefix) && len(b.ModelPrefix) > len(found.ModelPrefix) {
			name, found = key, b
		}
	}
	return name, found
}
