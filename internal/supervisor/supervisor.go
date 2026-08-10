package supervisor

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervisor is a managed host-local supervisor.  It owns only process
// lifecycle, recovery scheduling, and bounded callback draining; authority
// state remains in injected adapters.
type Supervisor struct {
	mu     sync.RWMutex
	cycle  sync.Mutex
	cfg    Config
	deps   Dependencies
	health Health

	running        bool
	stopping       bool
	workersStarted bool
	ln             net.Listener
	stop           chan struct{}
	done           chan struct{}
	shutdownDone   chan struct{}
	startupCancel  context.CancelFunc
	wg             sync.WaitGroup
}

// New validates and normalizes configuration but does not create directories,
// sockets, files, listeners, or host services.  Those effects begin only at
// Start, which is itself explicit.
func New(cfg Config, deps Dependencies) (*Supervisor, error) {
	cfg = normalizeConfig(cfg)
	if cfg.CycleInterval <= 0 {
		return nil, fmt.Errorf("cycle interval must be positive")
	}
	if cfg.MaxDeliveryAttempts <= 0 {
		return nil, fmt.Errorf("max delivery attempts must be positive")
	}
	if cfg.MaxDeliveriesPerCycle <= 0 {
		return nil, fmt.Errorf("max deliveries per cycle must be positive")
	}
	if cfg.RetryBase <= 0 || cfg.RetryMax <= 0 || cfg.RetryMax < cfg.RetryBase {
		return nil, fmt.Errorf("retry bounds are invalid")
	}
	if cfg.ShutdownTimeout <= 0 {
		return nil, fmt.Errorf("shutdown timeout must be positive")
	}
	if cfg.Clock == nil {
		cfg.Clock = RealClock{}
	}
	return &Supervisor{
		cfg:    cfg,
		deps:   deps,
		health: Health{State: HealthStopped},
	}, nil
}

func normalizeConfig(cfg Config) Config {
	defaults := DefaultConfig()
	if cfg.CycleInterval == 0 {
		cfg.CycleInterval = defaults.CycleInterval
	}
	if cfg.MaxDeliveryAttempts == 0 {
		cfg.MaxDeliveryAttempts = defaults.MaxDeliveryAttempts
	}
	if cfg.MaxDeliveriesPerCycle == 0 {
		cfg.MaxDeliveriesPerCycle = defaults.MaxDeliveriesPerCycle
	}
	if cfg.RetryBase == 0 {
		cfg.RetryBase = defaults.RetryBase
	}
	if cfg.RetryMax == 0 {
		cfg.RetryMax = defaults.RetryMax
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = defaults.ShutdownTimeout
	}
	if cfg.Clock == nil {
		cfg.Clock = defaults.Clock
	}

	if cfg.StateDir == "" {
		switch {
		case cfg.SocketPath != "":
			cfg.StateDir = filepath.Dir(cfg.SocketPath)
		case os.Getenv("AGENTCTL_STATE_HOME") != "":
			cfg.StateDir = os.Getenv("AGENTCTL_STATE_HOME")
		case os.Getenv("XDG_STATE_HOME") != "":
			cfg.StateDir = filepath.Join(os.Getenv("XDG_STATE_HOME"), "agentctl")
		default:
			home, err := os.UserHomeDir()
			if err == nil && home != "" {
				cfg.StateDir = filepath.Join(home, ".local", "state", "agentctl")
			} else {
				cfg.StateDir = filepath.Join(os.TempDir(), "agentctl")
			}
		}
	}
	if cfg.SocketPath == "" {
		cfg.SocketPath = filepath.Join(cfg.StateDir, "supervisor.sock")
	}
	if cfg.StatePath == "" {
		cfg.StatePath = filepath.Join(cfg.StateDir, "supervisor-state.json")
	}
	if p, err := filepath.Abs(cfg.StateDir); err == nil {
		cfg.StateDir = p
	}
	if p, err := filepath.Abs(cfg.SocketPath); err == nil {
		cfg.SocketPath = p
	}
	if p, err := filepath.Abs(cfg.StatePath); err == nil {
		cfg.StatePath = p
	}
	return cfg
}

