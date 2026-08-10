package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/output"
)

func schemaRepositoryRoot(t *testing.T) string {
	t.Helper()
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for candidate := workingDirectory; ; candidate = filepath.Dir(candidate) {
		if _, err := os.Stat(filepath.Join(candidate, "go.mod")); err == nil {
			return candidate
		}
		parent := filepath.Dir(candidate)
		if parent == candidate {
			t.Fatalf("could not locate repository root from %q", workingDirectory)
		}
	}
}

func TestSchemaListPublishesEverySchemaArtifact(t *testing.T) {
	var stdout bytes.Buffer
	renderer := output.Renderer{Mode: output.JSON, Writer: &stdout}
	if problem := (&app{}).schemaCommand(renderer, []string{"list"}); problem != nil {
		t.Fatal(problem)
	}
	var document struct {
		OK     bool `json:"ok"`
		Result struct {
			Schemas []struct {
				Name    string `json:"name"`
				Version int    `json:"version"`
				File    string `json:"file"`
			} `json:"schemas"`
		} `json:"result"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if !document.OK {
		t.Fatalf("schema list failed: %s", stdout.String())
	}
	want := map[string]string{
		"adapter-manifest":  "schemas/adapter-manifest.schema.json",
		"callback-envelope": "schemas/callback-envelope.schema.json",
		"context-result":    "schemas/context-result.schema.json",
		"error":             "schemas/error.schema.json",
		"event":             "schemas/event.schema.json",
		"event-page":        "schemas/event-page.schema.json",
		"execution":         "schemas/execution.schema.json",
		"knowledge-source":  "schemas/knowledge-source.schema.json",
		"subscription":      "schemas/subscription.schema.json",
	}
	if len(document.Result.Schemas) != len(want) {
		t.Fatalf("schema count=%d want=%d: %#v", len(document.Result.Schemas), len(want), document.Result.Schemas)
	}
	root := schemaRepositoryRoot(t)
	for _, item := range document.Result.Schemas {
		if item.Version != 1 || want[item.Name] != item.File {
			t.Fatalf("unexpected schema listing item: %#v", item)
		}
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(item.File))); err != nil {
			t.Fatalf("listed schema %q is not present: %v", item.File, err)
		}
		delete(want, item.Name)
	}
	if len(want) != 0 {
		t.Fatalf("schemas missing from listing: %#v", want)
	}
}

func TestNewSchemaDocumentsDeclareDraftAndRequiredShape(t *testing.T) {
	root := schemaRepositoryRoot(t)
	expected := map[string][]string{
		"callback-envelope.schema.json": {"schema_version", "delivery_id", "subscription_id", "event_id", "event_dedupe_key", "attempt", "sent_at", "expires_at", "nonce", "event"},
		"context-result.schema.json":    {"bundle_revision", "matches"},
		"event-page.schema.json":        {"events", "scanned", "filtered", "page_limit"},
		"subscription.schema.json":      {"id", "origin_host_id", "filter", "destination", "expires_at", "cursor", "state", "auto_expire_on_terminal"},
	}
	for filename, required := range expected {
		data, err := os.ReadFile(filepath.Join(root, "schemas", filename))
		if err != nil {
			t.Fatal(err)
		}
		var schema struct {
			Schema string   `json:"$schema"`
			ID     string   `json:"$id"`
			Req    []string `json:"required"`
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("%s: %v", filename, err)
		}
		if schema.Schema != "https://json-schema.org/draft/2020-12/schema" || schema.ID == "" {
			t.Fatalf("%s missing draft/id: %#v", filename, schema)
		}
		seen := map[string]bool{}
		for _, field := range schema.Req {
			seen[field] = true
		}
		for _, field := range required {
			if !seen[field] {
				t.Errorf("%s missing required field %q", filename, field)
			}
		}
	}
}
