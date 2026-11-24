package admission

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

const lockBudget = time.Second
const PollInterval = 250 * time.Millisecond

var ErrHolderOwned = errors.New("admission holder belongs to another process")
var ErrLockBusy = errors.New("admission bucket lock is busy")

type Owner struct {
	PID   int    `json:"pid"`
	Start string `json:"process_start"`
}

// Start identifies a kernel process generation. An empty value with no error
// proves the process is gone. An error means identity could not be checked.
type ProcessStart func(pid int) (string, error)

type Store struct {
	dir   string
	now   func() time.Time
	start ProcessStart
}

func New(journalPath string, now func() time.Time, start ProcessStart) *Store {
	return &Store{dir: filepath.Join(filepath.Dir(journalPath), "admission"), now: now, start: start}
}

type Ticket struct {
	Holder string `json:"holder"`
	Owner
	Priority   string     `json:"priority"`
	QueuedAt   time.Time  `json:"queued_at"`
	AcquiredAt *time.Time `json:"acquired_at,omitempty"`
	External   bool       `json:"external,omitempty"`
	Sequence   uint64     `json:"sequence"`
	// A pending external claim keeps the invoker's flock and separately
	// records the intended long-lived owner for same-holder replays.
	ClaimOwner *Owner `json:"claim_owner,omitempty"`
	// Only used when a previously verified external owner cannot be inspected.
	VerifiedAt *time.Time `json:"verified_at,omitempty"`
}

type Decision struct {
	Bucket     string      `json:"bucket"`
	Priority   string      `json:"priority"`
	Granted    bool        `json:"granted"`
	Position   int         `json:"position,omitempty"`
	Window     int         `json:"window"`
	Reason     string      `json:"reason"`
	AcquiredAt *time.Time  `json:"acquired_at,omitempty"`
	Adjustment *Adjustment `json:"-"`
	Warning    string      `json:"-"`
}

type Snapshot struct {
	Bucket            string         `json:"bucket"`
	Enabled           bool           `json:"enabled"`
	Window            int            `json:"window"`
	InUse             int            `json:"in_use"`
	Queued            map[string]int `json:"queued"`
	Holders           []Ticket       `json:"holders"`
	ThrottleRate      float64        `json:"throttle_rate"`
	Attempts          int64          `json:"attempts"`
	GoodputRPM        float64        `json:"goodput_rpm"`
	Admitted          int64          `json:"admitted"`
	TotalAttempts     int64          `json:"total_attempts"`
	ThrottledAttempts int64          `json:"throttled_attempts"`
	LastAdjustment    *Adjustment    `json:"last_adjustment,omitempty"`
	Warning           string         `json:"-"`
}

type Queue struct {
	store            *Store
	name             string
	bucket           Bucket
	holder, priority string
	waiter, owner    Owner
	file             *os.File
	external         bool
	retain           bool
	lockTimedOut     bool
}

func (s *Store) Queue(name string, b Bucket, holder, priority string, waiter, owner Owner, external bool) *Queue {
	return &Queue{store: s, name: name, bucket: b, holder: holder, priority: priority, waiter: waiter, owner: owner, external: external}
}

func ticketKey(holder string) string { return fmt.Sprintf("%x", sha256.Sum256([]byte(holder))) }

