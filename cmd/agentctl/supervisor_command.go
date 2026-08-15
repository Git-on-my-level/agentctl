package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/callback"
	"github.com/Git-on-my-level/agentctl/internal/output"
	agentruntime "github.com/Git-on-my-level/agentctl/internal/runtime"
	"github.com/Git-on-my-level/agentctl/internal/store"
	"github.com/Git-on-my-level/agentctl/internal/supervisor"
)

func (a *app) supervisorCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "usage: agentctl supervisor run|status|plan ...", false)
	}
	switch args[0] {
	case "run":
		return a.supervisorRun(ctx, renderer, c, args[1:])
	case "status":
		return a.supervisorStatus(ctx, renderer, args[1:])
	case "plan":
		return a.supervisorPlan(renderer, args[1:])
	default:
		return output.NewError(output.CodeUsage, "supervisor requires run, status, or plan", false)
	}
}

func (a *app) supervisorRun(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	cfg := supervisor.DefaultConfig()
	once := false
	for i := 0; i < len(args); i++ {
		take := func() (string, *output.Error) {
			if i+1 >= len(args) {
				return "", output.NewError(output.CodeUsage, args[i]+" requires a value", false)
			}
			i++
			return args[i], nil
		}
		switch args[i] {
		case "--once":
			once = true
		case "--socket":
			value, problem := take()
			if problem != nil {
				return problem
			}
			cfg.SocketPath = value
		case "--state-dir":
			value, problem := take()
			if problem != nil {
				return problem
			}
			cfg.StateDir = value
		case "--interval":
			value, problem := take()
			if problem != nil {
				return problem
			}
			parsed, err := time.ParseDuration(value)
			if err != nil || parsed <= 0 {
				return output.NewError(output.CodeUsage, "interval must be a positive Go duration", false)
			}
			cfg.CycleInterval = parsed
		default:
			return output.NewError(output.CodeUsage, "unknown supervisor run flag", false).WithDetail("flag", args[i])
		}
	}
	journalPath, err := a.journalPath(c)
	if err != nil {
		return output.Wrap(output.CodeInternal, "resolve journal path", false, err)
	}
	// Every bridge opens the journal only around one state transition. Callback
	// transport I/O and authority reprobes must not hold bbolt's process lock.
	resolvedStateDir := cfg.StateDir
	factory := supervisor.DependenciesFactoryFunc(func(cycleCtx context.Context) (supervisor.CycleDependencies, error) {
		bridge := pathSupervisorExecutions{path: journalPath}
		deps := supervisor.Dependencies{
			Executions: bridge,
			Reprober:   bridge,
			Outbox:     agentruntime.OutboxBridge{Store: journalSupervisorOutbox{path: journalPath}},
			Deliverer:  agentruntime.DelivererBridge{Transport: callbackTransport{journalPath: journalPath, stateDir: resolvedStateDir}},
		}
		return supervisor.CycleDependencies{Dependencies: deps}, nil
	})
	deps := supervisor.Dependencies{Factory: factory}
	service, err := supervisor.New(cfg, deps)
	if err != nil {
		return output.Wrap(output.CodeUsage, "configure supervisor", false, err)
	}
	resolvedStateDir = service.Config().StateDir
	if once {
		report, err := service.RunOnce(ctx)
		if err != nil {
			return output.Wrap(output.CodeRemoteFailure, "supervisor cycle failed", true, err)
		}
		if err := renderer.Success(output.Success{Result: report, Lines: []output.Line{{Lead: "supervisor.cycle", Fields: []output.Field{{Name: "recovered", Value: report.Recovery.Applied}, {Name: "delivered", Value: report.Delivery.Delivered}, {Name: "retried", Value: report.Delivery.Retried}, {Name: "dead_lettered", Value: report.Delivery.DeadLettered}}}}}); err != nil {
			return output.Wrap(output.CodeInternal, "write output", false, err)
		}
		return nil
	}
	if renderer.Mode == output.JSON {
		// A long-running process cannot emit a final success envelope. Emit one
		// compact startup document before blocking.
		_ = renderer.Success(output.Success{Result: map[string]any{"state": "starting", "socket": service.Config().SocketPath}, Lines: nil})
	} else {
		_ = renderer.Success(output.Success{Result: nil, Lines: []output.Line{{Lead: "supervisor", Fields: []output.Field{{Name: "state", Value: "starting"}, {Name: "socket", Value: service.Config().SocketPath}}}}})
	}
	if err := service.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
		return output.Wrap(output.CodeRemoteFailure, "supervisor stopped", true, err)
	}
	return nil
}

