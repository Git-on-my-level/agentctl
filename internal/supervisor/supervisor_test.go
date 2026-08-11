package supervisor

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

type fakeExecutions struct {
	mu       sync.Mutex
	items    []Execution
	probes   []string
	results  map[string]ProbeResult
	listErr  error
	applyErr map[string]error
}

func (f *fakeExecutions) ListNonTerminal(context.Context) ([]Execution, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]Execution(nil), f.items...), nil
}

func (f *fakeExecutions) ApplyProbe(_ context.Context, id string, result ProbeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, id)
	if err := f.applyErr[id]; err != nil {
		return err
	}
	f.results[id] = result
	return nil
}

type fakeReprober struct {
	mu      sync.Mutex
	calls   []string
	results map[string]ProbeResult
	errors  map[string]error
}

type cancelingExecutions struct {
	cancel context.CancelFunc
}

func (f cancelingExecutions) ListNonTerminal(ctx context.Context) ([]Execution, error) {
	f.cancel()
	return nil, ctx.Err()
}

func (cancelingExecutions) ApplyProbe(context.Context, string, ProbeResult) error { return nil }

type cycleFactory struct {
	mu       sync.Mutex
	opens    int
	closes   int
	deps     Dependencies
	closeErr error
}

func (f *cycleFactory) Open(context.Context) (CycleDependencies, error) {
	f.mu.Lock()
	f.opens++
	f.mu.Unlock()
	return CycleDependencies{Dependencies: f.deps, Close: func() error {
		f.mu.Lock()
		f.closes++
		f.mu.Unlock()
		return f.closeErr
	}}, nil
}

func (f *fakeReprober) Reprobe(_ context.Context, execution Execution) (ProbeResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, execution.ID)
	if err := f.errors[execution.ID]; err != nil {
		return ProbeResult{}, err
	}
	return f.results[execution.ID], nil
}

type fakeOutbox struct {
	mu         sync.Mutex
	entries    map[string]Delivery
	acks       []string
	retries    []retryCall
	dead       []deadCall
	pendingErr error
}

type retryCall struct {
	id     string
	next   time.Time
	reason string
}

type deadCall struct {
	id     string
	reason string
}

func (f *fakeOutbox) Pending(context.Context) ([]Delivery, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pendingErr != nil {
		return nil, f.pendingErr
	}
	items := make([]Delivery, 0, len(f.entries))
	for _, item := range f.entries {
		items = append(items, item)
	}
	return items, nil
}

func (f *fakeOutbox) Ack(_ context.Context, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.acks = append(f.acks, id)
	delete(f.entries, id)
	return nil
}

func (f *fakeOutbox) Retry(_ context.Context, id string, next time.Time, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	entry := f.entries[id]
	entry.Attempts++
	entry.NextAttemptAt = next
	f.entries[id] = entry
	f.retries = append(f.retries, retryCall{id: id, next: next, reason: reason})
	return nil
}

func (f *fakeOutbox) DeadLetter(_ context.Context, id, reason string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dead = append(f.dead, deadCall{id: id, reason: reason})
	delete(f.entries, id)
	return nil
}

type fakeDeliverer struct {
	mu     sync.Mutex
	calls  []string
	errors map[string][]error
}

func (f *fakeDeliverer) Deliver(_ context.Context, entry Delivery) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, entry.ID)
	queue := f.errors[entry.ID]
	if len(queue) == 0 {
		return nil
	}
	err := queue[0]
	f.errors[entry.ID] = queue[1:]
	return err
}

func testConfig(t *testing.T, clock Clock) Config {
	t.Helper()
	root := t.TempDir()
	cfg := DefaultConfig()
	cfg.StateDir = root
	cfg.SocketPath = filepath.Join(root, "supervisor.sock")
	cfg.StatePath = filepath.Join(root, "state.json")
	cfg.Clock = clock
	cfg.CycleInterval = time.Hour
	cfg.RetryBase = 2 * time.Second
	cfg.RetryMax = 10 * time.Second
	return cfg
}

func TestRunOnceRecoversDeterministically(t *testing.T) {
	clock := &fakeClock{now: time.Date(2026, 8, 10, 18, 0, 0, 0, time.UTC)}
	executions := &fakeExecutions{
		items: []Execution{{ID: "z", State: "running"}, {ID: "a", State: "waiting"}},
		results: map[string]ProbeResult{
			"a": {State: "completed", Liveness: "exited"},
			"z": {State: "running", Liveness: "alive"},
		},
		applyErr: map[string]error{},
	}
	reprober := &fakeReprober{results: executions.results, errors: map[string]error{}}
	s, err := New(testConfig(t, clock), Dependencies{Executions: executions, Reprober: reprober})
	if err != nil {
		t.Fatal(err)
	}
	report, err := s.RunOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Recovery.Examined != 2 || report.Recovery.Applied != 2 {
		t.Fatalf("unexpected recovery report: %+v", report.Recovery)
	}
	if want, got := []string{"a", "z"}, reprober.calls; !reflect.DeepEqual(want, got) {
		t.Fatalf("reprobe order = %v, want %v", got, want)
	}
	if got := s.Health().State; got != HealthStopped {
		t.Fatalf("health = %s, want stopped for daemonless RunOnce", got)
	}
}