func (q *Queue) Try(ctx context.Context, enqueue bool) (Decision, error) {
	d := Decision{Bucket: q.name, Priority: q.priority, Reason: "at_capacity"}
	if !q.bucket.Active() {
		d.Granted, d.Reason = true, "disabled"
		return d, nil
	}
	lock, dir, err := q.store.lock(ctx, q.name, true)
	if err != nil {
		q.lockTimedOut = q.lockTimedOut || errors.Is(err, ErrLockBusy)
		return d, err
	}
	defer lock.Close()
	now := q.store.now().UTC()
	tickets, err := q.store.tickets(dir, now, true, q.file, ticketKey(q.holder))
	if err != nil {
		return d, err
	}
	state, warning := readController(dir, now, q.bucket)
	d.Warning = warning
	inUse := 0
	var sequence uint64
	var own *Ticket
	position := 1
	for i := range tickets {
		ticket := &tickets[i]
		sequence = max(sequence, ticket.Sequence)
		if ticket.AcquiredAt != nil {
			inUse++
		}
		if ticket.Holder == q.holder {
			own = ticket
		}
	}
	previous := *state
	d.Adjustment = state.tick(now, q.bucket, inUse)
	d.Window = state.Window
	if q.lockTimedOut {
		q.lockTimedOut = false
		d.Window = min(d.Window, q.bucket.fallback())
		if d.Warning == "" {
			d.Warning = "admission_lock_timeout"
		}
	}
	if own != nil && own.External && own.AcquiredAt != nil {
		if !q.external || own.Owner != q.owner {
			return d, ErrHolderOwned
		}
		own.VerifiedAt = &now
		if err := writeJSON(dir, ticketKey(q.holder)+".json", own); err != nil {
			return d, err
		}
		d.Granted, d.AcquiredAt, d.Reason = true, own.AcquiredAt, "reused"
		d.Adjustment = nil
		return d, nil
	}
	reusedWaiter := own != nil && q.file == nil
	if reusedWaiter && (!q.external || own.ClaimOwner == nil || *own.ClaimOwner != q.owner) {
		return d, ErrHolderOwned
	}
	var next Ticket
	if own != nil {
		next = *own
	} else {
		next = Ticket{Holder: q.holder, Owner: q.waiter, Priority: q.priority, QueuedAt: now, Sequence: sequence + 1}
		if q.external {
			next.ClaimOwner = &q.owner
		}
	}
	for _, other := range tickets {
		if other.AcquiredAt != nil || other.Holder == q.holder {
			continue
		}
		if other.Priority == next.Priority && other.Sequence < next.Sequence || other.Priority == Interactive && next.Priority == Background {
			position++
		}
	}
	d.Position = position
	if reusedWaiter {
		d.Adjustment, d.Reason, d.Priority = nil, "already_queued", next.Priority
		return d, nil
	}
	granted := position == 1 && inUse < d.Window
	if q.file == nil && (granted || enqueue) {
		q.file, err = openOwned(filepath.Join(dir, ticketKey(q.holder)+".lock"), true)
		if err != nil {
			return d, err
		}
		if err := flock(q.file, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			q.file.Close()
			q.file = nil
			return d, err
		}
	}
	if granted {
		if q.external {
			start, err := q.store.start(q.owner.PID)
			if err != nil || start == "" || start != q.owner.Start {
				return d, ErrHolderOwned
			}
			next.Owner, next.External, next.VerifiedAt, next.ClaimOwner = q.owner, true, &now, nil
		}
		next.AcquiredAt = &now
		state.Admitted++
		state.tick(now, q.bucket, inUse+1)
	}
	if warning != "" || previous.Window != state.Window || previous.Admitted != state.Admitted ||
		previous.LastTick != state.LastTick || previous.LastFull != state.LastFull {
		if warning != "" {
			// Use fallback for this decision, then start the repaired controller
			// at the configured initial window on the next transaction.
			state.Window = q.bucket.InitialWindow
		}
		state.expire(now, q.bucket)
		if err := writeJSON(dir, "controller.json", state); err != nil {
			// Ticket accounting is independent of controller storage.
			d.Window, d.Adjustment, d.Warning = min(d.Window, q.bucket.fallback()), nil, "admission_state_write_failed"
			if granted && inUse >= d.Window {
				next.AcquiredAt, next.External, next.VerifiedAt, next.Owner = nil, false, nil, q.waiter
				if q.external {
					next.ClaimOwner = &q.owner
				}
				granted = false
				if !enqueue {
					return d, nil
				}
			}
		}
	}
	if !granted && !enqueue {
		return d, nil
	}
	if own == nil || granted {
		if err := writeJSON(dir, ticketKey(q.holder)+".json", &next); err != nil {
			return d, err
		}
	}
	if granted {
		d.Granted, d.Position, d.Reason, d.AcquiredAt = true, 0, "slot", &now
		q.retain = q.external
	}
	return d, nil
}

func (q *Queue) Close() {
	if q == nil || q.file == nil {
		return
	}
	if q.retain {
		q.file.Close()
		q.file = nil
		return
	}
	// Remove while still holding the ticket flock and bucket lock. If the
	// bucket is unavailable, closing the flock makes the record reclaimable.
	lock, dir, err := q.store.lock(context.Background(), q.name, false)
	if err == nil && lock != nil {
		_ = os.Remove(filepath.Join(dir, ticketKey(q.holder)+".json"))
		_ = os.Remove(filepath.Join(dir, ticketKey(q.holder)+".lock"))
		lock.Close()
	}
	q.file.Close()
	q.file = nil
}

func (s *Store) Release(ctx context.Context, name, holder string) (bool, error) {
	lock, dir, err := s.lock(ctx, name, false)
	if err != nil || lock == nil {
		return false, err
	}
	defer lock.Close()
	base := filepath.Join(dir, ticketKey(holder))
	var ticket Ticket
	if err := readJSON(base+".json", &ticket, 4096); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	if !ticket.External {
		if ticket.ClaimOwner != nil {
			return false, nil
		}
		return false, ErrHolderOwned
	}
	if err := os.Remove(base + ".json"); err != nil {
		return false, err
	}
	_ = os.Remove(base + ".lock")
	return true, nil
}