// Config returns a copy of the normalized configuration.
func (s *Supervisor) Config() Config {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// Start performs one recovery pass synchronously, then starts the Unix socket
// and periodic cycle loop.  It does not start Tailnet HTTP; that interface is
// exposed only through ServeTailnetHTTP with an explicit enabled config.
func (s *Supervisor) Start(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if s.running || s.stopping {
		s.mu.Unlock()
		return ErrAlreadyRunning
	}
	s.health = Health{State: HealthStarting, StartedAt: s.cfg.Clock.Now()}
	s.mu.Unlock()

	if err := ctx.Err(); err != nil {
		s.setFailed(err)
		return err
	}
	if err := ensureOwnerDir(s.cfg.StateDir); err != nil {
		s.setFailed(err)
		return err
	}
	if err := ensureOwnerStateFile(s.cfg.StatePath); err != nil {
		s.setFailed(err)
		return err
	}
	ln, err := listenOwnerSocket(s.cfg.SocketPath)
	if err != nil {
		s.setFailed(err)
		return err
	}

	runCtx, startupCancel := context.WithCancel(ctx)
	s.mu.Lock()
	s.ln = ln
	s.stop = make(chan struct{})
	s.done = make(chan struct{})
	s.shutdownDone = make(chan struct{})
	s.startupCancel = startupCancel
	s.workersStarted = false
	s.running = true
	s.health.State = HealthStarting
	s.mu.Unlock()

	// Recovery is deliberately before serving requests so a freshly started
	// supervisor never reports healthy while stale executions remain unprobed.
	_, runErr := s.RunOnce(runCtx)
	startupCancel()
	if runErr != nil {
		s.abortStartup(runErr)
		return runErr
	}
	if err := ctx.Err(); err != nil {
		s.abortStartup(err)
		return err
	}

	s.mu.Lock()
	if !s.running || s.stopping {
		s.mu.Unlock()
		return ErrNotRunning
	}
	s.wg.Add(2)
	s.workersStarted = true
	s.startupCancel = nil
	s.mu.Unlock()
	go s.acceptLoop()
	go s.cycleLoop()
	return nil
}

// Run starts the service and blocks until ctx is cancelled, then performs a
// graceful shutdown.  It is convenient for a host-manager entrypoint while
// keeping host installation outside this package.
func (s *Supervisor) Run(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.Start(ctx); err != nil {
		return err
	}
	<-ctx.Done()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
	defer cancel()
	return s.Shutdown(shutdownCtx)
}

func (s *Supervisor) cycleLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.cfg.CycleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			_, _ = s.RunOnce(context.Background())
		case <-s.stop:
			return
		}
	}
}

func (s *Supervisor) acceptLoop() {
	defer s.wg.Done()
	defer close(s.done)
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			select {
			case <-s.stop:
				return
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Temporary() {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			return
		}
		s.wg.Add(1)
		go func() {
			defer s.wg.Done()
			s.serveConn(conn)
		}()
	}
}

// RPCRequest is the bounded local Unix-socket request document.
type RPCRequest struct {
	Op string `json:"op"`
}

// RPCResponse is the bounded local Unix-socket response document.
type RPCResponse struct {
	OK      bool    `json:"ok"`
	Error   string  `json:"error,omitempty"`
	Health  *Health `json:"health,omitempty"`
	Status  *Status `json:"status,omitempty"`
	Version int     `json:"schema_version"`
}

