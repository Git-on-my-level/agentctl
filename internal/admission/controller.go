package admission

import (
	"math"
	"sort"
	"time"
)

type Sample struct {
	At        time.Time `json:"at"`
	Attempts  int64     `json:"attempts"`
	Throttled int64     `json:"throttled"`
}

type Adjustment struct {
	At     time.Time `json:"at"`
	From   int       `json:"from"`
	To     int       `json:"to"`
	Reason string    `json:"reason"`
}

type controller struct {
	Version        int         `json:"version"`
	Window         int         `json:"window"`
	LastTick       time.Time   `json:"last_tick"`
	LastFull       time.Time   `json:"last_full"`
	LastAdjustment *Adjustment `json:"last_adjustment,omitempty"`
	Samples        []Sample    `json:"samples,omitempty"`
	Admitted       int64       `json:"admitted"`
	Attempts       int64       `json:"attempts"`
	Throttled      int64       `json:"throttled_attempts"`
}

func (s *controller) expire(now time.Time, b Bucket) {
	cutoff := now.Add(-b.lookback())
	i := sort.Search(len(s.Samples), func(i int) bool { return s.Samples[i].At.After(cutoff) })
	s.Samples = s.Samples[i:]
}

func (s *controller) counts(now time.Time) (attempts, throttled int64) {
	for _, sample := range s.Samples {
		if !sample.At.After(now) {
			attempts += sample.Attempts
			throttled += sample.Throttled
		}
	}
	return
}

func (s *controller) observe(sample Sample) {
	// Aggregate by second, retaining the latest actual timestamp in each
	// bucket so a recently accepted sample never expires prematurely.
	second := sample.At.Truncate(time.Second)
	i := sort.Search(len(s.Samples), func(i int) bool { return !s.Samples[i].At.Before(second) })
	if i < len(s.Samples) && s.Samples[i].At.Truncate(time.Second).Equal(second) {
		if sample.At.After(s.Samples[i].At) {
			s.Samples[i].At = sample.At
		}
		s.Samples[i].Attempts += sample.Attempts
		s.Samples[i].Throttled += sample.Throttled
	} else {
		s.Samples = append(s.Samples, Sample{})
		copy(s.Samples[i+1:], s.Samples[i:])
		s.Samples[i] = sample
	}
	s.Attempts += sample.Attempts
	s.Throttled += sample.Throttled
}

func (s *controller) tick(now time.Time, b Bucket, inUse int) *Adjustment {
	if now.Sub(s.LastTick) < b.interval() {
		if inUse >= s.Window && (!s.LastFull.After(s.LastTick) || now.Sub(s.LastFull) >= time.Second) {
			s.LastFull = now
		}
		return nil
	}
	demand := inUse >= s.Window || s.LastFull.After(s.LastTick) && now.Sub(s.LastFull) <= b.interval()
	s.LastTick = now
	attempts, throttled := s.counts(now)
	if attempts == 0 {
		return nil
	}
	rate := float64(throttled) / float64(attempts)
	next, reason := s.Window, ""
	switch {
	case rate > b.target():
		next, reason = max(b.MinWindow, int(math.Floor(float64(s.Window)*b.decrease()))), "above_target"
	case rate < b.target() && demand:
		next, reason = min(b.MaxWindow, s.Window+1), "below_target_with_demand"
	}
	if next == s.Window {
		return nil
	}
	s.LastAdjustment = &Adjustment{At: now, From: s.Window, To: next, Reason: reason}
	s.Window = next
	return s.LastAdjustment
}
