package updatecheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestCachedNoticePreservesConcurrentPending(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "update-state.json")
	stale := cacheState{SchemaVersion: stateSchema, LatestVersion: "v0.3.4"}
	if err := writeState(path, stale); err != nil {
		t.Fatal(err)
	}
	// Check read this snapshot before a concurrent explicit apply records intent.
	if err := recordPending(ApplyOptions{Check: Options{StatePath: path}, Explicit: true}, "v0.3.4"); err != nil {
		t.Fatal(err)
	}
	current, _ := parseVersion("v0.3.3")
	notice, err := cachedNotice(path, stale, current, "2026-10-26", false)
	if err != nil || notice == nil {
		t.Fatalf("notice=%+v err=%v", notice, err)
	}
	state, err := readState(path)
	if err != nil || state.Pending == nil || !state.Pending.Explicit || state.Pending.Version != "v0.3.4" {
		t.Fatalf("cached notice erased concurrent pending request: %+v %v", state, err)
	}
	if state.NotifiedOn != "2026-10-26" {
		t.Fatal("notice bookkeeping lost")
	}
}

func TestConcurrentDrainingOwnersInstallPending(t *testing.T) {
	prefix := testutil.TempDir(t)
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	name := fmt.Sprintf("agentctl_v0.3.4_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	newBinary := []byte("#!/bin/sh\nprintf new\n")
	archive := releaseArchive(t, strings.TrimSuffix(name, ".tar.gz"), newBinary)
	digest := sha256.Sum256(archive)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "SHA256SUMS") {
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), name)
		} else {
			_, _ = w.Write(archive)
		}
	}))
	defer server.Close()
	path := filepath.Join(testutil.TempDir(t), "state", "update-state.json")
	if err := writeState(path, cacheState{SchemaVersion: stateSchema, InstalledVersion: "v0.3.3", Pending: &PendingApply{Version: "v0.3.4", Explicit: true}}); err != nil {
		t.Fatal(err)
	}
	scanned := make(chan struct{})
	releaseScan := make(chan struct{})
	var scans atomic.Int32
	options := ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: path, Getenv: func(string) string { return "" }}, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client(), OwnerIDs: func() ([]string, error) {
		if scans.Add(1) == 1 {
			close(scanned)
			<-releaseScan
			return []string{"pid:202"}, nil
		}
		return []string{}, nil
	}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	type outcome struct {
		result ApplyResult
		err    error
	}
	a, b := make(chan outcome, 1), make(chan outcome, 1)
	go func() { r, e := Resume(ctx, options, ModeNotify); a <- outcome{r, e} }()
	select {
	case <-scanned:
	case <-ctx.Done():
		t.Fatal("first owner never scanned")
	}
	go func() { r, e := Resume(ctx, options, ModeNotify); b <- outcome{r, e} }()
	// A retains its stale observation of B while B contends. A's stale deferral
	// must not consume B's final retry when the two owners have finished.
	var second outcome
	received := false
	select {
	case second = <-b:
		received = true
	case <-time.After(50 * time.Millisecond):
	}
	close(releaseScan)
	first := <-a
	if !received {
		second = <-b
	}
	if !first.result.Updated && !second.result.Updated {
		t.Fatalf("drained owners stranded pending update: A=%v B=%v", first.err, second.err)
	}
	if got, _ := os.ReadFile(executable); !bytes.Equal(got, newBinary) {
		t.Fatal("drain did not install update")
	}
	state, err := readState(path)
	if err != nil || state.Pending != nil {
		t.Fatalf("pending not cleared: %+v %v", state, err)
	}
}
