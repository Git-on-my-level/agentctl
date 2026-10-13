package skillpack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/config"
)

// SelectionConflict names a Skill Hub remote or ref disagreement. It unwraps
// to ErrConflict so existing conflict handling still applies.
type SelectionConflict struct {
	Reason     string
	Message    string
	Recorded   HubSelection
	Configured HubSelection
	Digest     string
}

func (c *SelectionConflict) Error() string {
	if c == nil {
		return ""
	}
	if c.Reason != "" && c.Message != "" {
		return c.Reason + ": " + c.Message
	}
	if c.Message != "" {
		return c.Message
	}
	return c.Reason
}

func (c *SelectionConflict) Unwrap() error { return ErrConflict }

// HubRebind is the plan or apply result for a remote-only Skill Hub rebind.
type HubRebind struct {
	Operation          string       `json:"operation"`
	Applicable         bool         `json:"applicable"`
	Reason             string       `json:"reason"`
	Message            string       `json:"message,omitempty"`
	Recorded           HubSelection `json:"recorded"`
	Configured         HubSelection `json:"configured"`
	AppliedCommit      string       `json:"applied_commit,omitempty"`
	CheckoutCommit     string       `json:"checkout_commit,omitempty"`
	Digest             string       `json:"digest,omitempty"`
	Proof              string       `json:"proof,omitempty"`
	NetworkAccess      bool         `json:"network_access"`
	ApplyNetworkAccess bool         `json:"apply_network_access"`
	ApplyMutates       bool         `json:"apply_mutates"`
	Applied            bool         `json:"applied"`
	checkoutPath       string
	statePath          string
	appliedDigest      string
}

func PlanHubRebind(ctx context.Context, home string, selection HubSelection) (HubRebind, error) {
	return planHubRebind(ctx, config.NativeSourceGit{}, home, selection)
}

func ApplyHubRebind(ctx context.Context, home string, selection HubSelection, expectedDigest string, now time.Time) (HubRebind, error) {
	return applyHubRebind(ctx, config.NativeSourceGit{}, home, selection, expectedDigest, now)
}

func planHubRebind(ctx context.Context, git config.SourceGit, home string, selection HubSelection) (HubRebind, error) {
	if git == nil {
		return HubRebind{}, errors.New("nil Git runner")
	}
	selection, err := NormalizeHubSelection(selection)
	if err != nil {
		return HubRebind{}, err
	}
	checkoutPath, statePath, err := DefaultHubPaths(home)
	if err != nil {
		return HubRebind{}, err
	}
	plan := HubRebind{
		Operation: "rebind", Configured: selection, ApplyNetworkAccess: true, ApplyMutates: true,
		checkoutPath: checkoutPath, statePath: statePath,
	}
	state, err := loadHubState(statePath)
	if errors.Is(err, os.ErrNotExist) {
		plan.Reason, plan.Message = "not_initialized", "Skill Hub source is not initialized on this host"
		return plan, nil
	}
	if err != nil {
		return HubRebind{}, err
	}
	plan.Recorded = state.Selection
	plan.AppliedCommit = state.AppliedCommit
	plan.appliedDigest = state.RebindDigest
	if state.Selection.Ref != selection.Ref {
		plan.Reason = "ref_changed"
		if state.Selection.Remote != selection.Remote {
			plan.Reason = "remote_and_ref_changed"
		}
		plan.Message = "rebind changes only the Skill Hub remote; a ref change is not a rebind"
		plan.Digest = hubRebindDigest(plan)
		return plan, nil
	}
	if state.Selection.Remote == selection.Remote {
		plan.Reason, plan.Message = "already_bound", "Skill Hub remote is already the configured remote"
		plan.Digest = hubRebindDigest(plan)
		return plan, nil
	}
	head, problem := hubCheckoutProof(ctx, git, checkoutPath, state.AppliedCommit)
	plan.CheckoutCommit = head
	if problem != "" {
		plan.Reason = problem
		if problem == "checkout_dirty" {
			plan.Message = "Skill Hub checkout has local changes"
		} else {
			plan.Message = "Skill Hub checkout does not match the pinned commit"
		}
		plan.Digest = hubRebindDigest(plan)
		return plan, nil
	}
	plan.Applicable = true
	plan.Reason = "remote_changed"
	plan.Message = "configured Skill Hub remote differs from the recorded remote"
	plan.Proof = "apply fetches the configured ref and requires the pinned commit to be an ancestor of that ref; a fork that contains the commit also passes"
	plan.Digest = hubRebindDigest(plan)
	return plan, nil
}

