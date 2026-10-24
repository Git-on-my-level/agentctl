// Package anxrun builds and delivers ANX run-link registrations for agentctl
// executions that carry an anx.card.<slug> label.
//
// Boundaries, per docs/design/anx-run-links.md:
//
//   - Registration never blocks or fails a run. Every error is spooled,
//     surfaced through `agentctl anx status`, and retried later.
//   - agentctl adds no ANX credential handling. Deliveries shell out to the
//     configured `anx` executable, which owns authentication and its own
//     config; agentctl passes only explicit workspace/base-url/config-dir
//     values from its own config and never hard-codes a base URL.
//   - Outcomes are read through the journal's read-only outcome accessor.
//     A registration never acknowledges collection: acknowledgement semantics
//     stay with the parent's explicit `result`/`await` call.
//   - Summaries are derived deterministically by RedactSummary; no model runs.
package anxrun

import (
	"github.com/Git-on-my-level/agentctl/internal/adapter"
)

// Identity correlator domain constants. The fingerprint MUST stay byte-for-byte
// identical to `agentctl identity` (see docs/identity.md): the v1 correlator
// hashes each component followed by a NUL byte, including the last. Reuse
// adapter.Fingerprint rather than re-deriving the formula.
const (
	IdentityV1Domain   = "agentctl.identity.v1"
	NativeSessionScope = "native_session"
	// NativeSessionIDKind is the ANX sessions-API value for this fingerprint.
	NativeSessionIDKind = "provider_session_sha256"
)

// NativeSessionFingerprint returns the identity v1 correlator for one native
// session. It reuses adapter.Fingerprint so the value can never diverge from
// `agentctl identity --json`.
func NativeSessionFingerprint(provider, nativeSessionID string) string {
	return adapter.Fingerprint(IdentityV1Domain, NativeSessionScope, provider, nativeSessionID)
}

// CanonicalProvider maps an agentctl adapter name onto the identity-report
// canonical provider id (claude-code -> claude),
// matching the normalization used by `agentctl identity`.
func CanonicalProvider(adapterName string) string {
	switch adapterName {
	case "claude-code":
		return "claude"
	default:
		return adapterName
	}
}