func (b Bucket) ValidSample(now time.Time, sample Sample) bool {
	return sample.Attempts >= 1 && sample.Attempts <= 1_000_000_000 &&
		sample.Throttled >= 0 && sample.Throttled <= sample.Attempts &&
		!sample.At.After(now) && sample.At.After(now.Add(-b.lookback()))
}

func (s *Store) Observe(ctx context.Context, name string, b Bucket, sample Sample) (*Adjustment, string, error) {
	if !b.Active() {
		return nil, "", nil
	}
	now := s.now().UTC()
	if !b.ValidSample(now, sample) {
		return nil, "", errors.New("observation must be within the sample window with 1 to 1000000000 attempts and throttled between zero and attempts")
	}
	lock, dir, err := s.lock(ctx, name, true)
	if err != nil {
		return nil, "", err
	}
	defer lock.Close()
	now = s.now().UTC()
	tickets, err := s.tickets(dir, now, true, nil, "")
	if err != nil {
		return nil, "", err
	}
	state, warning := readController(dir, now, b)
	if warning != "" {
		state.Window = b.InitialWindow
	}
	state.observe(sample)
	state.expire(now, b)
	inUse := 0
	for _, ticket := range tickets {
		if ticket.AcquiredAt != nil {
			inUse++
		}
	}
	adjustment := state.tick(now, b, inUse)
	if err := writeJSON(dir, "controller.json", state); err != nil {
		return nil, warning, err
	}
	return adjustment, warning, nil
}

func (s *Store) Status(ctx context.Context, name string, b Bucket) (Snapshot, error) {
	status := Snapshot{Bucket: name, Enabled: b.Active(), Window: b.fallback(), Queued: map[string]int{Interactive: 0, Background: 0}, Holders: []Ticket{}}
	lock, dir, err := s.lock(ctx, name, false)
	if err != nil {
		return status, err
	}
	if lock != nil {
		defer lock.Close()
	}
	now := s.now().UTC()
	state, warning := readController(dir, now, b)
	if lock != nil {
		tickets, err := s.tickets(dir, now, false, nil, "")
		if err != nil {
			return status, err
		}
		for _, ticket := range tickets {
			if ticket.AcquiredAt == nil {
				status.Queued[ticket.Priority]++
			} else {
				status.Holders = append(status.Holders, ticket)
				status.InUse++
			}
		}
		sort.Slice(status.Holders, func(i, j int) bool { return status.Holders[i].Sequence < status.Holders[j].Sequence })
	}
	state.expire(now, b)
	attempts, throttled := state.counts(now)
	status.Attempts = attempts
	if attempts != 0 {
		status.ThrottleRate = float64(throttled) / float64(attempts)
	}
	status.GoodputRPM = float64(attempts-throttled) / b.lookback().Minutes()
	status.Admitted, status.TotalAttempts, status.ThrottledAttempts = state.Admitted, state.Attempts, state.Throttled
	status.Window, status.LastAdjustment, status.Warning = state.Window, state.LastAdjustment, warning
	return status, nil
}

func readController(dir string, now time.Time, b Bucket) (*controller, string) {
	var state controller
	fallback := &controller{Version: 1, Window: b.fallback(), LastTick: now}
	err := readJSON(filepath.Join(dir, "controller.json"), &state, 1<<20)
	if errors.Is(err, os.ErrNotExist) {
		return fallback, "admission_state_missing"
	}
	if err != nil || state.Version != 1 || state.Window < 1 || state.Window > 256 ||
		state.Attempts < 0 || state.Throttled < 0 || state.Throttled > state.Attempts || state.Admitted < 0 ||
		state.LastTick.IsZero() || state.LastTick.After(now) || len(state.Samples) > 3601 {
		return fallback, "admission_state_invalid"
	}
	var previous time.Time
	var attempts, throttled int64
	for _, sample := range state.Samples {
		if !sample.At.After(previous) || sample.At.After(now) || sample.Attempts < 1 ||
			sample.Throttled < 0 || sample.Throttled > sample.Attempts ||
			sample.Attempts > state.Attempts-attempts || sample.Throttled > state.Throttled-throttled {
			return fallback, "admission_state_invalid"
		}
		previous = sample.At
		attempts += sample.Attempts
		throttled += sample.Throttled
	}
	state.Window = max(b.MinWindow, min(state.Window, b.MaxWindow))
	state.expire(now, b)
	return &state, ""
}