func (s *Supervisor) serveConn(conn net.Conn) {
	defer conn.Close()
	// A request is bounded and one JSON document per connection.  This avoids
	// turning the local socket into an unbounded transcript or command channel.
	dec := json.NewDecoder(io.LimitReader(conn, 64<<10))
	var req RPCRequest
	if err := dec.Decode(&req); err != nil {
		_ = json.NewEncoder(conn).Encode(RPCResponse{Version: 1, Error: "invalid request"})
		return
	}
	resp := RPCResponse{Version: 1}
	switch req.Op {
	case "health":
		h := s.Health()
		resp.OK, resp.Health = true, &h
	case "status":
		st := s.Status()
		resp.OK, resp.Status = true, &st
	case "shutdown":
		resp.OK = true
		// Do not wait for the accept loop from its own connection goroutine.
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), s.cfg.ShutdownTimeout)
			defer cancel()
			_ = s.Shutdown(ctx)
		}()
	default:
		resp.Error = "unknown operation"
	}
	_ = json.NewEncoder(conn).Encode(resp)
}

// Shutdown stops accepting requests, stops the cycle loop, and removes only
// the socket created by this instance.  State files/directories are retained.
func (s *Supervisor) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if !s.running && !s.stopping {
		s.mu.Unlock()
		return ErrNotRunning
	}
	if s.stopping {
		shutdownDone := s.shutdownDone
		s.mu.Unlock()
		select {
		case <-shutdownDone:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	s.running = false
	s.stopping = true
	s.health.State = HealthStopping
	ln, stop, done := s.ln, s.stop, s.done
	shutdownDone := s.shutdownDone
	workersStarted := s.workersStarted
	startupCancel := s.startupCancel
	s.mu.Unlock()

	if startupCancel != nil {
		startupCancel()
	}
	close(stop)
	_ = ln.Close()
	if !workersStarted {
		// Start has not launched accept/cycle workers yet, so no worker can
		// close done.  Closing it here makes cancellation deterministic and
		// lets finishShutdown complete without waiting forever.
		close(done)
	}
	go s.finishShutdown(done, shutdownDone)

	select {
	case <-done:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-shutdownDone:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Supervisor) finishShutdown(done, shutdownDone chan struct{}) {
	<-done
	s.wg.Wait()
	_ = removeOwnedSocket(s.cfg.SocketPath)
	s.mu.Lock()
	s.health.State = HealthStopped
	s.ln = nil
	s.stopping = false
	s.workersStarted = false
	s.startupCancel = nil
	s.mu.Unlock()
	close(shutdownDone)
}

// abortStartup synchronously tears down a listener created before startup
// recovery completed.  Normal Shutdown relies on worker-owned done closure;
// pre-worker cancellation has no such worker and must close done itself.
func (s *Supervisor) abortStartup(err error) {
	s.mu.Lock()
	if !s.running {
		s.mu.Unlock()
		return
	}
	s.running = false
	s.stopping = true
	s.health.State = HealthStopping
	s.health.LastError = boundedError(err)
	ln, stop, done := s.ln, s.stop, s.done
	shutdownDone := s.shutdownDone
	s.ln = nil
	s.mu.Unlock()
	close(stop)
	_ = ln.Close()
	close(done)
	_ = removeOwnedSocket(s.cfg.SocketPath)
	s.mu.Lock()
	s.health.State = HealthStopped
	s.stopping = false
	s.workersStarted = false
	s.startupCancel = nil
	s.mu.Unlock()
	close(shutdownDone)
}

// RunOnce performs recovery then one bounded outbox drain.  It is safe to
// call while the service is stopped, which allows a daemonless CLI invocation
// to reuse exactly the managed recovery behavior.
func (s *Supervisor) RunOnce(ctx context.Context) (report CycleReport, runErr error) {
	if ctx == nil {
		ctx = context.Background()
	}
	s.cycle.Lock()
	defer s.cycle.Unlock()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	deps := s.deps
	var closeCycle func() error
	if s.deps.Factory != nil {
		cycle, err := s.deps.Factory.Open(ctx)
		if err != nil {
			s.recordError(err)
			return report, err
		}
		deps = cycle.Dependencies
		closeCycle = cycle.Close
	}
	if closeCycle != nil {
		defer func() {
			if err := closeCycle(); err != nil {
				s.recordError(err)
				if runErr == nil {
					runErr = err
				}
			}
		}()
	}
	recovery := s.recoverWith(ctx, deps)
	delivery := s.drainOutboxWith(ctx, deps)
	report.Recovery, report.Delivery = recovery, delivery
	s.mu.Lock()
	now := s.cfg.Clock.Now()
	s.health.LastCycleAt = now
	s.health.LastRecoveryAt = now
	s.health.LastOutboxAt = now
	s.health.LastCycleReprobeErrors = len(recovery.Failures)
	s.health.LastCycleDeliveryErrors = len(delivery.Failures)
	if len(recovery.Failures) > 0 || len(delivery.Failures) > 0 {
		s.health.State = HealthDegraded
	} else if s.health.State == HealthStarting || s.health.State == HealthDegraded {
		s.health.State = HealthHealthy
	}
	s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return report, err
	}
	return report, nil
}

func (s *Supervisor) recover(ctx context.Context) RecoveryReport {
	return s.recoverWith(ctx, s.deps)
}

func (s *Supervisor) recoverWith(ctx context.Context, deps Dependencies) RecoveryReport {
	var report RecoveryReport
	if deps.Executions == nil || deps.Reprober == nil {
		return report
	}
	executions, err := deps.Executions.ListNonTerminal(ctx)
	if err != nil {
		report.Failures = append(report.Failures, RecoveryFailure{Error: boundedError(err)})
		s.recordError(err)
		return report
	}
	// Defensive filtering keeps a buggy adapter from probing an already
	// terminal execution and accidentally reopening it.
	nonTerminal := executions[:0]
	for _, execution := range executions {
		if !execution.IsTerminal() {
			nonTerminal = append(nonTerminal, execution)
		}
	}
	sort.Slice(nonTerminal, func(i, j int) bool { return nonTerminal[i].ID < nonTerminal[j].ID })
	report.Examined = len(nonTerminal)
	for _, execution := range nonTerminal {
		if err := ctx.Err(); err != nil {
			report.Failures = append(report.Failures, RecoveryFailure{ExecutionID: execution.ID, Error: boundedError(err)})
			break
		}
		result, err := deps.Reprober.Reprobe(ctx, execution)
		if err != nil {
			report.Failures = append(report.Failures, RecoveryFailure{ExecutionID: execution.ID, Error: boundedError(err)})
			s.recordError(err)
			continue
		}
		report.Probed++
		if result.ObservedAt.IsZero() {
			result.ObservedAt = s.cfg.Clock.Now()
		}
		if err := deps.Executions.ApplyProbe(ctx, execution.ID, result); err != nil {
			report.Failures = append(report.Failures, RecoveryFailure{ExecutionID: execution.ID, Error: boundedError(err)})
			s.recordError(err)
			continue
		}
		report.Applied++
	}
	s.mu.Lock()
	s.health.NonTerminalExecutions = report.Examined
	s.mu.Unlock()
	return report
}

func (s *Supervisor) drainOutbox(ctx context.Context) DeliveryReport {
	return s.drainOutboxWith(ctx, s.deps)
}

func (s *Supervisor) drainOutboxWith(ctx context.Context, deps Dependencies) DeliveryReport {
	var report DeliveryReport
	if deps.Outbox == nil || deps.Deliverer == nil {
		return report
	}
	entries, err := deps.Outbox.Pending(ctx)
	if err != nil {
		report.Failures = append(report.Failures, DeliveryFailure{Action: "pending", Error: boundedError(err)})
		s.recordError(err)
		return report
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].ID < entries[j].ID })
	report.Examined = len(entries)
	now := s.cfg.Clock.Now()
	for _, entry := range entries {
		if report.Attempted >= s.cfg.MaxDeliveriesPerCycle {
			report.Skipped++
			continue
		}
		if !entry.ExpiresAt.IsZero() && !now.Before(entry.ExpiresAt) {
			report.DeadLettered++
			if err := deps.Outbox.DeadLetter(ctx, entry.ID, "expired"); err != nil {
				report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: entry.Attempts, Action: "dead_letter", Error: boundedError(err)})
				s.recordError(err)
			}
			continue
		}
		if !entry.NextAttemptAt.IsZero() && entry.NextAttemptAt.After(now) {
			report.Skipped++
			continue
		}
		if entry.Attempts >= s.cfg.MaxDeliveryAttempts && !entry.AttemptInFlight {
			report.DeadLettered++
			if err := deps.Outbox.DeadLetter(ctx, entry.ID, "retry limit exceeded"); err != nil {
				report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: entry.Attempts, Action: "dead_letter", Error: boundedError(err)})
				s.recordError(err)
			}
			continue
		}
		attempt := entry.Attempts + 1
		beganAttempt := false
		if starter, ok := deps.Outbox.(AttemptStarter); ok {
			started, beginErr := starter.BeginAttempt(ctx, entry.ID)
			if beginErr != nil {
				if errors.Is(beginErr, ErrAttemptStartUnsupported) {
					// Legacy/custom outboxes remain usable. Their transport must
					// mint fresh wire metadata itself, but cannot persist the
					// crash boundary provided by AttemptStarter.
					beganAttempt = false
				} else {
					report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: entry.Attempts + 1, Action: "begin_attempt", Error: boundedError(beginErr)})
					s.recordError(beginErr)
					continue
				}
			} else {
				entry = started
				attempt = entry.Attempts
				beganAttempt = true
				if entry.Attempts > s.cfg.MaxDeliveryAttempts {
					report.DeadLettered++
					if deadErr := deps.Outbox.DeadLetter(ctx, entry.ID, "retry limit exceeded"); deadErr != nil {
						report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: entry.Attempts, Action: "dead_letter", Error: boundedError(deadErr)})
						s.recordError(deadErr)
					}
					continue
				}
			}
		}
		report.Attempted++
		err := deps.Deliverer.Deliver(ctx, entry)
		if err == nil {
			report.Delivered++
			if ackErr := deps.Outbox.Ack(ctx, entry.ID); ackErr != nil {
				report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: attempt, Action: "ack", Error: boundedError(ackErr)})
				s.recordError(ackErr)
			}
			continue
		}
		if !beganAttempt {
			attempt = entry.Attempts + 1
		}
		retryable := classifyDeliveryError(err)
		if !retryable || attempt >= s.cfg.MaxDeliveryAttempts {
			report.DeadLettered++
			reason := boundedError(err)
			if !retryable {
				reason = "permanent: " + reason
			}
			if dlErr := deps.Outbox.DeadLetter(ctx, entry.ID, reason); dlErr != nil {
				report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: attempt, Action: "dead_letter", Error: boundedError(dlErr)})
				s.recordError(dlErr)
			}
			continue
		}
		report.Retried++
		next := now.Add(backoff(s.cfg.RetryBase, s.cfg.RetryMax, attempt))
		if retryErr := deps.Outbox.Retry(ctx, entry.ID, next, boundedError(err)); retryErr != nil {
			report.Failures = append(report.Failures, DeliveryFailure{DeliveryID: entry.ID, Attempt: attempt, Action: "retry", Error: boundedError(retryErr)})
			s.recordError(retryErr)
		}
	}
	s.mu.Lock()
	s.health.PendingDeliveries = report.Examined - report.Delivered - report.DeadLettered
	if s.health.PendingDeliveries < 0 {
		s.health.PendingDeliveries = 0
	}
	s.mu.Unlock()
	return report
}

