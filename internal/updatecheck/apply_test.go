package updatecheck

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"sync/atomic"
	"testing"
	"time"
)

func TestApplyUpdatesManagedInstallFromVerifiedArchive(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl-custom")
	oldBinary := []byte("#!/bin/sh\nexit 0\n")
	writeManagedInstall(t, prefix, executable, oldBinary)
	archiveName := fmt.Sprintf("agentctl_v0.3.4_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	newBinary := []byte("#!/bin/sh\nprintf new\n")
	archive := releaseArchive(t, strings.TrimSuffix(archiveName, ".tar.gz"), newBinary)
	digest := sha256.Sum256(archive)

	var releaseChecks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/latest":
			releaseChecks.Add(1)
			http.Error(w, "rate limit exceeded", http.StatusForbidden)
		case request.URL.Path == "/releases/latest":
			http.Redirect(w, request, "/releases/tag/v0.3.4", http.StatusFound)
		case request.URL.Path == "/releases/tag/v0.3.4":
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(request.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), archiveName)
		case strings.HasSuffix(request.URL.Path, "/"+archiveName):
			_, _ = w.Write(archive)
		default:
			http.NotFound(w, request)
		}
	}))
	defer server.Close()

	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, CheckedOn: time.Now().UTC().Format("2006-01-02"), LatestVersion: "v0.3.3", InstalledVersion: "v0.3.3"}); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyOptions{
		Check:      Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", ReleasePageURL: server.URL + "/releases/latest", Client: server.Client(), Getenv: func(string) string { return "" }, Force: true},
		Executable: executable, ReleaseBaseURL: server.URL + "/downloads", Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.InstalledVersion != "v0.3.4" {
		t.Fatalf("result=%#v", result)
	}
	if releaseChecks.Load() != 1 {
		t.Fatalf("forced same-day check made %d requests, want 1", releaseChecks.Load())
	}
	installed, err := os.ReadFile(executable)
	if err != nil || !bytes.Equal(installed, newBinary) {
		t.Fatalf("installed=%q err=%v", installed, err)
	}
	status, err := ReadStatus(statePath, filepath.Join(t.TempDir(), "missing-policy"), func(string) string { return "" })
	if err != nil || status.InstalledVersion != "v0.3.4" || status.LastErrorCode != "" {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestApplyRejectsNoncanonicalReleaseTagBeforeDownload(t *testing.T) {
	for _, tag := range []string{"v0.3.4/../../evil", "v0.3.4;touch", "v0.3.4..", "v0.3.4-rc1", "v0.03.4", "0.3.4"} {
		t.Run(tag, func(t *testing.T) {
			prefix := t.TempDir()
			executable := filepath.Join(prefix, "bin", "agentctl")
			oldBinary := []byte("#!/bin/sh\nexit 0\n")
			writeManagedInstall(t, prefix, executable, oldBinary)
			var downloads atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				if request.URL.Path != "/latest" {
					downloads.Add(1)
					http.NotFound(w, request)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": tag})
			}))
			defer server.Close()
			statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
			_, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client()})
			var failure *ApplyError
			if !errors.As(err, &failure) || (failure.Code != "invalid_release_version" && failure.Code != "release_check_failed") || downloads.Load() != 0 {
				t.Fatalf("tag=%q error=%#v downloads=%d", tag, failure, downloads.Load())
			}
			installed, readErr := os.ReadFile(executable)
			if readErr != nil || !bytes.Equal(installed, oldBinary) {
				t.Fatalf("executable changed: %q (%v)", installed, readErr)
			}
		})
	}
}

func TestApplyRejectsNoncanonicalCachedTagBeforeDownload(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, CheckedOn: time.Now().UTC().Format("2006-01-02"), LatestVersion: "v0.03.4"}); err != nil {
		t.Fatal(err)
	}
	var downloads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downloads.Add(1)
		http.Error(w, "unexpected download", http.StatusInternalServerError)
	}))
	defer server.Close()
	_, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }}, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client()})
	var failure *ApplyError
	if !errors.As(err, &failure) || failure.Code != "invalid_release_version" || downloads.Load() != 0 {
		t.Fatalf("apply error=%#v downloads=%d", failure, downloads.Load())
	}
}

