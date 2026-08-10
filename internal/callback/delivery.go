package callback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

var (
	ErrInvalidDestination = errors.New("invalid callback destination")
	ErrRedirectRefused    = errors.New("webhook redirect refused")
	ErrReplay             = errors.New("webhook replay rejected")
	ErrInvalidAck         = errors.New("invalid callback acknowledgement")
	ErrExecutableChanged  = errors.New("callback executable changed since plan")
	ErrInvalidDelivery    = errors.New("invalid delivery record")
	ErrInvalidUnixSocket  = errors.New("invalid Unix callback socket")
	ErrUnixUnacknowledged = errors.New("Unix callback send has no acknowledgement")
)

// DestinationKind is one of the built-in, shell-free callback transports.
type DestinationKind string

const (
	DestinationParent  DestinationKind = "parent"
	DestinationStdout  DestinationKind = "stdout"
	DestinationFile    DestinationKind = "file"
	DestinationUnix    DestinationKind = "unix"
	DestinationWebhook DestinationKind = "webhook"
	DestinationCommand DestinationKind = "command"
)

// Destination is a storage-neutral callback target. CredentialRef is an
// opaque local reference and is never emitted in request URLs, argv, or logs.
type Destination struct {
	Kind            DestinationKind  `json:"kind"`
	Path            string           `json:"path,omitempty"` // file/unix path, webhook URL, or executable path
	Args            []string         `json:"args,omitempty"` // command arguments; event file path is appended
	CredentialRef   string           `json:"credential_ref,omitempty"`
	AllowedHosts    []string         `json:"allowed_hosts,omitempty"` // optional exact webhook host allowlist
	CommandIdentity *CommandIdentity `json:"command_identity,omitempty"`
}

// CommandIdentity is captured when a command subscription is persisted and
// compared again immediately before invocation. It prevents a path swap from
// silently retargeting a durable callback.
type CommandIdentity struct {
	Device uint64      `json:"device"`
	Inode  uint64      `json:"inode"`
	Mode   os.FileMode `json:"mode"`
}

func cloneDestination(d Destination) Destination {
	d.Args = append([]string(nil), d.Args...)
	d.AllowedHosts = append([]string(nil), d.AllowedHosts...)
	if d.CommandIdentity != nil {
		identity := *d.CommandIdentity
		d.CommandIdentity = &identity
	}
	return d
}

func (d Destination) Validate() error {
	switch d.Kind {
	case DestinationParent, DestinationStdout:
		if d.Path != "" || len(d.Args) != 0 {
			return fmt.Errorf("%w: %s does not accept path or args", ErrInvalidDestination, d.Kind)
		}
	case DestinationFile, DestinationUnix, DestinationCommand:
		if d.Path == "" || filepath.IsAbs(d.Path) && filepath.Clean(d.Path) == string(filepath.Separator) {
			return fmt.Errorf("%w: path is required", ErrInvalidDestination)
		}
		if d.Kind != DestinationCommand && len(d.Args) > 0 {
			return fmt.Errorf("%w: args only valid for command", ErrInvalidDestination)
		}
		if d.Kind != DestinationCommand && d.CredentialRef != "" {
			return fmt.Errorf("%w: credentials not valid for local destination", ErrInvalidDestination)
		}
		if d.Kind == DestinationUnix {
			if err := ValidateUnixSocket(d.Path); err != nil {
				return err
			}
		}
	case DestinationWebhook:
		if err := ValidateWebhookURL(d.Path, d.AllowedHosts); err != nil {
			return err
		}
	default:
		return fmt.Errorf("%w: unknown kind %q", ErrInvalidDestination, d.Kind)
	}
	return nil
}