func classifyDeliveryError(err error) bool {
	var de *DeliveryError
	if errors.As(err, &de) {
		return de.Retryable
	}
	var pe *PermanentDeliveryError
	if errors.As(err, &pe) {
		return false
	}
	var re *RetryableDeliveryError
	if errors.As(err, &re) {
		return true
	}
	// Unknown errors are temporary by default: callback transports commonly
	// wrap connection failures.  Context cancellation remains non-retryable
	// for this cycle and will be retried by the next supervisor invocation.
	return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
}

func backoff(base, max time.Duration, attempt int) time.Duration {
	if attempt <= 1 {
		return base
	}
	d := base
	for i := 1; i < attempt; i++ {
		if d >= max/2 || d > max {
			return max
		}
		d *= 2
	}
	if d > max {
		return max
	}
	return d
}

func (s *Supervisor) recordError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	s.health.LastError = boundedError(err)
	s.health.State = HealthDegraded
	s.mu.Unlock()
}

func (s *Supervisor) setFailed(err error) {
	s.mu.Lock()
	s.health.State = HealthFailed
	s.health.LastError = boundedError(err)
	s.mu.Unlock()
}

// Health returns a copy suitable for JSON encoding.
func (s *Supervisor) Health() Health {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.health
}

// Status returns a bounded read-only projection and performs no repair or
// refresh work.
func (s *Supervisor) Status() Status {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return Status{
		Running:    s.running,
		SocketPath: s.cfg.SocketPath,
		StateDir:   s.cfg.StateDir,
		Health:     s.health,
		Now:        s.cfg.Clock.Now(),
	}
}

