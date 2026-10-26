package updatecheck

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/testutil"
)

func TestPendingUpdateConvergesAfterOwnersDrain(t *testing.T) {
	prefix := testutil.TempDir(t)
	executable := filepath.Join(prefix, "bin", "agentctl")
	oldBinary := []byte("#!/bin/sh\nexit 0\n")
	writeManagedInstall(t, prefix, executable, oldBinary)
	archiveName := fmt.Sprintf("agentctl_v0.3.4_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	newBinary := []byte("#!/bin/sh\nprintf new\n")
	archive := releaseArchive(t, strings.TrimSuffix(archiveName, ".tar.gz"), newBinary)
	digest := sha256.Sum256(archive)
	checks, downloads := 0, 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/latest":
			checks++
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.4"})
		case strings.HasSuffix(r.URL.Path, "/SHA256SUMS"):
			downloads++
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), archiveName)
		case strings.HasSuffix(r.URL.Path, "/"+archiveName):
			downloads++
			_, _ = w.Write(archive)
		default:
			t.Errorf("unrequested release: %s", r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	statePath := filepath.Join(testutil.TempDir(t), "state", "update-state.json")
	owners := []string{"pid:101", "pid:202"}
	options := ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", Client: server.Client(), Getenv: func(string) string { return "" }}, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client(), Explicit: true, OwnerIDs: func() ([]string, error) { return owners, nil }}
	_, err := Apply(context.Background(), options)
	var failure *ApplyError
	if !errors.As(err, &failure) || failure.Code != "live_owners_deferred" {
		t.Fatalf("err=%v", err)
	}
	status, err := ReadStatus(statePath, "", func(string) string { return "notify" })
	if err != nil || status.Pending == nil || status.Pending.Version != "v0.3.4" || !status.Pending.Explicit || strings.Join(status.Pending.WaitingOn, ",") != "pid:101,pid:202" {
		t.Fatalf("status=%+v err=%v", status, err)
	}
	// Even repeated retries must leave live owners' installation untouched.
	_, err = Resume(context.Background(), options, ModeNotify)
	if !errors.As(err, &failure) || failure.Code != "live_owners_deferred" || downloads != 0 {
		t.Fatalf("err=%v downloads=%d", err, downloads)
	}
	if got, _ := os.ReadFile(executable); !bytes.Equal(got, oldBinary) {
		t.Fatal("live owners' binary changed")
	}
	owners = nil
	// Off pauses an explicit request; switching back to notify can finish it.
	result, err := Resume(context.Background(), options, ModeOff)
	if err != nil || result.Updated || downloads != 0 {
		t.Fatalf("off: %+v %v", result, err)
	}
	result, err = Resume(context.Background(), options, ModeNotify)
	if err != nil || !result.Updated || result.InstalledVersion != "v0.3.4" {
		t.Fatalf("drained: %+v %v", result, err)
	}
	if checks != 1 {
		t.Fatalf("pending request rediscovered latest %d times", checks)
	}
	if got, _ := os.ReadFile(executable); !bytes.Equal(got, newBinary) {
		t.Fatal("pending release not installed")
	}
	status, err = ReadStatus(statePath, "", func(string) string { return "notify" })
	if err != nil || status.Pending != nil || status.LastErrorCode != "" {
		t.Fatalf("completed: %+v %v", status, err)
	}
	_, err = Resume(context.Background(), options, ModeNotify)
	if err != nil || downloads != 2 {
		t.Fatalf("resume not idempotent: downloads=%d err=%v", downloads, err)
	}
}

func TestNotifyDoesNotResumeUnrequestedInstall(t *testing.T) {
	path := filepath.Join(testutil.TempDir(t), "state", "state.json")
	if err := writeState(path, cacheState{SchemaVersion: stateSchema, Pending: &PendingApply{Version: "v0.3.4"}}); err != nil {
		t.Fatal(err)
	}
	result, err := Resume(context.Background(), ApplyOptions{Check: Options{StatePath: path}}, ModeNotify)
	if err != nil || result.Updated {
		t.Fatalf("%+v %v", result, err)
	}
}
