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
	"testing"
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

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		switch {
		case request.URL.Path == "/latest":
			_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.4"})
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
	result, err := Apply(context.Background(), ApplyOptions{
		Check:      Options{CurrentVersion: "v0.3.3", StatePath: statePath, Endpoint: server.URL + "/latest", Client: server.Client(), Getenv: func(string) string { return "" }},
		Executable: executable, ReleaseBaseURL: server.URL + "/downloads", Client: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.InstalledVersion != "v0.3.4" {
		t.Fatalf("result=%#v", result)
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