// BearerTokenAuthenticator is a small opt-in authenticator for a Tailnet
// hook.  Tokens should be loaded from an owner-only credential source by the
// caller, never put in URLs, argv, or supervisor state.
type BearerTokenAuthenticator struct{ Token string }

func (a BearerTokenAuthenticator) Authenticate(r *http.Request) error {
	if r == nil {
		return errors.New("nil request")
	}
	const prefix = "Bearer "
	got := r.Header.Get("Authorization")
	if !strings.HasPrefix(got, prefix) || a.Token == "" {
		return errors.New("invalid bearer token")
	}
	provided := strings.TrimSpace(strings.TrimPrefix(got, prefix))
	if subtle.ConstantTimeCompare([]byte(provided), []byte(a.Token)) != 1 {
		return errors.New("invalid bearer token")
	}
	return nil
}

// TailnetHandler returns an authenticated, read-only HTTP handler only when
// explicitly enabled.  Supported routes are GET /health and GET /status.
func (s *Supervisor) TailnetHandler(cfg TailnetHookConfig) (http.Handler, error) {
	if !cfg.Enabled {
		return nil, ErrTailnetHookDisabled
	}
	if cfg.Authenticator == nil {
		return nil, ErrTailnetAuthenticatorRequired
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := cfg.Authenticator.Authenticate(r); err != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		var value any
		switch r.URL.Path {
		case "/health", "/healthz":
			value = s.Health()
		case "/status":
			value = s.Status()
		default:
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(value)
	}), nil
}