func applyHubRebind(ctx context.Context, git config.SourceGit, home string, selection HubSelection, expectedDigest string, now time.Time) (HubRebind, error) {
	if strings.TrimSpace(expectedDigest) == "" {
		return HubRebind{}, errors.New("rebind apply requires a digest")
	}
	lock, err := acquireLock(home)
	if err != nil {
		return HubRebind{}, err
	}
	defer lock.Close()
	plan, err := planHubRebind(ctx, git, home, selection)
	if err != nil {
		return HubRebind{}, err
	}
	if plan.Reason == "already_bound" && (plan.Digest == expectedDigest || plan.appliedDigest == expectedDigest) {
		return plan, nil
	}
	if plan.Digest != expectedDigest {
		return HubRebind{}, &SelectionConflict{
			Reason: "digest_mismatch", Message: "rebind digest does not match the current plan",
			Recorded: plan.Recorded, Configured: plan.Configured, Digest: plan.Digest,
		}
	}
	if plan.Reason == "already_bound" {
		return plan, nil
	}
	if !plan.Applicable {
		return HubRebind{}, &SelectionConflict{
			Reason: plan.Reason, Message: plan.Message, Recorded: plan.Recorded, Configured: plan.Configured,
		}
	}
	if _, err := git.Run(ctx, plan.checkoutPath, "fetch", "--no-tags", plan.Configured.Remote, plan.Configured.Ref); err != nil {
		return HubRebind{}, err
	}
	if _, err := git.Run(ctx, plan.checkoutPath, "merge-base", "--is-ancestor", plan.AppliedCommit, "FETCH_HEAD"); err != nil {
		return HubRebind{}, &SelectionConflict{
			Reason: "pinned_commit_absent", Message: "configured Skill Hub remote does not contain the pinned commit",
			Recorded: plan.Recorded, Configured: plan.Configured,
		}
	}
	if _, err := git.Run(ctx, plan.checkoutPath, "remote", "set-url", "origin", plan.Configured.Remote); err != nil {
		return HubRebind{}, err
	}
	state, err := loadHubState(plan.statePath)
	if err != nil {
		_, _ = git.Run(ctx, plan.checkoutPath, "remote", "set-url", "origin", plan.Recorded.Remote)
		return HubRebind{}, err
	}
	state.Selection.Remote = plan.Configured.Remote
	state.RebindDigest = plan.Digest
	state.CheckedOn = now.UTC().Format("2006-01-02")
	if err := saveHubState(plan.statePath, state, true); err != nil {
		_, _ = git.Run(ctx, plan.checkoutPath, "remote", "set-url", "origin", plan.Recorded.Remote)
		return HubRebind{}, err
	}
	plan.Applied = true
	plan.NetworkAccess = true
	return plan, nil
}

func hubCheckoutProof(ctx context.Context, git config.SourceGit, checkoutPath, applied string) (string, string) {
	status, err := git.Run(ctx, checkoutPath, "status", "--porcelain=v1", "--untracked-files=all")
	if err != nil {
		return "", "checkout_revision"
	}
	head, err := git.Run(ctx, checkoutPath, "rev-parse", "HEAD^{commit}")
	if err != nil {
		return "", "checkout_revision"
	}
	commit := strings.TrimSpace(string(head))
	if len(bytes.TrimSpace(status)) != 0 {
		return commit, "checkout_dirty"
	}
	if commit != applied {
		return commit, "checkout_revision"
	}
	return commit, ""
}

func hubRebindDigest(plan HubRebind) string {
	body := struct {
		RecordedRemote         string `json:"recorded_remote"`
		ConfiguredRemote       string `json:"configured_remote"`
		RecordedRef            string `json:"recorded_ref"`
		ConfiguredRef          string `json:"configured_ref"`
		RecordedManifestPath   string `json:"recorded_manifest_path"`
		ConfiguredManifestPath string `json:"configured_manifest_path"`
		AppliedCommit          string `json:"applied_commit"`
		CheckoutCommit         string `json:"checkout_commit"`
	}{
		RecordedRemote: plan.Recorded.Remote, ConfiguredRemote: plan.Configured.Remote,
		RecordedRef: plan.Recorded.Ref, ConfiguredRef: plan.Configured.Ref,
		RecordedManifestPath: plan.Recorded.ManifestPath, ConfiguredManifestPath: plan.Configured.ManifestPath,
		AppliedCommit: plan.AppliedCommit, CheckoutCommit: plan.CheckoutCommit,
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func hubSelectionChangeReason(recorded, configured HubSelection) string {
	remote := recorded.Remote != configured.Remote
	ref := recorded.Ref != configured.Ref
	switch {
	case remote && ref:
		return "remote_and_ref_changed"
	case remote:
		return "remote_changed"
	case ref:
		return "ref_changed"
	default:
		return "selection_changed"
	}
}

func SelectionDriftReason(recorded, configured HubSelection) string {
	if recorded.Remote != configured.Remote || recorded.Ref != configured.Ref {
		return hubSelectionChangeReason(recorded, configured)
	}
	if recorded.ManifestPath != configured.ManifestPath {
		return "manifest_path_changed"
	}
	return "selection_changed"
}
