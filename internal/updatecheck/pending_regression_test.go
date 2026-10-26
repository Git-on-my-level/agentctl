package updatecheck

import (
	"context"
	"encoding/json"
	"github.com/Git-on-my-level/agentctl/internal/testutil"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestDeferredStatusRecordsPendingRequest(t *testing.T) {
	prefix := testutil.TempDir(t)
	executable := filepath.Join(prefix, "bin", "agentctl")
	writeManagedInstall(t, prefix, executable, []byte("#!/bin/sh\nexit 0\n"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{"tag_name": "v0.3.4"})
	}))
	defer server.Close()
	path := filepath.Join(testutil.TempDir(t), "state", "update-state.json")
	_, _ = Apply(context.Background(), ApplyOptions{Check: Options{CurrentVersion: "v0.3.3", StatePath: path, Endpoint: server.URL, Client: server.Client(), Getenv: func(string) string { return "" }}, Executable: executable, LiveOwners: func() (int, error) { return 1, nil }})
	status, err := ReadStatus(path, "", func(string) string { return "" })
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(status)
	var doc map[string]any
	_ = json.Unmarshal(data, &doc)
	if doc["pending"] == nil {
		t.Fatalf("deferred update has no durable pending request: %s", data)
	}
}