// ServeTailnetHTTP serves the opt-in handler on a caller-provided listener.
// Passing a listener makes transport setup testable and prevents this method
// from guessing Tailnet interfaces or mutating host networking.
func (s *Supervisor) ServeTailnetHTTP(ctx context.Context, ln net.Listener, cfg TailnetHookConfig) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if ln == nil {
		return errors.New("Tailnet HTTP listener is nil")
	}
	h, err := s.TailnetHandler(cfg)
	if err != nil {
		return err
	}
	server := &http.Server{Handler: h}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = server.Shutdown(context.Background())
		case <-done:
		}
	}()
	err = server.Serve(ln)
	close(done)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

func boundedError(err error) string {
	if err == nil {
		return ""
	}
	text := strings.Join(strings.Fields(err.Error()), " ")
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func ensureOwnerDir(path string) error {
	if path == "" {
		return errors.New("state directory is empty")
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("state path is not a directory: %s", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: %s mode %o", ErrInsecurePermissions, path, info.Mode().Perm())
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s is not owned by the current user", ErrInsecurePermissions, path)
	}
	return nil
}

func ensureOwnerStateFile(path string) error {
	if path == "" {
		return nil
	}
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	if err := ensureOwnerDir(filepath.Dir(path)); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("state path is not a regular file: %s", path)
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("%w: %s mode %o", ErrInsecurePermissions, path, info.Mode().Perm())
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: %s is not owned by the current user", ErrInsecurePermissions, path)
	}
	return nil
}

func listenOwnerSocket(path string) (net.Listener, error) {
	if err := rejectSymlinkComponents(path); err != nil {
		return nil, err
	}
	if err := ensureOwnerDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%w: %s", ErrSocketExists, path)
		}
		if info.Mode().Perm()&0077 != 0 {
			return nil, fmt.Errorf("%w: %s mode %o", ErrInsecurePermissions, path, info.Mode().Perm())
		}
		if !ownedByCurrentUser(info) {
			return nil, fmt.Errorf("%w: socket %s is not owned by the current user", ErrInsecurePermissions, path)
		}
		if err := os.Remove(path); err != nil {
			return nil, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0600); err != nil {
		_ = ln.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return ln, nil
}

func removeOwnedSocket(path string) error {
	if err := rejectSymlinkComponents(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 {
		return nil
	}
	if !ownedByCurrentUser(info) {
		return fmt.Errorf("%w: socket %s is not owned by the current user", ErrInsecurePermissions, path)
	}
	return os.Remove(path)
}

// rejectSymlinkComponents inspects every existing component with Lstat before
// any operation can follow a link. Once a component is absent, later
// components cannot already exist beneath it, so the walk can stop safely.
func rejectSymlinkComponents(path string) error {
	if path == "" {
		return errors.New("path is empty")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = filepath.Clean(abs)
	volume := filepath.VolumeName(abs)
	remainder := strings.TrimPrefix(abs, volume)
	root := volume + string(filepath.Separator)
	if strings.HasPrefix(remainder, string(filepath.Separator)) {
		remainder = strings.TrimPrefix(remainder, string(filepath.Separator))
	} else {
		root = volume
	}
	current := root
	for _, component := range strings.Split(remainder, string(filepath.Separator)) {
		if component == "" || component == "." {
			continue
		}
		current = filepath.Join(current, component)
		info, err := os.Lstat(current)
		if errors.Is(err, os.ErrNotExist) {
			break
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("%w: %s", ErrSymlinkPath, current)
		}
	}
	return nil
}

func ownedByCurrentUser(info os.FileInfo) bool {
	if info == nil {
		return false
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false
	}
	return uint64(stat.Uid) == uint64(os.Geteuid())
}

// Request sends one local JSON-line request to a running supervisor.  It is a
// deliberately tiny helper for parent CLI integration and is not used by the
// managed service itself.
func Request(ctx context.Context, socketPath, op string) (RPCResponse, error) {
	var response RPCResponse
	if ctx == nil {
		ctx = context.Background()
	}
	dialer := net.Dialer{}
	conn, err := dialer.DialContext(ctx, "unix", socketPath)
	if err != nil {
		return response, err
	}
	defer conn.Close()
	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}
	if err := json.NewEncoder(conn).Encode(RPCRequest{Op: op}); err != nil {
		return response, err
	}
	if err := json.NewDecoder(bufio.NewReader(io.LimitReader(conn, 64<<10))).Decode(&response); err != nil {
		return response, err
	}
	if !response.OK {
		if response.Error == "" {
			response.Error = "supervisor request failed"
		}
		return response, errors.New(response.Error)
	}
	return response, nil
}
