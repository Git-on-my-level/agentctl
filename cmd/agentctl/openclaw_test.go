package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenClawDashLeadingPromptAndExactContent(t *testing.T) {
	for _, mode := range []string{"delegate", "run"} {
		for _, source := range []string{"stdin", "file"} {
			for _, prompt := range []string{"- Markdown bullet\n", "---\nfrontmatter\n---\n", "--json", "--"} {
				t.Run(mode+"/"+source+"/"+strings.Split(prompt, "\n")[0], func(t *testing.T) {
					f := newDelegateFixture(t, delegateSuccessScript)
					executable := filepath.Join(f.root, "openclaw")
					capture := filepath.Join(f.root, "argv")
					// NUL-separated capture preserves the exact multiline message argument.
					script := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'OpenClaw fixture'; exit 0; fi\nprintf '%s\\000' \"$@\" > '" + capture + "'\nprintf '%s\\n' '{\"payloads\":[{\"text\":\"  exact answer\\n\\tend  \\n\"}]}'\n"
					if err := os.WriteFile(executable, []byte(script), 0700); err != nil {
						t.Fatal(err)
					}
					profile := map[string]any{"schema_version": 1, "default_profile": "test", "profiles": map[string]any{"test": map[string]any{
						"adapters":          map[string]any{"openclaw": map[string]any{"executable": executable}},
						"agent_preferences": map[string]any{"mode": "advisory", "preferred": []any{map[string]any{"agent": "openclaw", "model": "fixture/model", "default": true}}},
					}}}
					data, err := json.Marshal(profile)
					if err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(f.config, data, 0600); err != nil {
						t.Fatal(err)
					}
					f.selectRequest(t, `{"harness":"openclaw","model":"fixture/model"}`)
					var stdout, stderr bytes.Buffer
					a := testApp(&stdout, &stderr)
					a.stdin = strings.NewReader(prompt)
					args := []string{"--config", f.config, "--journal", f.journal, mode, "--cwd", f.root}
					if mode == "delegate" {
						args = append(args, "--request-file", f.request, "--wait", "--content")
					}
					if source == "stdin" {
						args = append(args, "--prompt-stdin")
					} else {
						promptPath := filepath.Join(f.root, "prompt.md")
						if err := os.WriteFile(promptPath, []byte(prompt), 0600); err != nil {
							t.Fatal(err)
						}
						args = append(args, "--prompt-file", promptPath)
					}
					if mode == "run" {
						args = append(args, "--adapter", "openclaw", "--", executable, "agent", "--local", "--json", "--message")
					}
					if code := a.run(context.Background(), args); code != 0 {
						t.Fatalf("exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
					}
					if mode == "run" {
						var doc struct {
							Result struct {
								ID string `json:"id"`
							} `json:"result"`
						}
						if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
							t.Fatal(err)
						}
						stdout.Reset()
						stderr.Reset()
						if code := a.run(context.Background(), []string{"--config", f.config, "--journal", f.journal, "result", doc.Result.ID, "--content"}); code != 0 {
							t.Fatalf("result exit=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
						}
					}
					if want := "  exact answer\n\tend  \n"; stdout.String() != want {
						t.Fatalf("content=%q want=%q", stdout.String(), want)
					}
					captured, err := os.ReadFile(capture)
					if err != nil {
						t.Fatal(err)
					}
					argv := strings.Split(strings.TrimSuffix(string(captured), "\x00"), "\x00")
					if len(argv) < 2 || argv[len(argv)-1] != prompt || (argv[len(argv)-2] != "-m" && argv[len(argv)-2] != "--message") {
						t.Fatalf("native argv=%q; want exact message value %q", argv, prompt)
					}
				})
			}
		}
	}
}
