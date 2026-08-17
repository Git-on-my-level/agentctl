package updatecheck

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckNotifiesOncePerUTCDay(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.3.3"})
	}))
	defer server.Close()

	now := time.Date(2026, 8, 16, 23, 30, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state", "update-check.json")
	options := Options{CurrentVersion: "v0.3.2", StatePath: path, Endpoint: server.URL, Now: func() time.Time { return now }, Client: server.Client(), Getenv: func(string) string { return "" }}
	notice, err := Check(context.Background(), options)
	if err != nil || notice == nil || notice.LatestVersion != "v0.3.3" {
		t.Fatalf("first check notice=%#v err=%v", notice, err)
	}
	if second, err := Check(context.Background(), options); err != nil || second != nil {
		t.Fatalf("second check notice=%#v err=%v", second, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d want=1", requests.Load())
	}

	now = now.Add(25 * time.Hour)
	if nextDay, err := Check(context.Background(), options); err != nil || nextDay == nil {
		t.Fatalf("next-day check notice=%#v err=%v", nextDay, err)
	}
	if requests.Load() != 2 {
		t.Fatalf("requests=%d want=2", requests.Load())
	}
}

func TestCheckSkipsCurrentDevelopmentAndDisabledVersions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check.json")
	for _, test := range []Options{
		{CurrentVersion: "0.1.0-dev", StatePath: path},
		{CurrentVersion: "v0.3.2-dirty", StatePath: path},
		{CurrentVersion: "v0.3.2", StatePath: path, Getenv: func(string) string { return "off" }},
	} {
		if notice, err := Check(context.Background(), test); err != nil || notice != nil {
			t.Fatalf("options=%#v notice=%#v err=%v", test, notice, err)
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("disabled check wrote state: %v", err)
	}
}

func TestCheckFailsOpenAndBacksOffNetworkFailure(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		http.Error(w, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	now := time.Date(2026, 8, 16, 12, 0, 0, 0, time.UTC)
	options := Options{CurrentVersion: "v0.3.2", StatePath: filepath.Join(t.TempDir(), "state", "update-check.json"), Endpoint: server.URL, Now: func() time.Time { return now }, Client: server.Client(), Getenv: func(string) string { return "" }}
	if notice, err := Check(context.Background(), options); err == nil || notice != nil {
		t.Fatalf("failed check notice=%#v err=%v", notice, err)
	}
	if notice, err := Check(context.Background(), options); err != nil || notice != nil {
		t.Fatalf("backoff check notice=%#v err=%v", notice, err)
	}
	if requests.Load() != 1 {
		t.Fatalf("requests=%d want=1", requests.Load())
	}
}

func TestCheckDoesNotNotifyForCurrentOrOlderRelease(t *testing.T) {
	for _, latest := range []string{"v0.3.2", "v0.3.1"} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": latest})
		}))
		options := Options{CurrentVersion: "v0.3.2", StatePath: filepath.Join(t.TempDir(), "state", "update-check.json"), Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }}
		if notice, err := Check(context.Background(), options); err != nil || notice != nil {
			t.Fatalf("latest=%s notice=%#v err=%v", latest, notice, err)
		}
		server.Close()
	}
}

func TestConcurrentChecksCoalesce(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			close(started)
		}
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{"tag_name": "v0.3.3"})
	}))
	defer server.Close()

	options := Options{CurrentVersion: "v0.3.2", StatePath: filepath.Join(t.TempDir(), "state", "update-check.json"), Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }}
	var notices atomic.Int32
	var wait sync.WaitGroup
	wait.Add(8)
	for range 8 {
		go func() {
			defer wait.Done()
			notice, _ := Check(context.Background(), options)
			if notice != nil {
				notices.Add(1)
			}
		}()
	}
	<-started
	close(release)
	wait.Wait()
	if requests.Load() != 1 || notices.Load() != 1 {
		t.Fatalf("requests=%d notices=%d want=1,1", requests.Load(), notices.Load())
	}
}

func TestUnsafeCacheFailsOpenWithoutReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "update-check.json")
	if err := os.WriteFile(path, []byte(`{"schema_version":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if notice, err := Check(context.Background(), Options{CurrentVersion: "v0.3.2", StatePath: path}); err == nil || notice != nil {
		t.Fatalf("notice=%#v err=%v", notice, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o644 {
		t.Fatalf("unsafe cache was replaced: mode=%v", info.Mode().Perm())
	}
}

func TestSymlinkedCacheParentIsRejectedBeforeMutation(t *testing.T) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkedParent := filepath.Join(root, "linked")
	if err := os.Symlink(realParent, linkedParent); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(linkedParent, "nested", "update-check.json")
	if notice, err := Check(context.Background(), Options{CurrentVersion: "v0.3.2", StatePath: statePath}); err == nil || notice != nil {
		t.Fatalf("notice=%#v err=%v", notice, err)
	}
	if _, err := os.Stat(filepath.Join(realParent, "nested")); !os.IsNotExist(err) {
		t.Fatalf("symlink target was mutated: %v", err)
	}
}
