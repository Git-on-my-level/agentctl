package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/adapter"
	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/config"
	"github.com/Git-on-my-level/agentctl/internal/contracts"
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/model"
	"github.com/Git-on-my-level/agentctl/internal/output"
)

func (a *app) promoteCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl promote <execution-id> --title <title> [flags]", false)
	}
	sourceID, problem := parseExecutionRef(args[0], c)
	if problem != nil {
		return problem
	}
	var title, handoffFile, project, assignee, assigneeID, status, callerKey string
	plan, supersede := false, false
	for i := 1; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--title":
			value, p := take()
			if p != nil {
				return p
			}
			title = value
		case "--handoff-file":
			value, p := take()
			if p != nil {
				return p
			}
			handoffFile = value
		case "--project":
			value, p := take()
			if p != nil {
				return p
			}
			project = value
		case "--assignee":
			value, p := take()
			if p != nil {
				return p
			}
			assignee = value
		case "--assignee-id":
			value, p := take()
			if p != nil {
				return p
			}
			assigneeID = value
		case "--status":
			value, p := take()
			if p != nil {
				return p
			}
			status = value
		case "--idempotency-key":
			value, p := take()
			if p != nil {
				return p
			}
			callerKey = value
		case "--plan":
			plan = true
		case "--supersede":
			supersede = true
		default:
			return output.NewError(output.CodeUsage, "unknown promote flag", false).WithDetail("flag", args[i])
		}
	}
	if strings.TrimSpace(title) == "" {
		return output.NewError(output.CodeUsage, "--title is required", false)
	}
	if assignee != "" && assigneeID != "" {
		return output.NewError(output.CodeUsage, "--assignee and --assignee-id are mutually exclusive", false)
	}
	if handoffFile != "" {
		absolute, err := filepath.Abs(handoffFile)
		if err != nil {
			return output.Wrap(output.CodeUsage, "resolve handoff file", false, err)
		}
		cwd, err := os.Getwd()
		if err != nil {
			return output.Wrap(output.CodeInternal, "resolve working directory", false, err)
		}
		rel, err := filepath.Rel(cwd, absolute)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return output.NewError(output.CodeUsage, "handoff file must be inside the current working directory", false)
		}
		info, err := os.Lstat(absolute)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return output.NewError(output.CodeUsage, "handoff file must be a regular non-symlink file", false)
		}
		handoffFile = rel
	}
	journal, problem := a.openWrite(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	source, err := journal.GetExecution(ctx, sourceID)
	if err != nil {
		return mapStoreError("read source execution", err)
	}
	if source.Authority != model.AuthorityNative {
		return output.NewError(output.CodeInvalidState, "only direct native executions can be promoted", false)
	}
	cfgPath, err := configPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve config path", false, err)
	}
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return mapConfigError("read promotion config", err)
	}
	profileName, profile, err := cfg.ResolveProfile(c.profile)
	if err != nil {
		return mapConfigError("resolve promotion profile", err)
	}
	if profile.Multica == nil {
		return output.NewError(output.CodeCapabilityUnavailable, "selected profile has no Multica authority", false)
	}
	m := profile.Multica
	keyProjection := map[string]any{"origin_host": source.OriginHostID.String(), "source_execution": source.ID.String(), "source_fingerprints": promotionFingerprints(source), "destination": map[string]any{"server_url": m.ServerURL, "workspace_id": m.WorkspaceID}, "caller_key": callerKey}
	clientKey, _, err := callback.SemanticDedupeKey("multica", 1, keyProjection)
	if err != nil {
		return output.Wrap(output.CodeInternal, "derive promotion key", false, err)
	}
	semanticProjection := map[string]any{"title": title, "handoff_file": handoffFile != "", "project": project, "assignee": assignee, "assignee_id": assigneeID, "status": status, "supersede": supersede}
	semanticBytes, err := callback.CanonicalJSON(semanticProjection)
	if err != nil {
		return output.Wrap(output.CodeInternal, "derive promotion input digest", false, err)
	}
	semanticSum := sha256.Sum256(semanticBytes)
	inputDigest := "sha256:" + hex.EncodeToString(semanticSum[:])
	argv := []string{m.Executable, "--profile", m.Profile, "--workspace-id", m.WorkspaceID, "--server-url", m.ServerURL, "issue", "create", "--title", title, "--client-key", clientKey, "--output", "json"}
	if handoffFile != "" {
		argv = append(argv, "--description-file", handoffFile)
	}
	if project != "" {
		argv = append(argv, "--project", project)
	}
	if assignee != "" {
		argv = append(argv, "--assignee", assignee)
	}
	if assigneeID != "" {
		argv = append(argv, "--assignee-id", assigneeID)
	}
	if status != "" {
		argv = append(argv, "--status", status)
	}
	if plan {
		result := map[string]any{"source_execution_id": source.ID, "authority": "multica", "profile": profileName, "workspace_id": m.WorkspaceID, "client_key": clientKey, "argv": argv, "side_effect_class": output.RemoteCoordinationWrite}
		if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "promotion.plan", Fields: []output.Field{{Name: "source", Value: source.ID}, {Name: "profile", Value: profileName}, {Name: "client_key", Value: clientKey}, {Name: "side_effect_class", Value: output.RemoteCoordinationWrite}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	issue, err := runMulticaIssueCreate(ctx, argv)
	if err != nil {
		if errors.Is(err, errMulticaIssueConflict) {
			return output.Wrap(output.CodeConflict, "Multica issue client key conflicts with changed promotion semantics", false, err).WithDetail("client_key", clientKey).WithDetail("profile", profileName)
		}
		return output.Wrap(output.CodeRemoteFailure, "create or recover Multica issue", true, err).WithDetail("client_key", clientKey).WithDetail("profile", profileName)
	}
	issueID, _ := issue["id"].(string)
	identifier, _ := issue["identifier"].(string)
	if issueID == "" {
		return output.NewError(output.CodeRemoteFailure, "Multica response omitted issue ID", true).WithDetail("client_key", clientKey)
	}
	now := a.now().UTC()
	bindings, err := promotionBindings(m, issueID)
	if err != nil {
		return output.Wrap(output.CodeInternal, "create promotion aliases", false, err)
	}
	destination := model.Execution{Authority: model.AuthorityMultica, Adapter: "multica", Mode: model.ModeMultica, Acquisition: model.AcquisitionPromoted, State: model.StateWaiting, Liveness: model.LivenessUnknown, SourceState: promotionStringPointer("issue_created"), SourceBindings: bindings, Capabilities: promotedCapabilities(now), Supersedes: []ids.ExecutionID{}, Promotion: &model.PromotionLink{Role: "target", CounterpartExecutionID: source.ID, PromotionKey: clientKey, State: model.PromotionActive}, TaskContract: &model.TaskContract{ObjectiveSummary: title, SideEffectBoundary: "multica_issue"}, Observation: model.Observation{Source: model.ObservationDurableOutbox, Integrity: model.IntegrityVerified, ObservedAt: now}}
	if supersede {
		destination.Supersedes = []ids.ExecutionID{source.ID}
	}
	created, reused, err := journal.CreateExecution(ctx, destination, contracts.MutationKey{Scope: "promotion:create", Key: clientKey, InputDigest: inputDigest})
	if err != nil {
		return mapStoreError("record promoted execution", err)
	}
	if reused {
		// Replays may have completed all local steps already. Re-read before CAS.
		created, err = journal.GetExecution(ctx, created.ID)
		if err != nil {
			return mapStoreError("recover promoted execution", err)
		}
	}
	if created.Promotion == nil {
		created.Promotion = &model.PromotionLink{Role: "target", CounterpartExecutionID: source.ID, PromotionKey: clientKey, State: model.PromotionActive}
		created.UpdatedAt = now
		created.Observation.ObservedAt = now
		created, err = journal.UpdateExecution(ctx, created, created.Revision)
		if err != nil {
			return mapStoreError("link promoted execution", err)
		}
	} else if created.Promotion.PromotionKey != clientKey || created.Promotion.CounterpartExecutionID != source.ID {
		return output.NewError(output.CodeConflict, "promoted execution is linked to a different source", false).WithDetail("execution_id", created.ID.String())
	}
	source, err = journal.GetExecution(ctx, source.ID)
	if err != nil {
		return mapStoreError("recover source execution", err)
	}
	if source.Promotion == nil {
		source.Promotion = &model.PromotionLink{Role: "source", CounterpartExecutionID: created.ID, PromotionKey: clientKey, State: model.PromotionActive}
		if supersede {
			source.SupersededBy = &created.ID
		}
		source.UpdatedAt = now
		source.Observation.ObservedAt = now
		source, err = journal.UpdateExecution(ctx, source, source.Revision)
		if err != nil {
			return mapStoreError("link source execution", err)
		}
	} else if source.Promotion.PromotionKey != clientKey || source.Promotion.CounterpartExecutionID != created.ID {
		return output.NewError(output.CodeConflict, "source execution is already linked to a different promotion", false).WithDetail("source_execution_id", source.ID.String())
	}
	if err := appendPromotionEvent(ctx, journal, source, created.ID, clientKey, now); err != nil {
		return mapStoreError("record promotion event", err)
	}
	issueAlias, ok := bindingAlias(created.SourceBindings, "multica_issue")
	if !ok {
		return output.NewError(output.CodeInternal, "promoted execution omitted its issue alias", false).WithDetail("execution_id", created.ID.String())
	}
	result := map[string]any{"execution": redactedExecution(created), "source_execution_id": source.ID, "issue_alias": issueAlias, "identifier": identifier, "app_url": m.AppURL, "client_key": clientKey, "reused": reused}
	lines := []output.Line{{Lead: created.ID.String(), Fields: []output.Field{{Name: "authority", Value: "multica"}, {Name: "state", Value: created.State}, {Name: "source", Value: source.ID}, {Name: "issue", Value: issueAlias}, {Name: "identifier", Value: identifier}, {Name: "reused", Value: reused}}}}
	if err := renderer.Success(output.Success{Result: result, Lines: lines}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

var errMulticaIssueConflict = errors.New("Multica issue client key conflict")

func runMulticaIssueCreate(ctx context.Context, argv []string) (map[string]any, error) {
	if len(argv) == 0 {
		return nil, errors.New("Multica argv is empty")
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Env = os.Environ()
	cmd.Stdin = nil
	var stdout, stderr boundedBuffer
	stdout.limit = 1 << 20
	stderr.limit = 64 << 10
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		message := strings.ToLower(strings.TrimSpace(stderr.String()))
		if strings.Contains(message, "conflicts with the current state") || strings.Contains(message, "client key conflict") || strings.Contains(message, "issue_client_key_conflict") {
			return nil, fmt.Errorf("%w: Multica rejected changed semantics", errMulticaIssueConflict)
		}
		return nil, fmt.Errorf("Multica issue create failed (%T)", err)
	}
	var result map[string]any
	dec := json.NewDecoder(bytes.NewReader(stdout.Bytes()))
	dec.UseNumber()
	if err := dec.Decode(&result); err != nil {
		return nil, errors.New("Multica returned invalid JSON")
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("Multica returned trailing output")
	}
	return result, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Buffer.Len()+len(p) > b.limit {
		return 0, errors.New("process output exceeded limit")
	}
	return b.Buffer.Write(p)
}

func promotionFingerprints(source model.Execution) []string {
	out := make([]string, 0, len(source.SourceBindings))
	for _, binding := range source.SourceBindings {
		out = append(out, binding.Fingerprint)
	}
	return out
}

func promotionBindings(m *config.Multica, issueID string) ([]model.SourceBinding, error) {
	bindings := make([]model.SourceBinding, 0, 5)
	for _, item := range []struct {
		kind, value string
		typ         ids.Type
	}{{"runtime_executable", m.Executable, ids.TypeSource}, {"multica_profile", m.Profile, ids.TypeSource}, {"multica_endpoint", m.ServerURL, ids.TypeSource}, {"multica_workspace", m.WorkspaceID, ids.TypeProject}, {"multica_issue", issueID, ids.TypeIssue}} {
		alias, err := ids.New(item.typ)
		if err != nil {
			return nil, err
		}
		value := item.value
		bindings = append(bindings, model.SourceBinding{Kind: item.kind, AliasID: alias, Fingerprint: adapter.Fingerprint("multica", item.kind, item.value), OpaqueID: &value})
	}
	return bindings, nil
}

func bindingAlias(bindings []model.SourceBinding, kind string) (ids.ID, bool) {
	for _, binding := range bindings {
		if binding.Kind == kind {
			return binding.AliasID, true
		}
	}
	return ids.ID{}, false
}

func promotedCapabilities(now time.Time) model.CapabilitySnapshot {
	reason := "issue is durable; a concrete run is bound when Multica dispatches"
	return model.CapabilitySnapshot{NegotiatedAt: now, AdapterVersion: "0.1.0", Items: []model.CapabilityItem{{Name: "durable_idempotency", Status: model.CapabilitySupported, Source: "multica_api", SemanticsVersion: 1}, {Name: "events", Status: model.CapabilityDegraded, Source: "multica_api", SemanticsVersion: 1, Reason: &reason, Constraints: map[string]any{"cross_restart": true, "scope": "workspace_events", "source": "native_cli"}}, {Name: "snapshot", Status: model.CapabilityUnavailable, Source: "manifest", SemanticsVersion: 1, Reason: &reason}}}
}

func appendPromotionEvent(ctx context.Context, journal contracts.Journal, source model.Execution, destination ids.ExecutionID, key string, now time.Time) error {
	payload := map[string]any{"counterpart_execution": destination.String(), "authority": "multica"}
	projectionValue := map[string]any{"source_execution": source.ID.String(), "counterpart_execution": destination.String(), "promotion_key": key, "kind": "promoted"}
	dedupe, projection, err := callback.SemanticDedupeKey(source.Adapter, 1, projectionValue)
	if err != nil {
		return err
	}
	event := model.Event{ExecutionID: source.ID, OriginHostID: source.OriginHostID, Ordering: model.OrderingObservation, Kind: model.EventPromoted, Authority: source.Authority, Adapter: source.Adapter, ObservedAt: now, DedupeKey: dedupe, DedupeVersion: 1, Payload: payload}
	_, _, err = journal.AppendEvent(ctx, event, projection)
	return err
}

func promotionStringPointer(value string) *string { return &value }
func redactedExecution(value model.Execution) model.Execution {
	value.CWD = nil
	value.Repository = nil
	value.SourceBindings = append([]model.SourceBinding(nil), value.SourceBindings...)
	for i := range value.SourceBindings {
		value.SourceBindings[i].OpaqueID = nil
	}
	return value
}