func TestRunOnceFactoryClosesBoundedDependencies(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	executions := &fakeExecutions{items: []Execution{{ID: "a", State: "running"}}, results: map[string]ProbeResult{"a": {State: "completed"}}, applyErr: map[string]error{}}
	reprober := &fakeReprober{results: executions.results, errors: map[string]error{}}
	factory := &cycleFactory{deps: Dependencies{Executions: executions, Reprober: reprober}}
	cfg := testConfig(t, clock)
	s, err := New(cfg, Dependencies{Factory: factory})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RunOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	factory.mu.Lock()
	opens, closes := factory.opens, factory.closes
	factory.mu.Unlock()
	if opens != 2 || closes != 2 {
		t.Fatalf("factory opens=%d closes=%d, want one close per cycle", opens, closes)
	}
}

func TestRecoveryContinuesAfterOneProbeFailure(t *testing.T) {
	clock := &fakeClock{now: time.Unix(10, 0)}
	executions := &fakeExecutions{
		items:    []Execution{{ID: "a", State: "running"}, {ID: "b", State: "running"}},
		results:  map[string]ProbeResult{"b": {State: "completed"}},
		applyErr: map[string]error{},
	}
	reprober := &fakeReprober{
		results: executions.results,
		errors:  map[string]error{"a": errors.New("backend unavailable")},
	}
	s, err := New(testConfig(t, clock), Dependencies{Executions: executions, Reprober: reprober})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if len(report.Recovery.Failures) != 1 || report.Recovery.Applied != 1 {
		t.Fatalf("unexpected recovery report: %+v", report.Recovery)
	}
	if got := s.Health().State; got != HealthDegraded {
		t.Fatalf("health = %s, want degraded", got)
	}
}