func (a *app) supervisorStatus(ctx context.Context, renderer output.Renderer, args []string) *output.Error {
	socketPath := ""
	for i := 0; i < len(args); i++ {
		if args[i] != "--socket" || i+1 >= len(args) {
			return output.NewError(output.CodeUsage, "usage: agentctl supervisor status [--socket path]", false)
		}
		i++
		socketPath = args[i]
	}
	if socketPath == "" {
		var problem *output.Error
		socketPath, problem = a.defaultSupervisorSocket()
		if problem != nil {
			return problem
		}
	}
	conn, err := (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
	if err != nil {
		return output.Wrap(output.CodeDependencyUnavailable, "connect supervisor", true, err)
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(supervisor.RPCRequest{Op: "status"}); err != nil {
		return output.Wrap(output.CodeRemoteFailure, "request supervisor status", true, err)
	}
	var response supervisor.RPCResponse
	if err := json.NewDecoder(io.LimitReader(conn, 64<<10)).Decode(&response); err != nil {
		return output.Wrap(output.CodeRemoteFailure, "decode supervisor status", true, err)
	}
	if !response.OK || response.Status == nil {
		return output.NewError(output.CodeRemoteFailure, "supervisor rejected status request", true)
	}
	status := *response.Status
	if err := renderer.Success(output.Success{Result: status, Lines: []output.Line{{Lead: "supervisor", Fields: []output.Field{{Name: "running", Value: status.Running}, {Name: "state", Value: status.Health.State}, {Name: "pending", Value: status.Health.PendingDeliveries}, {Name: "socket", Value: status.SocketPath}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) defaultSupervisorSocket() (string, *output.Error) {
	stateDir := ""
	if a.getenv != nil {
		stateDir = strings.TrimSpace(a.getenv("AGENTCTL_STATE_HOME"))
		if stateDir == "" {
			if xdg := strings.TrimSpace(a.getenv("XDG_STATE_HOME")); xdg != "" {
				stateDir = filepath.Join(xdg, "agentctl")
			}
		}
	}
	if stateDir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", output.Wrap(output.CodeDependencyUnavailable, "resolve supervisor state directory", false, err)
		}
		if home == "" {
			return "", output.NewError(output.CodeDependencyUnavailable, "resolve supervisor state directory", false)
		}
		stateDir = filepath.Join(home, ".local", "state", "agentctl")
	}
	absolute, err := filepath.Abs(stateDir)
	if err != nil {
		return "", output.Wrap(output.CodeInternal, "resolve supervisor state directory", false, err)
	}
	return filepath.Join(absolute, "supervisor.sock"), nil
}

func (a *app) supervisorPlan(renderer output.Renderer, args []string) *output.Error {
	platform, executable, stateDir := "", "", ""
	for i := 0; i < len(args); i++ {
		if i+1 >= len(args) {
			return output.NewError(output.CodeUsage, args[i]+" requires a value", false)
		}
		value := args[i+1]
		i++
		switch args[i-1] {
		case "--platform":
			platform = value
		case "--executable":
			executable = value
		case "--state-dir":
			stateDir = value
		default:
			return output.NewError(output.CodeUsage, "unknown supervisor plan flag", false).WithDetail("flag", args[i-1])
		}
	}
	if !filepath.IsAbs(executable) || !filepath.IsAbs(stateDir) {
		return output.NewError(output.CodeUsage, "supervisor plan requires absolute --executable and --state-dir", false)
	}
	service := supervisor.ServicePlan{Name: "io.agentctl.supervisor", Description: "agentctl host-local supervisor", Executable: executable, Arguments: []string{"supervisor", "run"}, StateDir: stateDir, SocketPath: filepath.Join(stateDir, "supervisor.sock")}
	var result any
	switch platform {
	case "darwin":
		home, err := os.UserHomeDir()
		if err != nil {
			return output.Wrap(output.CodeInternal, "resolve home", false, err)
		}
		plan, err := supervisor.BuildLaunchdInstallPlan(service, filepath.Join(home, "Library", "LaunchAgents"))
		if err != nil {
			return output.Wrap(output.CodeUsage, "build launchd plan", false, err)
		}
		result = plan
	case "linux":
		home, err := os.UserHomeDir()
		if err != nil {
			return output.Wrap(output.CodeInternal, "resolve home", false, err)
		}
		configHome := ""
		if a.getenv != nil {
			configHome = strings.TrimSpace(a.getenv("XDG_CONFIG_HOME"))
		}
		if configHome == "" {
			configHome = filepath.Join(home, ".config")
		} else if !filepath.IsAbs(configHome) {
			return output.NewError(output.CodeUsage, "XDG_CONFIG_HOME must be an absolute path", false)
		}
		plan, err := supervisor.BuildSystemdInstallPlan(service, filepath.Join(configHome, "systemd", "user"))
		if err != nil {
			return output.Wrap(output.CodeUsage, "build systemd plan", false, err)
		}
		result = plan
	default:
		return output.NewError(output.CodeUsage, "--platform must be darwin or linux", false)
	}
	if err := renderer.Success(output.Success{Result: result, Lines: []output.Line{{Lead: "supervisor.plan", Fields: []output.Field{{Name: "platform", Value: platform}, {Name: "side_effect_class", Value: output.ReadOnly}, {Name: "writes", Value: false}}}}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

type pathSupervisorExecutions struct{ path string }

func (b pathSupervisorExecutions) withBridge(fn func(agentruntime.SupervisorExecutions) error) error {
	journal, err := openJournalWithRetry(b.path, store.Options{})
	if err != nil {
		return err
	}
	defer journal.Close()
	engine, err := agentruntime.New(journal, agentruntime.Options{})
	if err != nil {
		return err
	}
	return fn(agentruntime.SupervisorExecutions{Engine: engine})
}

func (b pathSupervisorExecutions) ListNonTerminal(ctx context.Context) (result []supervisor.Execution, err error) {
	err = b.withBridge(func(bridge agentruntime.SupervisorExecutions) error {
		result, err = bridge.ListNonTerminal(ctx)
		return err
	})
	return result, err
}

func (b pathSupervisorExecutions) Reprobe(ctx context.Context, execution supervisor.Execution) (result supervisor.ProbeResult, err error) {
	err = b.withBridge(func(bridge agentruntime.SupervisorExecutions) error {
		result, err = bridge.Reprobe(ctx, execution)
		return err
	})
	return result, err
}

func (b pathSupervisorExecutions) ApplyProbe(ctx context.Context, id string, result supervisor.ProbeResult) error {
	return b.withBridge(func(bridge agentruntime.SupervisorExecutions) error {
		return bridge.ApplyProbe(ctx, id, result)
	})
}

type journalSupervisorOutbox struct{ path string }

func (b journalSupervisorOutbox) withJournal(fn func(*store.Journal) error) error {
	journal, err := openJournalWithRetry(b.path, store.Options{})
	if err != nil {
		return err
	}
	defer journal.Close()
	return fn(journal)
}

func (b journalSupervisorOutbox) ListPending(ctx context.Context) ([]agentruntime.OutboxRecord, error) {
	var items []store.DeliveryRecord
	err := b.withJournal(func(journal *store.Journal) error {
		var err error
		items, err = journal.ListPendingDeliveries(ctx)
		return err
	})
	if err != nil {
		return nil, err
	}
	result := make([]agentruntime.OutboxRecord, 0, len(items))
	for _, item := range items {
		result = append(result, agentruntime.OutboxRecord{ID: item.ID, ExecutionID: item.ExecutionID, SubscriptionID: item.SubscriptionID, Attempts: item.Attempts, AttemptInFlight: item.AttemptInFlight, NextAttemptAt: item.NextAttemptAt, ExpiresAt: item.ExpiresAt, Payload: append([]byte(nil), item.Payload...)})
	}
	return result, nil
}
func (b journalSupervisorOutbox) MarkAcknowledged(ctx context.Context, id string) error {
	return b.withJournal(func(journal *store.Journal) error { return journal.Ack(ctx, id) })
}
func (b journalSupervisorOutbox) ScheduleRetry(ctx context.Context, id string, next time.Time, reason string) error {
	return b.withJournal(func(journal *store.Journal) error { return journal.Retry(ctx, id, next, reason) })
}
func (b journalSupervisorOutbox) MarkDeadLetter(ctx context.Context, id, reason string) error {
	return b.withJournal(func(journal *store.Journal) error { return journal.DeadLetter(ctx, id, reason) })
}

func (b journalSupervisorOutbox) BeginAttempt(ctx context.Context, id string) (agentruntime.OutboxRecord, error) {
	var item store.DeliveryRecord
	err := b.withJournal(func(journal *store.Journal) error {
		var err error
		item, err = journal.BeginDeliveryAttempt(ctx, id)
		return err
	})
	if err != nil {
		return agentruntime.OutboxRecord{}, err
	}
	return agentruntime.OutboxRecord{ID: item.ID, ExecutionID: item.ExecutionID, SubscriptionID: item.SubscriptionID, Attempts: item.Attempts, AttemptInFlight: item.AttemptInFlight, NextAttemptAt: item.NextAttemptAt, ExpiresAt: item.ExpiresAt, Payload: append([]byte(nil), item.Payload...)}, nil
}

type callbackTransport struct {
	journalPath string
	stateDir    string
}

func (t callbackTransport) Deliver(ctx context.Context, record agentruntime.OutboxRecord) error {
	journal, err := openJournalWithRetry(t.journalPath, store.Options{ReadOnly: true})
	if err != nil {
		return &supervisor.RetryableDeliveryError{Err: err}
	}
	value, err := journal.GetSubscription(ctx, record.SubscriptionID)
	_ = journal.Close()
	if err != nil {
		return &supervisor.PermanentDeliveryError{Err: err}
	}
	var envelope callback.Envelope
	dec := json.NewDecoder(bytes.NewReader(record.Payload))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&envelope); err != nil {
		return &supervisor.PermanentDeliveryError{Err: err}
	}
	validationNow := time.Time{}
	if record.AttemptInFlight {
		validationNow = time.Now().UTC()
	}
	if err := envelope.Validate(validationNow); err != nil {
		return &supervisor.PermanentDeliveryError{Err: err}
	}
	if !record.AttemptInFlight {
		fresh, reissueErr := callback.ReissueEnvelope(envelope, record.Attempts+1, time.Now().UTC(), record.ExpiresAt)
		if reissueErr != nil {
			return &supervisor.PermanentDeliveryError{Err: reissueErr}
		}
		envelope = fresh
	}
	wirePayload, err := envelope.CanonicalBody()
	if err != nil {
		return &supervisor.PermanentDeliveryError{Err: err}
	}
	switch value.Destination.Kind {
	case callback.DestinationFile:
		if err := callback.AppendOwnerOnlyFile(value.Destination.Path, envelope); err != nil {
			return &supervisor.RetryableDeliveryError{Err: err}
		}
		return nil
	case callback.DestinationUnix:
		_, err := callback.SendUnixWithAcknowledgement(ctx, value.Destination.Path, wirePayload, envelope, time.Now().UTC())
		if err != nil {
			return &supervisor.RetryableDeliveryError{Err: err}
		}
		return nil
	case callback.DestinationCommand:
		plan, err := callback.PlanCommand(value.Destination)
		if err != nil {
			return &supervisor.PermanentDeliveryError{Err: err}
		}
		dir := filepath.Join(t.stateDir, "callback-events")
		path, err := callback.WriteOwnerOnlyEvent(dir, envelope)
		if err != nil {
			return &supervisor.RetryableDeliveryError{Err: err}
		}
		defer os.Remove(path)
		if err := callback.InvokeCommand(ctx, plan, path); err != nil {
			return &supervisor.RetryableDeliveryError{Err: err}
		}
		return nil
	case callback.DestinationWebhook:
		key, err := resolveCredential(value.Destination.CredentialRef)
		if err != nil {
			return &supervisor.PermanentDeliveryError{Err: err}
		}
		req, err := callback.BuildWebhookRequestWithAllowedHosts(envelope, value.Destination.Path, key, time.Now().UTC(), value.Destination.AllowedHosts)
		if err != nil {
			return &supervisor.PermanentDeliveryError{Err: err}
		}
		req = req.WithContext(ctx)
		client, err := callback.NewSafeWebhookClient(value.Destination.Path, value.Destination.AllowedHosts, 30*time.Second, nil)
		if err != nil {
			return &supervisor.PermanentDeliveryError{Err: err}
		}
		response, err := client.Do(req)
		if err != nil {
			return &supervisor.RetryableDeliveryError{Err: err}
		}
		defer response.Body.Close()
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 16<<10+1))
		if readErr != nil {
			return &supervisor.RetryableDeliveryError{Err: readErr}
		}
		if len(body) > 16<<10 {
			return &supervisor.PermanentDeliveryError{Err: errors.New("webhook acknowledgement exceeds limit")}
		}
		if _, err := callback.IsAcknowledged(response.StatusCode, body, envelope, time.Now().UTC()); err != nil {
			if callback.ClassifyDelivery(response.StatusCode, err) == callback.RetryTransient {
				return &supervisor.RetryableDeliveryError{Err: err}
			}
			return &supervisor.PermanentDeliveryError{Err: err}
		}
		return nil
	default:
		return &supervisor.PermanentDeliveryError{Err: fmt.Errorf("unsupported persistent destination %q", value.Destination.Kind)}
	}
}

func resolveCredential(ref string) ([]byte, error) {
	ref = strings.TrimSpace(ref)
	if strings.HasPrefix(ref, "env:") {
		value := os.Getenv(strings.TrimPrefix(ref, "env:"))
		if value == "" {
			return nil, errors.New("webhook credential environment reference is unavailable")
		}
		return []byte(value), nil
	}
	if strings.HasPrefix(ref, "file:") {
		path := strings.TrimPrefix(ref, "file:")
		if !filepath.IsAbs(path) {
			return nil, errors.New("credential file must be absolute")
		}
		info, err := os.Lstat(path)
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
			return nil, errors.New("credential file must be a regular owner-only file")
		}
		value, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		value = bytes.TrimSpace(value)
		if len(value) == 0 {
			return nil, errors.New("credential file is empty")
		}
		return value, nil
	}
	return nil, errors.New("webhook credential_ref must use env:NAME or file:/absolute/path")
}

var _ agentruntime.DurableOutbox = journalSupervisorOutbox{}
var _ agentruntime.DeliveryTransport = callbackTransport{}
