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
		"adapter-manifest":     "schemas/adapter-manifest.schema.json",
		"callback-envelope":    "schemas/callback-envelope.schema.json",
		"config-bundle":        "schemas/config-bundle.schema.json",
		"context-result":       "schemas/context-result.schema.json",
		"continue-result":      "schemas/continue-result.schema.json",
		"data-cleanup-plan":    "schemas/data-cleanup-plan.schema.json",
		"data-inventory":       "schemas/data-inventory.schema.json",
		"delegate-request":     "schemas/delegate-request.schema.json",
		"delegate-result":      "schemas/delegate-result.schema.json",
		"error":                "schemas/error.schema.json",
		"event":                "schemas/event.schema.json",
		"event-page":           "schemas/event-page.schema.json",
		"execution":            "schemas/execution.schema.json",
		"fanout-manifest":      "schemas/fanout-manifest.schema.json",
		"identity-report":      "schemas/identity-report.schema.json",
		"recent-result":        "schemas/recent-result.schema.json",
		"launch-recipe-report": "schemas/launch-recipe-report.schema.json",
		"inbox-result":         "schemas/inbox-result.schema.json",
		"knowledge-source":     "schemas/knowledge-source.schema.json",
		"outcome":              "schemas/outcome.schema.json",
		"skill-pack":           "schemas/skill-pack.schema.json",
		"skill-pack-report":    "schemas/skill-pack-report.schema.json",
		"steer-result":         "schemas/steer-result.schema.json",
		"subscription":         "schemas/subscription.schema.json",
		"task-contract-input":  "schemas/task-contract-input.schema.json",
		"workspace-owners":     "schemas/workspace-owners.schema.json",
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
		"identity-report.schema.json":   {"schema_version", "provider", "native_session", "execution", "environment", "capabilities", "harnesses"},
		"callback-envelope.schema.json": {"schema_version", "delivery_id", "subscription_id", "event_id", "event_dedupe_key", "attempt", "sent_at", "expires_at", "nonce", "event"},
		"context-result.schema.json":    {"bundle_revision", "matches"},
		"continue-result.schema.json":   {"continues", "resolved", "request_sha256", "reused"},
		"steer-result.schema.json":      {"id", "adapter", "state"},
		"event-page.schema.json":        {"events", "scanned", "filtered", "page_limit"},
		"fanout-manifest.schema.json":   {"schema_version", "children"},
		"inbox-result.schema.json":      {"executions", "count", "total", "has_more", "host_local", "as_of", "stale_after_seconds"},
		"outcome.schema.json":           {"schema_version", "execution_id", "revision", "state", "availability", "recorded_at", "source", "result_ref"},
		"skill-pack.schema.json":        {"schema_version", "skills"},
		"skill-pack-report.schema.json": {"schema_version", "healthy", "source", "manifest_sha256", "actions", "changed", "applied", "unsupported", "conflicts"},
		"subscription.schema.json":      {"id", "origin_host_id", "filter", "destination", "expires_at", "cursor", "state", "auto_expire_on_terminal"},
		"workspace-owners.schema.json":  {"schema_version", "authority", "ownership_semantics", "exclusive", "host_local", "owners", "unattributed_nonterminal_count", "evidence_complete"},
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

// assertResultMatchesSchemaShape keeps a command's result builder and its
// normative schema from drifting: every emitted member, at the top level and
// in the named nested objects, must be declared, and every required member
// must be present.
func assertResultMatchesSchemaShape(t *testing.T, out, filename string, nested ...string) {
	t.Helper()
	var envelope struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal([]byte(out), &envelope); err != nil || !envelope.OK {
		t.Fatalf("not a success envelope (%v): %s", err, out)
	}
	data, err := os.ReadFile(filepath.Join(schemaRepositoryRoot(t), "schemas", filename))
	if err != nil {
		t.Fatal(err)
	}
	type object struct {
		Required   []string                   `json:"required"`
		Properties map[string]json.RawMessage `json:"properties"`
	}
	check := func(where string, schema object, value map[string]any) {
		for key := range value {
			if _, ok := schema.Properties[key]; !ok {
				t.Errorf("%s: %s emits %q, which the schema does not declare", filename, where, key)
			}
		}
		for _, key := range schema.Required {
			if _, ok := value[key]; !ok {
				t.Errorf("%s: %s is missing required %q", filename, where, key)
			}
		}
	}
	var root object
	if err := json.Unmarshal(data, &root); err != nil {
		t.Fatal(err)
	}
	check("result", root, envelope.Result)
	for _, name := range nested {
		value, ok := envelope.Result[name].(map[string]any)
		if !ok {
			t.Errorf("%s: result has no %q object", filename, name)
			continue
		}
		var child object
		if err := json.Unmarshal(root.Properties[name], &child); err != nil {
			t.Fatal(err)
		}
		check(name, child, value)
	}
}
