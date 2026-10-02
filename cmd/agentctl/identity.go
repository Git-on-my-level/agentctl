package main

import (
	"context"
	"regexp"
	"runtime"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const identitySchemaVersion = "agentctl.identity.v1"
const identityUsage = "usage: agentctl identity [--json] [--execution exec-... | --provider name [--native-session-id id]]"

// Identity is evidence for optional composition, never an authenticated agent
// principal. Native IDs stay private; only a domain-separated correlator leaves
// this report. Consumers must add their independently enrolled host namespace.
type identityEvidence struct {
	ID         *string `json:"id"`
	Provenance string  `json:"provenance"`
	Confidence string  `json:"confidence"`
}
type identitySession struct {
	identityEvidence
	IDKind string `json:"id_kind"`
}
type identityCapability struct {
	Status     string `json:"status"`
	Provenance string `json:"provenance"`
	Confidence string `json:"confidence"`
	Scope      string `json:"scope"`
	Reason     string `json:"reason"`
}
type identityHarness struct {
	ProviderID   string `json:"provider_id"`
	Availability string `json:"availability"`
	Provenance   string `json:"provenance"`
}
type identityEnvironment struct {
	OS             string  `json:"os"`
	Arch           string  `json:"arch"`
	HostID         *string `json:"host_id"`
	HostProvenance string  `json:"host_provenance"`
}
type identityReport struct {
	SchemaVersion string                        `json:"schema_version"`
	Provider      identityEvidence              `json:"provider"`
	NativeSession identitySession               `json:"native_session"`
	Execution     identityEvidence              `json:"execution"`
	Environment   identityEnvironment           `json:"environment"`
	Capabilities  map[string]identityCapability `json:"capabilities"`
	Harnesses     []identityHarness             `json:"harnesses"`
}

var identityProviderPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)

func unknownIdentity() identityEvidence {
	return identityEvidence{Provenance: "unknown", Confidence: "unknown"}
}
func identityValue(id, provenance, confidence string) identityEvidence {
	return identityEvidence{ID: &id, Provenance: provenance, Confidence: confidence}
}
func validNativeSessionID(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 256 && utf8.ValidString(value) && strings.IndexFunc(value, unicode.IsControl) < 0
}
func sessionIdentity(provider, session, provenance, confidence string) identitySession {
	value := identitySession{identityEvidence: unknownIdentity(), IDKind: "provider_session_sha256"}
	if identityProviderPattern.MatchString(provider) && validNativeSessionID(session) {
		value.identityEvidence = identityValue(adapter.Fingerprint(identitySchemaVersion, "native_session", provider, session), provenance, confidence)
	}
	return value
}
func identityCapabilityValue(status, provenance, confidence, scope, reason string) identityCapability {
	return identityCapability{Status: status, Provenance: provenance, Confidence: confidence, Scope: scope, Reason: reason}
}

func (a *app) identityCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	executionRef, provider, session := "", "", ""
	seen := map[string]bool{}
	for i := 0; i < len(args); i++ {
		flag := args[i]
		if seen[flag] {
			return output.NewError(output.CodeUsage, identityUsage, false)
		}
		seen[flag] = true
		switch flag {
		case "--json":
			renderer.Mode = output.JSON
		case "--execution", "--provider", "--native-session-id":
			if i+1 >= len(args) || strings.TrimSpace(args[i+1]) == "" {
				return output.NewError(output.CodeUsage, identityUsage, false)
			}
			i++
			switch flag {
			case "--execution":
				executionRef = args[i]
			case "--provider":
				provider = canonicalAdapterName(args[i])
			case "--native-session-id":
				session = args[i]
			}
		default:
			return output.NewError(output.CodeUsage, identityUsage, false)
		}
	}
	if (executionRef != "" && provider != "") || (session != "" && provider == "") || (provider != "" && !identityProviderPattern.MatchString(provider)) || (session != "" && !validNativeSessionID(session)) {
		return output.NewError(output.CodeUsage, identityUsage, false)
	}
	explicitExecution := executionRef != ""
	getenv := a.getenv
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	report := identityReport{SchemaVersion: identitySchemaVersion, Provider: unknownIdentity(), Execution: unknownIdentity(), NativeSession: sessionIdentity("", "", "", ""), Environment: identityEnvironment{OS: runtime.GOOS, Arch: runtime.GOARCH, HostProvenance: "unknown"}, Capabilities: map[string]identityCapability{}, Harnesses: identityHarnesses(getenv)}
	for _, name := range []string{"resume", "history", "logs"} {
		report.Capabilities[name] = identityCapabilityValue("unknown", "unknown", "unknown", "agentctl_native_adapter", "not_observed")
	}
	// Explicit provider/session hints stand alone. They must not accidentally
	// borrow an ambient managed execution or its host/session identity.
	if provider != "" {
		report.Provider = identityValue(provider, "explicit", "self_reported")
		report.NativeSession = sessionIdentity(provider, session, "explicit", "self_reported")
	} else {
		managed := executionRef != "" || getenv("AGENTCTL_EXECUTION_ID") != "" || getenv("AGENTCTL_ADAPTER") != "" || getenv("AGENTCTL_AUTHORITY") != ""
		if executionRef == "" {
			executionRef = getenv("AGENTCTL_EXECUTION_ID")
		}
		if managed {
			if p := canonicalAdapterName(getenv("AGENTCTL_ADAPTER")); !explicitExecution && identityProviderPattern.MatchString(p) {
				report.Provider = identityValue(p, "managed_environment", "self_reported")
			}
			if id, err := ids.ParseExecutionID(executionRef); err == nil {
				report.Execution = identityValue(id.String(), "managed_environment", "self_reported")
				if explicitExecution {
					report.Execution = identityValue(id.String(), "explicit", "self_reported")
				}
				found, problem := a.identityFromJournal(ctx, c, id, &report, explicitExecution)
				if problem != nil {
					return problem
				}
				if !found {
					capability := report.Capabilities["resume"]
					capability.Reason = "execution_not_observed"
					report.Capabilities["resume"] = capability
				}
			} else if explicitExecution {
				return output.NewError(output.CodeUsage, "identity requires a typed execution ID", false)
			}
			// Never use CODEX_THREAD_ID here: nested managed children inherit the
			// parent's environment until the native harness supplies its own ID.
		} else if value := getenv("CODEX_THREAD_ID"); validNativeSessionID(value) && !competingNativeIdentityEnvironment(getenv) {
			report.Provider = identityValue("codex", "native_environment", "self_reported")
			report.NativeSession = sessionIdentity("codex", value, "native_environment", "self_reported")
		}
	}
	if err := renderer.Success(output.Success{Result: report, Lines: []output.Line{{Lead: "identity", Fields: []output.Field{{Name: "schema", Value: report.SchemaVersion}, {Name: "provider", Value: identityDisplay(report.Provider)}, {Name: "session", Value: identityDisplay(report.NativeSession.identityEvidence)}, {Name: "execution", Value: identityDisplay(report.Execution)}}}}}); err != nil {
		return output.NewError(output.CodeInternal, "write identity output", false)
	}
	return nil
}

