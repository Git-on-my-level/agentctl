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
	"time"

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
		if err == nil && canonical == harness && item.Model == value && modelNamePattern.MatchString(value) && stringCode(value, 128, false) == 0 {
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
func Allowlisted(r Registration, catalog []route.ModelRecord, verified ...Verification) (Registration, error) {
	policy := Verification{}
	if len(verified) > 0 {
		policy = verified[0]
	}
	for _, value := range []string{r.OriginHostID, r.ExternalID} {
		if code := wordIDCode(value); code != 0 {
			return Registration{}, egressError(code)
		}
	}
	if _, err := ids.ParseHostID(r.OriginHostID); err != nil {
		return Registration{}, egressError(EgressFormat)
	}
	if _, err := ids.ParseExecutionID(r.ExternalID); err != nil {
		return Registration{}, egressError(EgressFormat)
	}
	if r.Phase != PhaseStart && r.Phase != PhaseTerminal {
		return Registration{}, egressError(EgressFormat)
	}
	if r.Sequence < 0 {
		return Registration{}, egressError(EgressFormat)
	}
	if year := r.BuiltAt.UTC().Year(); year < 1 || year > 9999 {
		return Registration{}, egressError(EgressFormat)
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
		BuiltAt:  r.BuiltAt.UTC(),
		Sequence: r.Sequence, ResultCollected: r.ResultCollected,
	}
	out.EgressErrorCode = validEgressCode(r.EgressErrorCode)
	reject := func(code int) {
		if code != 0 && (out.EgressErrorCode == 0 || code < out.EgressErrorCode) {
			out.EgressErrorCode = code
		}
	}
	if code := disclosureCode(r.CWD); code == 0 {
		out.CWD = r.CWD
	} else {
		reject(code)
	}
	// Only the locally resolved origin supplies repository coordinates. Never
	// trust a repository object from a legacy or edited outbox entry.
	if origin := policy.Origin; origin != nil && policy.allowsOwner(origin.Owner) {
		repo := &Repository{}
		for _, field := range []struct {
			source string
			target *string
		}{
			{origin.Host, &repo.Host}, {origin.Owner, &repo.Owner}, {origin.Name, &repo.Name},
		} {
			if code := disclosureCode(field.source); code == 0 {
				*field.target = field.source
			} else {
				reject(code)
			}
		}
		if *repo != (Repository{}) {
			out.Repository = repo
		}
	}
	for _, field := range []struct {
		source *time.Time
		target **time.Time
	}{{r.StartedAt, &out.StartedAt}, {r.EndedAt, &out.EndedAt}} {
		if field.source != nil {
			value := field.source.UTC()
			if value.Year() < 1 || value.Year() > 9999 {
				reject(EgressFormat)
			} else {
				*field.target = &value
			}
		}
	}
	reject(stringCode(r.Adapter, 32, false))
	reject(stringCode(r.Model, 128, false))
	reject(stringCode(r.State, 16, false))
	reject(stringCode(r.Liveness, 16, false))
	out.Model = safeModel(out.Adapter, r.Model, catalog)
	if stringCode(r.RequestKeyHash, 71, true) == 0 && digestPattern.MatchString(r.RequestKeyHash) {
		out.RequestKeyHash = r.RequestKeyHash
	} else if r.RequestKeyHash != "" {
		code := stringCode(r.RequestKeyHash, 71, true)
		if code == 0 {
			code = EgressFormat
		}
		reject(code)
	}
	for _, ref := range r.CardRefs {
		code := stringCode(strings.TrimPrefix(ref, cardRefPrefix), 64, false)
		if code != 0 {
			reject(code)
			continue
		}
		if strings.HasPrefix(ref, cardRefPrefix) && ParseCardSlug(strings.TrimPrefix(ref, cardRefPrefix)) == nil {
			if len(out.CardRefs) == 16 {
				break
			}
			if card, ok := policy.Cards[ref]; ok && card.Ref == ref && safeServerCoordinate(card.ThreadID) {
				out.CardRefs = append(out.CardRefs, card.Ref)
			} else {
				reject(EgressUnverified)
			}
		} else {
			reject(EgressFormat)
		}
	}
	if _, err := ids.ParseExecutionID(r.SupersedesExternalID); err == nil && wordIDCode(r.SupersedesExternalID) == 0 {
		out.SupersedesExternalID = r.SupersedesExternalID
	} else if r.SupersedesExternalID != "" {
		code := wordIDCode(r.SupersedesExternalID)
		if code == 0 {
			code = EgressFormat
		}
		reject(code)
	}
	if n := r.NativeSession; n != nil {
		code := stringCode(n.Provider, 32, false)
		if code == 0 {
			code = stringCode(n.Kind, 32, false)
		}
		if code == 0 {
			code = stringCode(n.Fingerprint, 71, true)
		}
		if code == 0 && (!digestPattern.MatchString(n.Fingerprint) || n.Kind != NativeSessionIDKind || (n.Provider != "claude" && safeHarness(n.Provider) == "other")) {
			code = EgressFormat
		}
		if code == 0 {
			out.NativeSession = &NativeSession{Provider: n.Provider, Kind: NativeSessionIDKind, Fingerprint: n.Fingerprint}
		} else {
			reject(code)
		}
	}
	if o := r.Outcome; o != nil {
		out.Outcome = &Outcome{Availability: safeAvailability(o.Availability)}
		reject(stringCode(o.Availability, 32, false))
		if o.Failure != nil {
			out.Outcome.Failure = &FailureProjection{Code: "execution_failed", Retryable: o.Failure.Retryable}
		}
		reject(validEgressCode(o.EgressErrorCode))
		for _, link := range o.Links {
			if r.SchemaVersion != RegistrationSchemaVersion {
				reject(EgressUnverified)
				continue
			}
			if len(out.Outcome.Links) == MaxLinks {
				break
			}
			if safe, code := validateLink(link); code == 0 {
				if safe.Kind == LinkKindCommit {
					out.Outcome.Links = append(out.Outcome.Links, safe)
				} else if authorized, ok := policy.repository(safe); ok {
					out.Outcome.Links = append(out.Outcome.Links, authorized)
				} else {
					reject(EgressUnverified)
				}
			} else {
				reject(code)
			}
		}
		var err error
		if r.SchemaVersion == RegistrationSchemaVersion {
			out.Outcome.HeadAfter, err = SanitizeHeadAfter(o.HeadAfter)
		}
		if err != nil {
			reject(EgressFormat)
		}
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