func TestDrainOutboxBoundedRetryAndDeadLetter(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &fakeOutbox{entries: map[string]Delivery{
		"a": {ID: "a"},
		"b": {ID: "b", Attempts: 1},
	}}
	deliverer := &fakeDeliverer{errors: map[string][]error{
		"a": {&RetryableDeliveryError{Err: errors.New("timeout")}},
		"b": {&PermanentDeliveryError{Err: errors.New("forbidden")}},
	}}
	cfg := testConfig(t, clock)
	cfg.MaxDeliveryAttempts = 3
	s, err := New(cfg, Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Delivery.Retried != 1 || report.Delivery.DeadLettered != 1 {
		t.Fatalf("unexpected delivery report: %+v", report.Delivery)
	}
	if len(outbox.retries) != 1 || outbox.retries[0].id != "a" || outbox.retries[0].next != clock.now.Add(2*time.Second) {
		t.Fatalf("unexpected retry calls: %+v", outbox.retries)
	}
	if len(outbox.dead) != 1 || outbox.dead[0].id != "b" {
		t.Fatalf("unexpected dead letters: %+v", outbox.dead)
	}
	if !reflect.DeepEqual([]string{"a", "b"}, deliverer.calls) {
		t.Fatalf("delivery order = %v", deliverer.calls)
	}
}

func TestDeliveryRetryLimitIsBounded(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &fakeOutbox{entries: map[string]Delivery{"a": {ID: "a", Attempts: 2}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{"a": {errors.New("must not deliver")}}}
	cfg := testConfig(t, clock)
	cfg.MaxDeliveryAttempts = 2
	s, err := New(cfg, Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if report.Delivery.DeadLettered != 1 || len(deliverer.calls) != 0 {
		t.Fatalf("retry limit was not enforced: %+v calls=%v", report.Delivery, deliverer.calls)
	}
}

func TestStartSocketOwnerOnlyAndGracefulShutdown(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	cfg := testConfig(t, clock)
	if err := os.Chmod(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = s.Shutdown(context.Background()) }()

	stateInfo, err := os.Stat(cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if mode := stateInfo.Mode().Perm(); mode != 0700 {
		t.Fatalf("state dir mode = %o, want 700", mode)
	}
	socketInfo, err := os.Stat(cfg.SocketPath)
	if err != nil {
		t.Fatal(err)
	}
	if mode := socketInfo.Mode().Perm(); mode != 0600 {
		t.Fatalf("socket mode = %o, want 600", mode)
	}
	response, err := Request(context.Background(), cfg.SocketPath, "health")
	if err != nil || !response.OK || response.Health == nil {
		t.Fatalf("health request = %+v, err %v", response, err)
	}
	if err := s.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cfg.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket still exists after shutdown: %v", err)
	}
	if got := s.Health().State; got != HealthStopped {
		t.Fatalf("health after shutdown = %s", got)
	}
}

func TestRestartReprobesNonterminalAndDrainsDurableOutbox(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, clock)
	cfg.StateDir = root
	cfg.SocketPath = filepath.Join(root, "supervisor.sock")
	cfg.StatePath = filepath.Join(root, "state.json")
	executions := &fakeExecutions{
		items:    []Execution{{ID: "exec-a", State: "running"}},
		results:  map[string]ProbeResult{"exec-a": {State: "running", Liveness: "alive"}},
		applyErr: map[string]error{},
	}
	reprober := &fakeReprober{results: executions.results, errors: map[string]error{}}
	outbox := &fakeOutbox{entries: map[string]Delivery{"delivery-a": {ID: "delivery-a"}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{}}
	deps := Dependencies{Executions: executions, Reprober: reprober, Outbox: outbox, Deliverer: deliverer}
	first, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := first.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reprober.calls) != 1 || len(deliverer.calls) != 1 {
		t.Fatalf("first lifecycle probes=%v deliveries=%v", reprober.calls, deliverer.calls)
	}
	// Re-add a durable entry to model an acknowledgement-window crash: the
	// next process must use the same delivery ID rather than minting a new one.
	outbox.mu.Lock()
	outbox.entries["delivery-b"] = Delivery{ID: "delivery-b"}
	outbox.mu.Unlock()
	second, err := New(cfg, deps)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := second.Shutdown(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(reprober.calls) != 2 || !reflect.DeepEqual(deliverer.calls, []string{"delivery-a", "delivery-b"}) {
		t.Fatalf("restart recovery probes=%v deliveries=%v", reprober.calls, deliverer.calls)
	}
}

func TestTailnetHookIsExplicitAndAuthenticated(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	s, err := New(testConfig(t, clock), Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.TailnetHandler(TailnetHookConfig{}); !errors.Is(err, ErrTailnetHookDisabled) {
		t.Fatalf("disabled hook err = %v", err)
	}
	h, err := s.TailnetHandler(TailnetHookConfig{Enabled: true, Authenticator: BearerTokenAuthenticator{Token: "secret"}})
	if err != nil {
		t.Fatal(err)
	}
	unauthenticated := httptest.NewRecorder()
	h.ServeHTTP(unauthenticated, httptest.NewRequest(http.MethodGet, "/health", nil))
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated status = %d", unauthenticated.Code)
	}
	authenticated := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/status", nil)
	req.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(authenticated, req)
	if authenticated.Code != http.StatusOK || !strings.Contains(authenticated.Body.String(), `"running"`) {
		t.Fatalf("authenticated response = %d %s", authenticated.Code, authenticated.Body.String())
	}
	post := httptest.NewRecorder()
	postReq := httptest.NewRequest(http.MethodPost, "/status", nil)
	postReq.Header.Set("Authorization", "Bearer secret")
	h.ServeHTTP(post, postReq)
	if post.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status = %d", post.Code)
	}
}

func TestServicePlansArePureAndDeterministic(t *testing.T) {
	service := ServicePlan{
		Name:        "agentctl-supervisor",
		Description: "Managed supervisor",
		Executable:  "/opt/agentctl",
		SocketPath:  "/tmp/agentctl.sock",
		StateDir:    "/tmp/agentctl",
		Environment: map[string]string{"Z": "last", "A": "first"},
	}
	launchd, err := BuildLaunchdInstallPlan(service, "/tmp/LaunchAgents")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(launchd.Contents), "agentctl-supervisor") || launchd.Path != "/tmp/LaunchAgents/agentctl-supervisor.plist" {
		t.Fatalf("launchd plan = %+v", launchd)
	}
	if strings.Contains(string(launchd.Contents), "<true></true>") || strings.Count(string(launchd.Contents), "<true/>") != 2 {
		t.Fatalf("launchd booleans must use canonical empty elements: %s", launchd.Contents)
	}
	systemd, err := BuildSystemdInstallPlan(service, "/tmp/systemd")
	if err != nil {
		t.Fatal(err)
	}
	text := string(systemd.Contents)
	if strings.Index(text, "Environment=A=") > strings.Index(text, "Environment=Z=") {
		t.Fatalf("environment order is not deterministic: %s", text)
	}
	if _, err := os.Stat(systemd.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("plan unexpectedly wrote host file: %v", err)
	}
}

func TestBackoffClamps(t *testing.T) {
	for attempt, want := range map[int]time.Duration{1: time.Second, 2: 2 * time.Second, 3: 4 * time.Second, 10: 8 * time.Second} {
		if got := backoff(time.Second, 8*time.Second, attempt); got != want {
			t.Fatalf("backoff(%d) = %s, want %s", attempt, got, want)
		}
	}
}

func TestRecoveryListFailureIsDegraded(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	executions := &fakeExecutions{listErr: errors.New("journal unavailable"), results: map[string]ProbeResult{}}
	s, err := New(testConfig(t, clock), Dependencies{Executions: executions, Reprober: &fakeReprober{}})
	if err != nil {
		t.Fatal(err)
	}
	report, _ := s.RunOnce(context.Background())
	if len(report.Recovery.Failures) != 1 || s.Health().State != HealthDegraded {
		t.Fatalf("report=%+v health=%+v", report, s.Health())
	}
}

func TestSocketPathRejectsRegularFile(t *testing.T) {
	root := t.TempDir()
	socketPath := filepath.Join(root, "supervisor.sock")
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(socketPath, []byte("sentinel"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := testConfig(t, &fakeClock{now: time.Unix(1, 0)})
	cfg.SocketPath = socketPath
	if err := os.Chmod(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	s, err := New(cfg, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrSocketExists) {
		t.Fatalf("start err = %v, want ErrSocketExists", err)
	}
}

func TestStartCancellationDuringRecoveryDoesNotDeadlock(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	cfg := testConfig(t, clock)
	if err := os.Chmod(cfg.StateDir, 0700); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	s, err := New(cfg, Dependencies{Executions: cancelingExecutions{cancel: cancel}, Reprober: &fakeReprober{}})
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- s.Start(ctx) }()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Start did not return after startup recovery cancellation")
	}
	if _, err := os.Stat(cfg.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("startup socket remains after cancellation: %v", err)
	}
	if got := s.Health().State; got != HealthStopped {
		t.Fatalf("health after cancelled startup = %s", got)
	}
}

func TestRunOnceReturnsImmediateContextCancellation(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	s, err := New(testConfig(t, clock), Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RunOnce(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("RunOnce error = %v, want context.Canceled", err)
	}
}

func TestStateAndSocketSymlinkComponentsRejectedBeforeCreation(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target")
	if err := os.Mkdir(target, 0700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(root, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.StateDir = filepath.Join(link, "state")
	cfg.SocketPath = filepath.Join(root, "supervisor.sock")
	cfg.StatePath = filepath.Join(cfg.StateDir, "state.json")
	cfg.Clock = &fakeClock{now: time.Unix(100, 0)}
	s, err := New(cfg, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("symlink state start error = %v, want ErrSymlinkPath", err)
	}
	if _, err := os.Stat(filepath.Join(target, "state")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state was created through symlink: %v", err)
	}
	if _, err := os.Lstat(cfg.SocketPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("socket was created despite symlink state path: %v", err)
	}

	// A safe state root must still reject a symlinked StatePath parent before
	// ensureOwnerDir can create anything below it.
	safe := filepath.Join(root, "safe")
	if err := os.Mkdir(safe, 0700); err != nil {
		t.Fatal(err)
	}
	stateLink := filepath.Join(root, "state-link")
	if err := os.Symlink(target, stateLink); err != nil {
		t.Fatal(err)
	}
	cfg.StateDir = safe
	cfg.SocketPath = filepath.Join(safe, "supervisor.sock")
	cfg.StatePath = filepath.Join(stateLink, "state.json")
	s, err = New(cfg, Dependencies{})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Start(context.Background()); !errors.Is(err, ErrSymlinkPath) {
		t.Fatalf("symlink StatePath start error = %v, want ErrSymlinkPath", err)
	}
}

func TestOutboxPendingOrderingIndependentOfMapOrder(t *testing.T) {
	clock := &fakeClock{now: time.Unix(100, 0)}
	outbox := &fakeOutbox{entries: map[string]Delivery{"c": {ID: "c"}, "a": {ID: "a"}, "b": {ID: "b"}}}
	deliverer := &fakeDeliverer{errors: map[string][]error{}}
	s, err := New(testConfig(t, clock), Dependencies{Outbox: outbox, Deliverer: deliverer})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = s.RunOnce(context.Background())
	want := []string{"a", "b", "c"}
	got := append([]string(nil), deliverer.calls...)
	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("delivery IDs = %v, want %v", deliverer.calls, want)
	}
}
