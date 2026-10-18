package diagnostic

import (
	"github.com/Git-on-my-level/agentctl/internal/ids"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testOutputID(n uint64) string {
	id, _ := ids.FromPayload(ids.TypeExecution, n)
	return id.String()
}
func TestOutputCacheCapOldestFirst(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	o, err := OpenOutputs(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	for i := 0; i < OutputFileLimit; i++ {
		path, err := o.Write(testOutputID(uint64(i)), []byte("private diagnostic"))
		if err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	// The oldest file has the highest ID, so lexical ordering cannot pass.
	oldest, _ := outputName(testOutputID(OutputFileLimit - 1))
	stamp := time.Unix(0, 0)
	if err := os.Chtimes(filepath.Join(dir, oldest), stamp, stamp); err != nil {
		t.Fatal(err)
	}
	path, err := o.Write(testOutputID(OutputFileLimit), make([]byte, OutputLimit+100))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, oldest)); !os.IsNotExist(err) {
		t.Fatal("oldest diagnostic was not evicted", err)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "failed-output-*.txt"))
	if len(files) != OutputFileLimit {
		t.Fatalf("count=%d", len(files))
	}
	info, _ := os.Stat(path)
	if info.Size() != OutputLimit || info.Mode().Perm() != 0600 {
		t.Fatal("bound or mode incorrect", info)
	}
	if filepath.Base(path) != "failed-output-"+testOutputID(OutputFileLimit)+".txt" {
		t.Fatal("missing execution ID")
	}
}
func TestOutputCachePrivateConfinedAndPlanReadOnly(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0700)
	read, err := OpenOutputs(dir, false)
	if err != nil {
		t.Fatal(err)
	}
	proof, err := read.Inspect(testOutputID(1), "")
	if err != nil || proof != nil {
		t.Fatal(proof, err)
	}
	read.Close()
	entries, _ := os.ReadDir(dir)
	if len(entries) != 0 {
		t.Fatal("plan mutated cache")
	}
	o, err := OpenOutputs(dir, true)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Close()
	if other, err := OpenOutputs(dir, true); err == nil {
		other.Close()
		t.Fatal("concurrent writer bypassed lock")
	}
	path, err := o.Write(testOutputID(1), []byte("private"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := o.Inspect(testOutputID(2), path); err == nil {
		t.Fatal("cross-execution pointer accepted")
	}
	if _, err := o.Inspect(testOutputID(1), "/outside/failed-output-123.txt"); err == nil {
		t.Fatal("external pointer accepted")
	}
	proof, err = o.Inspect(testOutputID(1), path)
	if err != nil {
		t.Fatal(err)
	}
	os.WriteFile(path, []byte("changed"), 0600)
	if err := o.Remove(*proof); err == nil {
		t.Fatal("changed proof accepted")
	}
	os.Remove(path)
	os.Symlink(filepath.Join(t.TempDir(), "external"), path)
	if _, err := o.Inspect(testOutputID(1), path); err == nil {
		t.Fatal("symlink accepted")
	}
}
