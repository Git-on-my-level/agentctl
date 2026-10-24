package anxrun

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/launchrecipe"
	"github.com/Git-on-my-level/agentctl/internal/route"
)

var digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
var modelNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9./_\[\]=,-]{0,127}$`)

func safeHarness(value string) string {
	if value == "generic-process" || value == "multica" {
		return value
	}
	if canonical, err := launchrecipe.CanonicalHarness(value); err == nil && value == canonical {
		return value
	}
	return "other"
}

func safeModel(harness, value string, catalog []route.ModelRecord) string {
	for _, item := range catalog {
		canonical, err := launchrecipe.CanonicalHarness(item.Adapter)
		if err == nil && canonical == harness && item.Model == value && modelNamePattern.MatchString(value) && !sensitiveText(value) {
			return value
		}
	}
	return "other"
}

func safeAvailability(value string) string {
	switch value {
	case AvailabilityStored, AvailabilityOmittedByPolicy, AvailabilityUnavailableAtSource:
		return value
	default:
		return AvailabilityUnavailableAtSource
	}
}

// Allowlisted creates a fresh document, so newly added local fields cannot
// accidentally cross the egress boundary. Call both before persistence and
// immediately before delivery (including old on-disk registrations).
func Allowlisted(r Registration, catalog []route.ModelRecord) (Registration, error) {
	if _, err := ids.ParseHostID(r.OriginHostID); err != nil {
		return Registration{}, fmt.Errorf("invalid_run_identity")
	}
	if _, err := ids.ParseExecutionID(r.ExternalID); err != nil {
		return Registration{}, fmt.Errorf("invalid_run_identity")
	}
	if r.Phase != PhaseStart && r.Phase != PhaseTerminal {
		return Registration{}, fmt.Errorf("invalid_run_phase")
	}
	state := r.State
	switch state {
	case StateStarting, StateRunning, StateCompleted, StateFailed, StateCancelled, StateUnknown:
	default:
		state = StateUnknown
	}
	liveness := r.Liveness
	switch liveness {
	case LivenessAlive, LivenessStale, LivenessUnknow:
	default:
		liveness = LivenessUnknow
	}
	out := Registration{
		SchemaVersion: RegistrationSchemaVersion, Phase: r.Phase, Launcher: "agentctl",
		ExternalID: r.ExternalID, OriginHostID: r.OriginHostID,
		DurableID: "agentctl://" + r.OriginHostID + "/" + r.ExternalID,
		Adapter:   safeHarness(r.Adapter), State: state, Liveness: liveness,
		StartedAt: r.StartedAt, EndedAt: r.EndedAt, BuiltAt: r.BuiltAt,
		Sequence: r.Sequence, ResultCollected: r.ResultCollected,
	}
	out.Model = safeModel(out.Adapter, r.Model, catalog)
	if digestPattern.MatchString(r.RequestKeyHash) {
		out.RequestKeyHash = r.RequestKeyHash
	}
	for _, ref := range r.CardRefs {
		if strings.HasPrefix(ref, cardRefPrefix) && ParseCardSlug(strings.TrimPrefix(ref, cardRefPrefix)) == nil {
			if len(out.CardRefs) == 16 {
				break
			}
			out.CardRefs = append(out.CardRefs, ref)
		}
	}
	if _, err := ids.ParseExecutionID(r.SupersedesExternalID); err == nil {
		out.SupersedesExternalID = r.SupersedesExternalID
	}
	if n := r.NativeSession; n != nil && digestPattern.MatchString(n.Fingerprint) && n.Kind == NativeSessionIDKind {
		provider := n.Provider
		if provider == "claude" || safeHarness(provider) != "other" {
			out.NativeSession = &NativeSession{Provider: provider, Kind: NativeSessionIDKind, Fingerprint: n.Fingerprint}
		}
	}
	if o := r.Outcome; o != nil {
		out.Outcome = &Outcome{Availability: safeAvailability(o.Availability)}
		if o.Failure != nil {
			out.Outcome.Failure = &FailureProjection{Code: "execution_failed", Retryable: o.Failure.Retryable}
		}
		// Only schema v2 was built with the explicit summary-block opt-in.
		if r.SchemaVersion == RegistrationSchemaVersion {
			redacted := RedactSummary(o.Summary, DefaultSummaryMaxBytes)
			out.Outcome.Summary = redacted.Summary
			out.Outcome.SummaryOmittedReason = redacted.OmittedReason
		}
		if out.Outcome.Summary == "" {
			switch o.SummaryOmittedReason {
			case "not_opted_in", SummaryOmittedRedactionFailed:
				out.Outcome.SummaryOmittedReason = o.SummaryOmittedReason
			}
		}
		for _, link := range o.Links {
			if len(out.Outcome.Links) == MaxLinks {
				break
			}
			if safe, ok := SanitizeLink(link.Ref); ok && safe.Kind == link.Kind {
				out.Outcome.Links = append(out.Outcome.Links, safe)
			}
		}
		out.Outcome.HeadAfter, _ = SanitizeHeadAfter(o.HeadAfter)
	}
	return out, nil
}

// hashRequestKey runs under the spool state lock. The random host-local key
// is never sent to ANX; only the domain-separated HMAC leaves the host.
func (s *Spool) hashRequestKey(raw string) (string, error) {
	if raw == "" {
		return "", nil
	}
	path := filepath.Join(s.root, "request-key-hmac")
	key, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return "", err
		}
		if err = writeFileAtomic(path, key); err != nil {
			return "", err
		}
	} else if err != nil {
		return "", err
	}
	if len(key) != 32 {
		return "", fmt.Errorf("invalid_hash_key")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte("agentctl.anx.request-key.v1\x00"))
	mac.Write([]byte(raw))
	return "sha256:" + hex.EncodeToString(mac.Sum(nil)), nil
}