func (s *Store) tickets(dir string, now time.Time, prune bool, own *os.File, ownKey string) ([]Ticket, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var tickets []Ticket
	for _, entry := range entries {
		key, ok := strings.CutSuffix(entry.Name(), ".json")
		if !ok || len(key) != 64 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		var ticket Ticket
		if err := readJSON(path, &ticket, 4096); err != nil {
			return nil, err
		}
		if !ValidHolder(ticket.Holder) || ticketKey(ticket.Holder) != key || !ValidPriority(ticket.Priority) ||
			ticket.PID <= 0 || ticket.Start == "" || ticket.Sequence == 0 || ticket.QueuedAt.IsZero() ||
			ticket.External && (ticket.AcquiredAt == nil || ticket.VerifiedAt == nil || ticket.ClaimOwner != nil) ||
			ticket.ClaimOwner != nil && (ticket.AcquiredAt != nil || ticket.ClaimOwner.PID <= 0 || ticket.ClaimOwner.Start == "") {
			return nil, errors.New("invalid admission ticket")
		}
		locked := own != nil && key == ownKey
		if !locked && !ticket.External {
			file, err := openOwned(filepath.Join(dir, key+".lock"), false)
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return nil, err
			}
			if file != nil {
				err := flock(file, unix.LOCK_EX|unix.LOCK_NB)
				file.Close()
				locked = errors.Is(err, unix.EWOULDBLOCK)
				if err != nil && !locked {
					return nil, err
				}
			}
		}
		if ticket.External {
			start, err := s.start(ticket.PID)
			locked = start == ticket.Start || err != nil && now.Sub(*ticket.VerifiedAt) < time.Minute
			if prune && err == nil && locked && now.Sub(*ticket.VerifiedAt) >= 30*time.Second {
				ticket.VerifiedAt = &now
				if err := writeJSON(dir, entry.Name(), &ticket); err != nil {
					return nil, err
				}
			}
		}
		if locked {
			tickets = append(tickets, ticket)
			continue
		}
		if prune {
			if err := os.Remove(path); err != nil {
				return nil, err
			}
			_ = os.Remove(filepath.Join(dir, key+".lock"))
		}
	}
	return tickets, nil
}

var flockCall sync.Mutex

func flock(file *os.File, how int) error {
	flockCall.Lock()
	defer flockCall.Unlock()
	for {
		err := unix.Flock(int(file.Fd()), how)
		if !errors.Is(err, unix.EINTR) {
			return err
		}
	}
}

func (s *Store) lock(ctx context.Context, name string, create bool) (*os.File, string, error) {
	if !bucketName.MatchString(name) {
		return nil, "", errors.New("invalid admission bucket")
	}
	dir := filepath.Join(s.dir, name)
	for _, path := range []string{s.dir, dir} {
		if create {
			if err := os.MkdirAll(path, 0o700); err != nil {
				return nil, dir, err
			}
		}
		var st unix.Stat_t
		if err := unix.Lstat(path, &st); err != nil {
			if !create && errors.Is(err, os.ErrNotExist) {
				return nil, dir, nil
			}
			return nil, dir, err
		}
		if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0o077 != 0 || st.Uid != uint32(os.Geteuid()) {
			return nil, dir, errors.New("admission directory must be owner-only")
		}
	}
	file, err := openOwned(filepath.Join(dir, "bucket.lock"), create)
	if !create && errors.Is(err, os.ErrNotExist) {
		return nil, dir, nil
	}
	if err != nil {
		return nil, dir, err
	}
	lockCtx, cancel := context.WithTimeout(ctx, lockBudget)
	defer cancel()
	for {
		if lockCtx.Err() != nil {
			file.Close()
			if err := ctx.Err(); err != nil {
				return nil, dir, err
			}
			return nil, dir, ErrLockBusy
		}
		err = flock(file, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			return file, dir, nil
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			file.Close()
			return nil, dir, err
		}
		select {
		case <-lockCtx.Done():
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func openOwned(path string, create bool) (*os.File, error) {
	flags := unix.O_RDONLY | unix.O_NOFOLLOW | unix.O_CLOEXEC | unix.O_NONBLOCK
	if create {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Open(path, flags, 0o600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		file.Close()
		return nil, err
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0o077 != 0 || st.Uid != uint32(os.Geteuid()) || st.Nlink != 1 {
		file.Close()
		return nil, errors.New("admission file must be owner-only and regular")
	}
	return file, nil
}

func readJSON(path string, value any, limit int64) error {
	f, err := openOwned(path, false)
	if err != nil {
		return err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if err != nil {
		return err
	}
	if int64(len(data)) > limit {
		return errors.New("admission state is too large")
	}
	return json.Unmarshal(data, value)
}

func writeJSON(dir, name string, value any) error {
	f, err := os.CreateTemp(dir, ".write-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	err = json.NewEncoder(f).Encode(value)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(f.Name(), filepath.Join(dir, name))
}