// WriteOwnerOnlyEvent atomically writes one JSON event document and returns
// its path. The file and its containing directory are owner-only.
func WriteOwnerOnlyEvent(dir string, event any) (string, error) {
	if dir == "" {
		return "", fmt.Errorf("event directory is required")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	if err := ensureNoSymlink(dir); err != nil {
		return "", err
	}
	if err := ensureOwnerOnlyDir(dir); err != nil {
		return "", err
	}
	b, err := CanonicalJSON(event)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp(dir, ".event-*")
	if err != nil {
		return "", err
	}
	path := f.Name()
	defer func() {
		_ = f.Close()
		if err != nil {
			_ = os.Remove(path)
		}
	}()
	if err = f.Chmod(0o600); err == nil {
		_, err = f.Write(append(b, '\n'))
	}
	if err == nil {
		err = f.Sync()
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

// AppendOwnerOnlyFile appends one NDJSON event, creating the file securely.
func AppendOwnerOnlyFile(path string, event any) error {
	if path == "" || filepath.Clean(path) != path {
		return fmt.Errorf("invalid event file path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := ensureNoSymlink(filepath.Dir(path)); err != nil {
		return err
	}
	if err := ensureOwnerOnlyDir(filepath.Dir(path)); err != nil {
		return err
	}
	b, err := CanonicalJSON(event)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := f.Chmod(0o600); err != nil {
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		return err
	}
	return f.Sync()
}

// WriteStdout emits one complete NDJSON callback document. It accepts an
// io.Writer so a CLI can pass os.Stdout while tests and embedders use a buffer.
func WriteStdout(w io.Writer, event any) error {
	if w == nil {
		return errors.New("stdout writer is nil")
	}
	b, err := CanonicalJSON(event)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	for len(b) > 0 {
		n, writeErr := w.Write(b)
		if writeErr != nil {
			return writeErr
		}
		if n <= 0 || n > len(b) {
			return io.ErrShortWrite
		}
		b = b[n:]
	}
	return nil
}

func ensureOwnerOnlyDir(path string) error {
	if err := ensureNoSymlink(path); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("state directory %q must be owner-only", path)
	}
	return nil
}

func ensureNoSymlink(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	root := volume + string(filepath.Separator)
	remainder := strings.TrimPrefix(abs, root)
	current := root
	if remainder == "" {
		return nil
	}
	for _, part := range strings.Split(remainder, string(filepath.Separator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		st, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path %q contains a symlink", path)
		}
	}
	return nil
}

// CommandPlan captures executable identity during --plan. Invocation rejects
// replacement or symlink retargeting before starting the process.
type CommandPlan struct {
	Destination Destination
	Device      uint64
	Inode       uint64
	Mode        os.FileMode
	Path        string
}

func PlanCommand(d Destination) (CommandPlan, error) {
	if d.Kind != DestinationCommand || d.Path == "" {
		return CommandPlan{}, fmt.Errorf("%w: command destination required", ErrInvalidDestination)
	}
	if err := d.Validate(); err != nil {
		return CommandPlan{}, err
	}
	st, err := os.Stat(d.Path)
	if err != nil {
		return CommandPlan{}, err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return CommandPlan{}, fmt.Errorf("%w: executable is not runnable", ErrInvalidDestination)
	}
	dev, ino := statIdentity(st)
	mode := st.Mode()
	if d.CommandIdentity != nil && (d.CommandIdentity.Device != dev || d.CommandIdentity.Inode != ino || d.CommandIdentity.Mode != mode) {
		return CommandPlan{}, ErrExecutableChanged
	}
	return CommandPlan{Destination: cloneDestination(d), Device: dev, Inode: ino, Mode: mode, Path: d.Path}, nil
}

func (p CommandPlan) Identity() CommandIdentity {
	return CommandIdentity{Device: p.Device, Inode: p.Inode, Mode: p.Mode}
}

func statIdentity(st os.FileInfo) (uint64, uint64) {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return uint64(sys.Dev), uint64(sys.Ino)
	}
	return 0, uint64(st.ModTime().UnixNano()) ^ uint64(st.Size())
}

// InvokeCommand passes the owner-only event path as one argv element and
// never invokes a shell. The environment is intentionally minimal.
func InvokeCommand(ctx context.Context, plan CommandPlan, eventPath string) error {
	if eventPath == "" {
		return errors.New("event path is required")
	}
	st, err := os.Stat(plan.Path)
	if err != nil {
		return err
	}
	dev, ino := statIdentity(st)
	if dev != plan.Device || ino != plan.Inode || st.Mode() != plan.Mode {
		return ErrExecutableChanged
	}
	argv := append([]string(nil), plan.Destination.Args...)
	argv = append(argv, eventPath)
	cmd := exec.CommandContext(ctx, plan.Path, argv...)
	cmd.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	cmd.Stdin = nil
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run()
}

// ValidateUnixSocket checks the socket itself and its parent before any
// connection is attempted. Existing components must be owner-owned, owner
// only, and free of symlink components; a missing socket is not deliverable.
func ValidateUnixSocket(path string) error {
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.IndexByte(path, 0) >= 0 {
		return fmt.Errorf("%w: socket path must be absolute and canonical", ErrInvalidUnixSocket)
	}
	if err := ensureNoSymlink(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: parent path: %v", ErrInvalidUnixSocket, err)
	}
	if err := validateOwnerOnlyDir(filepath.Dir(path)); err != nil {
		return fmt.Errorf("%w: parent directory: %v", ErrInvalidUnixSocket, err)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("%w: socket is not present: %v", ErrInvalidUnixSocket, err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%w: path is not a Unix socket", ErrInvalidUnixSocket)
	}
	if err := validateOwnerOnlyFile(st); err != nil {
		return fmt.Errorf("%w: socket permissions: %v", ErrInvalidUnixSocket, err)
	}
	return nil
}

func validateOwnerOnlyDir(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.IsDir() {
		return errors.New("parent is not a directory")
	}
	if st.Mode().Perm()&0o077 != 0 {
		return errors.New("directory is not owner-only")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && uint64(sys.Uid) != uint64(os.Getuid()) {
		return errors.New("directory is not owned by current user")
	}
	return nil
}

func validateOwnerOnlyFile(st os.FileInfo) error {
	if st.Mode().Perm()&0o077 != 0 {
		return errors.New("socket is not owner-only")
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); ok && uint64(sys.Uid) != uint64(os.Getuid()) {
		return errors.New("socket is not owned by current user")
	}
	return nil
}

// SendUnixUnacknowledged writes bytes to a validated local Unix stream. A
// successful return means only that the local kernel accepted the bytes; it
// is not a destination acknowledgement and must not advance a delivery
// cursor.
func SendUnixUnacknowledged(ctx context.Context, path string, body []byte) error {
	if path == "" {
		return fmt.Errorf("unix socket path is required")
	}
	if len(body) == 0 || len(body) > maxWebhookBody {
		return fmt.Errorf("Unix callback body exceeds size limit")
	}
	if err := ValidateUnixSocket(path); err != nil {
		return err
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return err
	}
	defer conn.Close()
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	_, err = conn.Write(body)
	return err
}

// SendUnix is retained as a compatibility alias; it intentionally has
// unacknowledged semantics. Use SendUnixWithAcknowledgement when delivery
// proof is required.
func SendUnix(ctx context.Context, path string, body []byte) error {
	return SendUnixUnacknowledged(ctx, path, body)
}

// SendUnixWithAcknowledgement sends a bounded document and waits for the
// strict callback acknowledgement JSON used by webhook receivers. The
// acknowledgement is bounded and validated against the same envelope.
func SendUnixWithAcknowledgement(ctx context.Context, path string, body []byte, envelope Envelope, now time.Time) (Acknowledgement, error) {
	if len(body) == 0 || len(body) > maxWebhookBody {
		return Acknowledgement{}, fmt.Errorf("Unix callback body exceeds size limit")
	}
	if err := envelope.Validate(now); err != nil {
		return Acknowledgement{}, err
	}
	if err := ValidateUnixSocket(path); err != nil {
		return Acknowledgement{}, err
	}
	d := net.Dialer{}
	conn, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return Acknowledgement{}, err
	}
	defer conn.Close()
	deadline := time.Now().Add(10 * time.Second)
	if until, ok := ctx.Deadline(); ok && until.Before(deadline) {
		deadline = until
	}
	_ = conn.SetDeadline(deadline)
	if _, err := conn.Write(body); err != nil {
		return Acknowledgement{}, err
	}
	ackBody, err := io.ReadAll(io.LimitReader(conn, maxAckBody+1))
	if err != nil {
		return Acknowledgement{}, err
	}
	if len(ackBody) > maxAckBody {
		return Acknowledgement{}, ErrInvalidAck
	}
	return ValidateAcknowledgement(ackBody, envelope, now)
}

// ClassifyUnixSend keeps an unacknowledged write in the retry path. Only the
// explicit acknowledged form can produce RetryAcknowledged.
func ClassifyUnixSend(err error, acknowledged bool) RetryClass {
	if err == nil && acknowledged {
		return RetryAcknowledged
	}
	if err != nil {
		return RetryTransient
	}
	return RetryTransient
}

// RetryClass describes whether an outbox item should be retried, paused, or
// moved to dead-letter storage.
type RetryClass string

const (
	RetryAcknowledged RetryClass = "acknowledged"
	RetryTransient    RetryClass = "retry"
	RetryPaused       RetryClass = "paused"
	RetryDeadLetter   RetryClass = "dead_letter"
)

func ClassifyHTTP(status int, err error) RetryClass {
	if err == nil && status >= 200 && status <= 299 {
		return RetryAcknowledged
	}
	if err != nil || status == 408 || status == 429 || status >= 500 {
		return RetryTransient
	}
	if status == 401 || status == 403 {
		return RetryPaused
	}
	return RetryDeadLetter
}

// ClassifyDelivery includes local transport errors for which retrying is
// unsafe. In particular a refused redirect is a dead-letter destination, not
// a transient connection failure.
func ClassifyDelivery(status int, err error) RetryClass {
	if errors.Is(err, ErrRedirectRefused) {
		return RetryDeadLetter
	}
	return ClassifyHTTP(status, err)
}

// RetryPolicy bounds attempts and delay. Jitter is supplied by the caller so
// tests and durable schedulers can use deterministic randomness.
type RetryPolicy struct {
	MaxAttempts  int
	InitialDelay time.Duration
	MaxDelay     time.Duration
	Jitter       time.Duration
}

func (p RetryPolicy) Normalize() RetryPolicy {
	if p.MaxAttempts <= 0 {
		p.MaxAttempts = 8
	}
	if p.InitialDelay <= 0 {
		p.InitialDelay = time.Second
	}
	if p.MaxDelay <= 0 || p.MaxDelay < p.InitialDelay {
		p.MaxDelay = 5 * time.Minute
	}
	if p.Jitter < 0 {
		p.Jitter = 0
	}
	return p
}

func (p RetryPolicy) ShouldRetry(attempt int) bool {
	p = p.Normalize()
	return attempt >= 0 && attempt < p.MaxAttempts
}

func (p RetryPolicy) Delay(attempt int, random *rand.Rand) time.Duration {
	p = p.Normalize()
	if attempt < 1 {
		attempt = 1
	}
	d := p.InitialDelay
	for i := 1; i < attempt && d < p.MaxDelay; i++ {
		d *= 2
		if d > p.MaxDelay || d < 0 {
			d = p.MaxDelay
		}
	}
	if d > p.MaxDelay {
		d = p.MaxDelay
	}
	if p.Jitter > 0 {
		if random == nil {
			random = rand.New(rand.NewSource(1))
		}
		d += time.Duration(random.Int63n(int64(p.Jitter) + 1))
		if d > p.MaxDelay {
			d = p.MaxDelay
		}
	}
	return d
}

// OutboxEntry is the durable, storage-neutral delivery record. A restart
// resumes this same DeliveryID rather than creating a new event or attempt.
type OutboxEntry struct {
	DeliveryID     string    `json:"delivery_id"`
	SubscriptionID string    `json:"subscription_id"`
	EventID        string    `json:"event_id"`
	EventDedupeKey string    `json:"event_dedupe_key"`
	Attempt        int       `json:"attempt"`
	NextAttemptAt  time.Time `json:"next_attempt_at"`
	State          string    `json:"state"`
	LastError      string    `json:"last_error,omitempty"`
}

const (
	DeliveryPending    = "pending"
	DeliveryAcked      = "acknowledged"
	DeliveryRetry      = "retry"
	DeliveryPaused     = "paused"
	DeliveryDeadLetter = "dead_letter"
)

// RecordResult applies one delivery result without changing the event or
// delivery ID. The caller persists this value atomically with its receipt.
func RecordResult(entry OutboxEntry, class RetryClass, attempt int, at time.Time, err error, policy RetryPolicy) (OutboxEntry, error) {
	if entry.DeliveryID == "" || entry.SubscriptionID == "" || entry.EventID == "" || entry.EventDedupeKey == "" || attempt < 1 {
		return OutboxEntry{}, ErrInvalidDelivery
	}
	entry.Attempt = attempt
	entry.LastError = ""
	switch class {
	case RetryAcknowledged:
		entry.State = DeliveryAcked
		entry.NextAttemptAt = time.Time{}
	case RetryPaused:
		entry.State = DeliveryPaused
		entry.NextAttemptAt = time.Time{}
	case RetryDeadLetter:
		entry.State = DeliveryDeadLetter
		entry.NextAttemptAt = time.Time{}
	case RetryTransient:
		entry.LastError = redactDeliveryError(err)
		if !policy.ShouldRetry(attempt) {
			entry.State = DeliveryDeadLetter
			entry.NextAttemptAt = time.Time{}
		} else {
			entry.State = DeliveryRetry
			entry.NextAttemptAt = at.Add(policy.Delay(attempt, nil))
		}
	default:
		return OutboxEntry{}, ErrInvalidDelivery
	}
	return entry, nil
}

func redactDeliveryError(err error) string {
	if err == nil {
		return ""
	}
	// Keep durable errors bounded and avoid persisting arbitrary response bodies
	// (which may contain credentials or native output).
	s := err.Error()
	if len(s) > 256 {
		s = s[:256]
	}
	return s
}
