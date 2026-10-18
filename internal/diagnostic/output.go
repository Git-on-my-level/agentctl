package diagnostic

// Failed output is a private, bounded diagnostic cache, not a result authority.
import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"golang.org/x/sys/unix"
)

const OutputLimit = 64 << 10
const OutputFileLimit = 128

// OutputFile is a content-free deletion proof included in cleanup plan digests.
type OutputFile struct {
	Name   string `json:"name"`
	Bytes  int64  `json:"bytes"`
	SHA256 string `json:"sha256"`
}

type Outputs struct {
	root *os.Root
	lock *os.File
	dir  string
}

// OpenOutputs does not mutate anything unless lock is true. Writers and cleanup
// share a nonblocking advisory lock; a busy cache fails closed rather than hanging.
func OpenOutputs(dir string, lock bool) (*Outputs, error) {
	info, err := os.Lstat(dir)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != 0700 {
		return nil, errors.New("diagnostic directory must be private mode 0700")
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		return nil, err
	}
	actual, err := root.Stat(".")
	if err != nil || !os.SameFile(info, actual) {
		root.Close()
		return nil, errors.New("diagnostic directory changed")
	}
	o := &Outputs{root: root, dir: dir}
	if lock {
		f, err := root.OpenFile(".failed-output.lock", os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
		if err != nil {
			o.Close()
			return nil, err
		}
		o.lock = f
		info, err := f.Stat()
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
			o.Close()
			return nil, errors.New("unsafe diagnostic lock")
		}
		if err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
			o.Close()
			return nil, fmt.Errorf("diagnostic cache busy: %w", err)
		}
	}
	return o, nil
}
func (o *Outputs) Close() {
	if o.lock != nil {
		o.lock.Close()
	}
	o.root.Close()
}
func outputName(id string) (string, error) {
	if _, err := ids.ParseExecutionID(id); err != nil {
		return "", err
	}
	return "failed-output-" + id + ".txt", nil
}

var legacyOutputName = regexp.MustCompile(`^failed-output-[0-9]+\.txt$`)

func managedOutput(name string) bool {
	if legacyOutputName.MatchString(name) {
		return true
	}
	id := strings.TrimSuffix(strings.TrimPrefix(name, "failed-output-"), ".txt")
	want, err := outputName(id)
	return err == nil && want == name
}
func (o *Outputs) inspect(name string) (*OutputFile, error) {
	if !managedOutput(name) {
		return nil, errors.New("invalid diagnostic filename")
	}
	f, err := o.root.OpenFile(name, os.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > OutputLimit {
		return nil, errors.New("unsafe diagnostic file")
	}
	raw, err := io.ReadAll(io.LimitReader(f, OutputLimit+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > OutputLimit {
		return nil, errors.New("diagnostic exceeds bound")
	}
	sum := sha256.Sum256(raw)
	return &OutputFile{Name: name, Bytes: int64(len(raw)), SHA256: "sha256:" + hex.EncodeToString(sum[:])}, nil
}

// Inspect accepts only the execution-named sibling, or a legacy random sibling
// explicitly referenced by that outcome. It never traverses an arbitrary pointer.
func (o *Outputs) Inspect(id, ref string) (*OutputFile, error) {
	name, err := outputName(id)
	if err != nil {
		return nil, err
	}
	if ref != "" {
		if filepath.Clean(filepath.Dir(ref)) != filepath.Clean(o.dir) {
			return nil, errors.New("diagnostic pointer is not a sibling")
		}
		base := filepath.Base(ref)
		if base != name && !legacyOutputName.MatchString(base) {
			return nil, errors.New("diagnostic pointer does not belong to execution")
		}
		name = base
	}
	return o.inspect(name)
}
func (o *Outputs) Remove(proof OutputFile) error {
	if o.lock == nil {
		return errors.New("diagnostic removal requires lock")
	}
	current, err := o.inspect(proof.Name)
	if err != nil {
		return err
	}
	if current == nil || *current != proof {
		return errors.New("diagnostic file changed; review a new cleanup plan")
	}
	return o.root.Remove(proof.Name)
}
func (o *Outputs) Write(id string, raw []byte) (string, error) {
	if o.lock == nil {
		return "", errors.New("diagnostic write requires lock")
	}
	name, err := outputName(id)
	if err != nil {
		return "", err
	}
	path := filepath.Join(o.dir, name)
	if len(path) > 1024 {
		return "", errors.New("diagnostic pointer exceeds bound")
	}
	if _, err := o.root.Lstat(name); !errors.Is(err, os.ErrNotExist) {
		return "", errors.New("diagnostic already exists or cannot be inspected")
	}
	// Prune before creation so the hard count bound holds even after a crash.
	d, err := o.root.Open(".")
	if err != nil {
		return "", err
	}
	entries, err := d.ReadDir(-1)
	d.Close()
	if err != nil {
		return "", err
	}
	type entry struct {
		name string
		info os.FileInfo
	}
	files := []entry{}
	for _, e := range entries {
		if !managedOutput(e.Name()) {
			continue
		}
		info, err := o.root.Lstat(e.Name())
		if err != nil {
			return "", err
		}
		if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Size() > OutputLimit {
			return "", errors.New("unsafe diagnostic cache entry")
		}
		files = append(files, entry{e.Name(), info})
	}
	sort.Slice(files, func(i, j int) bool {
		if files[i].info.ModTime().Equal(files[j].info.ModTime()) {
			return files[i].name < files[j].name
		}
		return files[i].info.ModTime().Before(files[j].info.ModTime())
	})
	for len(files) >= OutputFileLimit {
		if err := o.root.Remove(files[0].name); err != nil {
			return "", err
		}
		files = files[1:]
	}
	f, err := o.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return "", err
	}
	if len(raw) > OutputLimit {
		raw = raw[:OutputLimit]
	}
	_, err = f.Write(raw)
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		o.root.Remove(name)
		return "", err
	}
	return path, nil
}
