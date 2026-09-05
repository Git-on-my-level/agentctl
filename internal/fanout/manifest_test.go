package fanout

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Git-on-my-level/agentctl/internal/ids"
)

func TestDecodeSharedAndDistinctPrompts(t *testing.T) {
	for _, body := range []string{
		`{"schema_version":1,"prompt_file":"shared.md","children":[{"argv":["agent","--native-flag",""]}]}`,
		`{"schema_version":1,"children":[{"name":"review","prompt_file":"review.md","argv":["codex","exec","--json"]},{"name":"implement","prompt_file":"implement.md","argv":["claude"]}]}`,
		`{"schema_version":1,"prompt_file":"shared.md","prompt_delivery":"argv","labels":["batch-one"],"children":[{"prompt_file":"override.md","prompt_delivery":"stdin","labels":["batch-one","review"],"argv":["agent"]}]}`,
	} {
		m, err := Decode([]byte(body))
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("decoded invalid manifest: %v", err)
		}
	}
}

func TestDecodeRejectsAmbiguousOrInvalidJSON(t *testing.T) {
	valid := `{"schema_version":1,"prompt_file":"task.md","children":[{"argv":["agent"]}]}`
	for name, body := range map[string]string{
		"trailing_document":     valid + `{}`,
		"trailing_garbage":      valid + `garbage`,
		"unknown_field":         strings.Replace(valid, `"schema_version":1`, `"schema_version":1,"typo":true`, 1),
		"duplicate_key":         strings.Replace(valid, `"schema_version":1`, `"schema_version":2,"schema_version":1`, 1),
		"duplicate_case_alias":  strings.Replace(valid, `"schema_version":1`, `"Schema_Version":2,"schema_version":1`, 1),
		"nested_duplicate":      strings.Replace(valid, `"argv":["agent"]`, `"argv":["first"],"argv":["agent"]`, 1),
		"case_alias":            strings.Replace(valid, `"schema_version"`, `"SCHEMA_VERSION"`, 1),
		"empty_optional_string": strings.Replace(valid, `"argv":["agent"]`, `"argv":["agent"],"name":""`, 1),
		"null_manifest":         `null`,
		"null_child":            strings.Replace(valid, `{"argv":["agent"]}`, `null`, 1),
		"null_argv":             strings.Replace(valid, `["agent"]`, `[null]`, 1),
		"null_optional":         strings.Replace(valid, `"argv":["agent"]`, `"argv":["agent"],"no_store_result":null`, 1),
		"invalid_utf8":          valid[:len(valid)-1] + string([]byte{0xff}) + `}`,
		"oversized":             strings.Repeat(" ", MaxManifestBytes+1),
		"excessive_depth":       strings.Repeat("[", 20) + `1` + strings.Repeat("]", 20),
		"non_object":            `[]`,
		"malformed":             `{"schema_version":1`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Decode([]byte(body)); err == nil {
				t.Fatal("accepted invalid or ambiguous JSON")
			}
		})
	}
	if _, err := Decode([]byte(valid + " \n\t")); err != nil {
		t.Fatalf("trailing whitespace must be accepted: %v", err)
	}
}

func TestValidateChildControls(t *testing.T) {
	rawID, err := ids.New(ids.TypeExecution)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*Manifest){
		"missing_prompt":       func(m *Manifest) { m.PromptFile = "" },
		"blank_prompt":         func(m *Manifest) { m.PromptFile = " " },
		"blank_child_prompt":   func(m *Manifest) { m.Children[0].PromptFile = " " },
		"bad_version":          func(m *Manifest) { m.SchemaVersion = 2 },
		"no_children":          func(m *Manifest) { m.Children = nil },
		"too_many_children":    func(m *Manifest) { m.Children = make([]Child, MaxChildren+1) },
		"negative_concurrency": func(m *Manifest) { m.Concurrency = -1 },
		"excess_concurrency":   func(m *Manifest) { m.Concurrency = 17 },
		"delivery":             func(m *Manifest) { m.PromptDelivery = "auto" },
		"child_delivery":       func(m *Manifest) { m.Children[0].PromptDelivery = "auto" },
		"no_argv":              func(m *Manifest) { m.Children[0].Argv = nil },
		"blank_executable":     func(m *Manifest) { m.Children[0].Argv[0] = " " },
		"nul_argv":             func(m *Manifest) { m.Children[0].Argv = []string{"agent", "--flag=\x00"} },
		"too_many_args":        func(m *Manifest) { m.Children[0].Argv = append([]string{"agent"}, make([]string, 256)...) },
		"bad_timeout":          func(m *Manifest) { m.Children[0].Timeout = "eventually" },
		"zero_timeout":         func(m *Manifest) { m.Children[0].Timeout = "0s" },
		"bad_id":               func(m *Manifest) { m.Children[0].ExecutionID = "exec-not-valid" },
		"duplicate_ids": func(m *Manifest) {
			m.Children[0].ExecutionID = rawID.String()
			m.Children = append(m.Children, m.Children[0])
		},
		"invalid_name": func(m *Manifest) { m.Children[0].Name = "review task" },
		"duplicate_names": func(m *Manifest) {
			m.Children[0].Name = "review"
			m.Children = append(m.Children, m.Children[0])
		},
		"bad_labels":             func(m *Manifest) { m.Labels = []string{"bad label"} },
		"duplicate_labels":       func(m *Manifest) { m.Labels = []string{"x", "x"} },
		"duplicate_child_labels": func(m *Manifest) { m.Children[0].Labels = []string{"x", "x"} },
		"too_many_combined_labels": func(m *Manifest) {
			for i := 0; i < MaxLabels; i++ {
				m.Labels = append(m.Labels, fmt.Sprintf("batch-%d", i))
			}
			m.Children[0].Labels = []string{"extra"}
		},
	} {
		t.Run(name, func(t *testing.T) {
			m := Manifest{SchemaVersion: 1, PromptFile: "task.md", Children: []Child{{Argv: []string{"agent"}}}}
			mutate(&m)
			if err := m.Validate(); err == nil {
				t.Fatal("accepted invalid manifest")
			}
		})
	}
}

func TestMergeLabelsStableAndDoesNotMutateInput(t *testing.T) {
	shared, child := []string{"batch", "shared"}, []string{"shared", "review"}
	got := MergeLabels(shared, child)
	if !reflect.DeepEqual(got, []string{"batch", "shared", "review"}) {
		t.Fatalf("labels=%v", got)
	}
	got[0] = "changed"
	if shared[0] != "batch" || child[0] != "shared" {
		t.Fatal("mutated input")
	}
}

func TestDecodeDoesNotEchoNativeArgumentValues(t *testing.T) {
	body := []byte(`{"schema_version":1,"prompt_file":"task.md","children":[{"argv":["agent","secret-value\u0000"]}]}`)
	_, err := Decode(body)
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unbounded or missing diagnostic: %v", err)
	}
}

func FuzzDecode(f *testing.F) {
	f.Add([]byte(`{"schema_version":1,"prompt_file":"task.md","children":[{"argv":["agent"]}]}`))
	f.Add([]byte(`{"schema_version":1,"children":[{"prompt_file":"a.md","argv":["agent"]}]}`))
	f.Add([]byte(`{"schema_version":1,"schema_version":2}`))
	f.Fuzz(func(t *testing.T, body []byte) {
		m, err := Decode(body)
		if err != nil {
			return
		}
		if err := m.Validate(); err != nil {
			t.Fatalf("accepted invalid manifest: %v", err)
		}
		canonical, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := Decode(bytes.TrimSpace(canonical)); err != nil {
			t.Fatalf("round trip: %v", err)
		}
	})
}