func TestApplyManagedInstallReportsBusyStateLock(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	state := cacheState{SchemaVersion: stateSchema, InstalledVersion: "v0.3.2"}
	if err := writeState(statePath, state); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	release, acquired, err := acquireLock(statePath+".lock", time.Now().UTC())
	if err != nil || !acquired {
		t.Fatalf("acquire lock: acquired=%t err=%v", acquired, err)
	}
	defer release()
	_, err = Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: statePath, Getenv: func(string) string { return "" }, Force: true}, Executable: executable})
	var failure *ApplyError
	if !errors.As(err, &failure) || failure.Code != "state_lock_failed" || !failure.Retryable {
		t.Fatalf("apply error=%#v raw=%v", failure, err)
	}
	after, err := os.ReadFile(statePath)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("busy state changed: %q (%v)", after, err)
	}
}

func TestApplyNoopReconcilesVersionAfterManualInstall(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	var releaseChecks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		releaseChecks.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.11.1"})
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, CheckedOn: time.Now().UTC().Format("2006-01-02"), LatestVersion: "v0.10.2", InstalledVersion: "v0.10.2"}); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.11.1", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: executable})
	if err != nil || result.Updated || result.InstalledVersion != "v0.11.1" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	state, err := readState(statePath)
	if err != nil || state.InstalledVersion != "v0.11.1" || state.LatestVersion != "v0.11.1" || releaseChecks.Load() != 1 {
		t.Fatalf("state=%#v err=%v releaseChecks=%d", state, err, releaseChecks.Load())
	}
}

func TestApplyCurrentUnmanagedBinaryDoesNotClaimInstalledVersion(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.11.1"})
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, InstalledVersion: "v0.10.2"}); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.11.1", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: filepath.Join(t.TempDir(), "not-managed")})
	if err != nil || result.Updated || result.InstalledVersion != "" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	state, err := readState(statePath)
	if err != nil || state.InstalledVersion != "v0.10.2" {
		t.Fatalf("unmanaged executable changed installed record: %#v (%v)", state, err)
	}
}

func TestApplyCurrentNoopClearsPreviousUpdateFailure(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	for _, latest := range []string{"v0.11.1", "v0.10.2"} {
		t.Run(latest, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
				_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": latest})
			}))
			defer server.Close()
			statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
			if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, InstalledVersion: "v0.11.1", LastErrorCode: "release_check_failed", LastErrorStage: "release_check_failed"}); err != nil {
				t.Fatal(err)
			}
			result, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.11.1", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: executable})
			if err != nil || result.Updated || result.InstalledVersion != "v0.11.1" {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			state, err := readState(statePath)
			if err != nil || state.LastErrorCode != "" || state.LastErrorStage != "" || state.LatestVersion != latest {
				t.Fatalf("noop retained stale update failure: %#v (%v)", state, err)
			}
		})
	}
}

func TestApplyReportsBusyLockInsteadOfCurrent(t *testing.T) {
	var releaseChecks atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		releaseChecks.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.11.2"})
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, InstalledVersion: "v0.11.1"}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath+".lock", []byte("1\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.11.1", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: filepath.Join(t.TempDir(), "not-managed")})
	var applyError *ApplyError
	if result.Updated || result.InstalledVersion != "" || !errors.As(err, &applyError) || applyError.Code != "state_lock_failed" || !applyError.Retryable {
		t.Fatalf("result=%#v error=%#v raw=%v", result, applyError, err)
	}
	if releaseChecks.Load() != 0 {
		t.Fatalf("busy lock still performed %d release lookups", releaseChecks.Load())
	}
}

func TestApplyClassifiesRateLimitedReleaseCheckAndReconcilesInstalledVersion(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		http.Error(w, "token=secret", http.StatusForbidden)
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	if err := writeState(statePath, cacheState{SchemaVersion: stateSchema, CheckedOn: time.Now().UTC().Format("2006-01-02"), LatestVersion: "v0.10.2", InstalledVersion: "v0.10.2"}); err != nil {
		t.Fatal(err)
	}
	_, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.11.1", StatePath: statePath, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }, Force: true}, Executable: executable})
	var applyError *ApplyError
	if !errors.As(err, &applyError) || applyError.Code != "release_check_failed" || applyError.SafeCause != "release lookup returned HTTP 403" || !applyError.Retryable {
		t.Fatalf("error=%#v raw=%v", applyError, err)
	}
	state, err := readState(statePath)
	if err != nil || state.InstalledVersion != "v0.11.1" || state.LastErrorCode != "release_check_failed" {
		t.Fatalf("state=%#v err=%v", state, err)
	}
}