func identityHarnesses(getenv func(string) string) []identityHarness {
	// Reuse bootstrap's executable discovery, without reading any harness
	// configuration, running native binaries, or equating presence with ability.
	specs := append([]bootstrapHarnessSpec(nil), bootstrapHarnessSpecs...)
	specs = append(specs, bootstrapHarnessSpec{Name: "devin", Executables: []string{"devin"}}, bootstrapHarnessSpec{Name: "zcode", Executables: []string{"zcode"}})
	result := make([]identityHarness, 0, len(specs))
	for _, spec := range specs {
		availability := "unavailable"
		for _, executable := range spec.Executables {
			if bootstrapLookPath(executable, getenv) != "" {
				availability = "available"
				break
			}
		}
		result = append(result, identityHarness{ProviderID: canonicalAdapterName(spec.Name), Availability: availability, Provenance: "path_lookup"})
	}
	return result
}

func (a *app) identityFromJournal(ctx context.Context, c common, id ids.ExecutionID, report *identityReport, required bool) (bool, *output.Error) {
	path, err := a.journalPath(c)
	if err != nil {
		if required {
			return false, output.NewError(output.CodeDependencyUnavailable, "identity journal unavailable", false)
		}
		return false, nil
	}
	journal, err := store.Open(path, store.Options{ReadOnly: true, LockTimeout: 50 * time.Millisecond})
	if err != nil {
		if required {
			return false, output.NewError(output.CodeDependencyUnavailable, "identity journal unavailable", true)
		}
		return false, nil
	}
	defer journal.Close()
	execution, err := journal.GetExecution(ctx, id)
	if err != nil {
		if required {
			return false, output.NewError(output.CodeNotFound, "identity execution unavailable", false)
		}
		return false, nil
	}
	provider := canonicalAdapterName(execution.Adapter)
	if identityProviderPattern.MatchString(provider) {
		report.Provider = identityValue(provider, "execution_journal", "observed")
	}
	report.Execution = identityValue(execution.ID.String(), "execution_journal", "observed")
	host := execution.OriginHostID.String()
	report.Environment.HostID = &host
	report.Environment.HostProvenance = "execution_journal"
	// Multica run/issue identifiers are not native conversation identifiers.
	if execution.Authority == model.AuthorityNative {
		report.NativeSession = sessionIdentity(provider, nativeSessionID(execution), "native_stream", "observed")
	}
	report.Capabilities = identityExecutionCapabilities(execution)
	return true, nil
}

func identityExecutionCapabilities(execution model.Execution) map[string]identityCapability {
	result := map[string]identityCapability{}
	for _, name := range []string{"history", "logs"} {
		result[name] = identityCapabilityValue("unsupported", "adapter_contract", "observed", "agentctl_native_adapter", "no_native_retrieval_route")
	}
	resume := identityCapabilityValue("unknown", "execution_journal", "observed", "agentctl_continue", "native_session_not_observed")
	switch {
	case execution.Authority != model.AuthorityNative:
		resume.Status = "unsupported"
		resume.Reason = "authority_owns_continuation"
	case execution.Delegation == nil || execution.Delegation.NativePlan == nil:
		resume.Status = "unsupported"
		resume.Reason = "frozen_launch_recipe_missing"
	case execution.State != model.StateCompleted:
		resume.Reason = "completed_turn_not_observed"
	case nativeSessionID(execution) != "":
		plan := execution.Delegation.NativePlan
		if _, err := adapter.ContinuationArgv(execution.Adapter, plan.Recipe(), plan.PromptDelivery, nativeSessionID(execution)); err != nil {
			resume.Status = "unsupported"
			resume.Reason = "native_resume_route_unavailable"
		} else {
			resume.Status = "supported"
			resume.Reason = "recorded_recipe_requires_continue_preflight"
		}
	}
	result["resume"] = resume
	return result
}

func identityDisplay(value identityEvidence) string {
	if value.ID == nil {
		return "unknown"
	}
	return *value.ID
}

// Competing native markers may themselves be inherited. In either nesting
// direction, a CODEX_THREAD_ID alone cannot identify the current caller. Do
// not inspect or render marker values (one can contain a local path).
func competingNativeIdentityEnvironment(getenv func(string) string) bool {
	return getenv("CLAUDECODE") != "" || getenv("CURSOR_AGENT_COMPLETED_PATH") != ""
}