func TestApplyRejectsChecksumMismatchBeforeInstaller(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	oldBinary := []byte("#!/bin/sh\nexit 0\n")
	writeManagedInstall(t, prefix, executable, oldBinary)
	archiveName := fmt.Sprintf("agentctl_v0.3.4_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	archive := releaseArchive(t, strings.TrimSuffix(archiveName, ".tar.gz"), []byte("new"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.4"})
		case strings.HasSuffix(request.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), archiveName)
		default:
			_, _ = w.Write(archive)
		}
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	_, err := Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", Client: server.Client(), Getenv: func(string) string { return "" }}, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client()})
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("err=%v", err)
	}
	var applyError *ApplyError
	if !errors.As(err, &applyError) || applyError.Code != "checksum_mismatch" || applyError.Retryable {
		t.Fatalf("apply error=%#v", applyError)
	}
	installed, _ := os.ReadFile(executable)
	if !bytes.Equal(installed, oldBinary) {
		t.Fatalf("checksum failure changed executable: %q", installed)
	}
}

func TestRetryableApplyFailureBecomesDueAfterBackoff(t *testing.T) {
	prefix := t.TempDir()
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	archiveName := fmt.Sprintf("agentctl_v0.3.4_%s_%s.tar.gz", runtime.GOOS, runtime.GOARCH)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.4"})
		case strings.HasSuffix(request.URL.Path, "/SHA256SUMS"):
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), archiveName)
		default:
			http.Error(w, "retry later", http.StatusServiceUnavailable)
		}
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "state", "update-state.json")
	options := Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", Client: server.Client(), Getenv: func(string) string { return "" }}
	_, err := Apply(context.Background(), ApplyOptions{Check: options, Executable: executable, ReleaseBaseURL: server.URL, Client: server.Client()})
	var applyError *ApplyError
	if !errors.As(err, &applyError) || applyError.Code != "archive_download_failed" || !applyError.Retryable {
		t.Fatalf("apply error=%#v err=%v", applyError, err)
	}
	state, err := readState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if state.CheckedOn != "" || Due(options) {
		t.Fatalf("retry state=%#v due=%v; want one-hour backoff", state, Due(options))
	}
	state.LastAttemptAt = time.Now().UTC().Add(-retryInterval - time.Minute)
	if err := writeState(statePath, state); err != nil {
		t.Fatal(err)
	}
	if !Due(options) {
		t.Fatal("retryable failure did not become due after backoff")
	}
}

func TestExtractArchiveRejectsTraversalAndLinks(t *testing.T) {
	for _, test := range []struct {
		name     string
		header   tar.Header
		contents []byte
	}{
		{name: "traversal", header: tar.Header{Name: "../escaped", Mode: 0o600, Size: 1, Typeflag: tar.TypeReg}, contents: []byte("x")},
		{name: "symlink", header: tar.Header{Name: "package/link", Mode: 0o700, Typeflag: tar.TypeSymlink, Linkname: "/tmp/target"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var archive bytes.Buffer
			compressed := gzip.NewWriter(&archive)
			writer := tar.NewWriter(compressed)
			if err := writer.WriteHeader(&test.header); err != nil {
				t.Fatal(err)
			}
			if len(test.contents) > 0 {
				if _, err := writer.Write(test.contents); err != nil {
					t.Fatal(err)
				}
			}
			if err := writer.Close(); err != nil {
				t.Fatal(err)
			}
			if err := compressed.Close(); err != nil {
				t.Fatal(err)
			}
			if err := extractArchive(archive.Bytes(), t.TempDir()); err == nil {
				t.Fatal("unsafe archive was accepted")
			}
		})
	}
}

func writeManagedInstall(t *testing.T, prefix, executable string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(executable), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executable, content, 0o700); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	manifest := filepath.Join(prefix, "share", "agentctl", "install-manifest")
	if err := os.MkdirAll(filepath.Dir(manifest), 0o700); err != nil {
		t.Fatal(err)
	}
	value := fmt.Sprintf("manifest_version=1\ntarget=%s\nsha256=%s\n", executable, hex.EncodeToString(digest[:]))
	if err := os.WriteFile(manifest, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
}

func releaseArchive(t *testing.T, root string, binary []byte) []byte {
	t.Helper()
	var result bytes.Buffer
	gz := gzip.NewWriter(&result)
	tarWriter := tar.NewWriter(gz)
	files := map[string]struct {
		content []byte
		mode    int64
	}{
		root + "/agentctl":           {binary, 0o700},
		root + "/scripts/install.sh": {[]byte("#!/bin/sh\nset -eu\ncp \"$2\" \"$4/bin/$6\"\nchmod 700 \"$4/bin/$6\"\n"), 0o700},
	}
	for name, file := range files {
		if err := tarWriter.WriteHeader(&tar.Header{Name: name, Mode: file.mode, Size: int64(len(file.content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tarWriter.Write(file.content); err != nil {
			t.Fatal(err)
		}
	}
	if err := tarWriter.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return result.Bytes()
}
