package anxrun

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestExplicitBaseURLWinsOverWorkspaceAliases(t *testing.T) {
	for _, adapter := range []string{"codex", "multica"} {
		for _, tc := range []struct{ name, pinned, host string }{
			{"directory-rule", "", ""},
			{"host-alias", "", "host"},
			{"pinned-alias", "omi", "host"},
		} {
			t.Run(adapter+"/"+tc.name, func(t *testing.T) {
				runner := &fakeRunner{}
				sender := &CLISender{options: CLIOptions{BaseURL: "https://explicit.test", WorkspaceAlias: tc.host}, run: runner.run}
				r := testRegistration("endpoint", PhaseTerminal, 1)
				r.Adapter, r.WorkspaceAlias, r.CWD = adapter, tc.pinned, "/omi-cwd"
				if adapter == "multica" {
					r.NativeSession, r.CWD = nil, ""
				}
				prepared, err := sender.Prepare(context.Background(), r)
				if err != nil || prepared.WorkspaceAlias != tc.pinned || len(runner.argvs) != 0 {
					t.Fatalf("explicit endpoint attempted workspace discovery: %+v %v", prepared, err)
				}
				if err := sender.Send(context.Background(), prepared); err != nil {
					t.Fatal(err)
				}
				for _, argv := range runner.argvs {
					joined := strings.Join(argv, " ")
					if !strings.Contains(joined, "--base-url https://explicit.test") || strings.Contains(joined, "--workspace") {
						t.Fatalf("alias can override explicit endpoint: %v", argv)
					}
				}
			})
		}
	}
}

func TestNoCWDUsesConfiguredDefaultAndPersistsBeforeSend(t *testing.T) {
	for _, adapter := range []string{"codex", "multica"} {
		t.Run(adapter, func(t *testing.T) {
			spool := testSpool(t)
			r := testRegistration("no-cwd", PhaseTerminal, 0)
			r.CWD, r.Adapter = "", adapter
			if adapter == "multica" {
				r.NativeSession = nil
			}
			if _, err := spool.Enqueue(r); err != nil {
				t.Fatal(err)
			}
			runner := &fakeRunner{}
			selections := 0
			sender := &CLISender{Resolve: fixtureResolve(r), run: func(ctx context.Context, argv []string, body []byte) ([]byte, error) {
				joined := strings.Join(argv, " ")
				if strings.Contains(joined, "config workspaces") {
					selections++
					// ANX selected a directory rule for the flush worker. The run
					// has no cwd, so only the configured default is authoritative.
					return []byte(`{"ok":true,"result":{"default":"https://default.test/","base_url":"https://worker.test","workspaces":[{"alias":"team_ops","base_url":"https://default.test"},{"alias":"worker","base_url":"https://worker.test"}]}}`), nil
				}
				entries, err := spool.ListPending()
				if err != nil || len(entries) != 1 || entries[0].Registration.WorkspaceAlias != "team_ops" {
					t.Fatalf("default not persisted before send: %+v %v", entries, err)
				}
				if !strings.Contains(joined, "--workspace team_ops") {
					t.Fatalf("wrong default route: %v", argv)
				}
				return runner.run(ctx, argv, body)
			}}
			result, err := spool.Flush(context.Background(), sender, 0)
			if err != nil || result.Delivered != 1 || selections != 1 {
				t.Fatalf("flush=%+v selections=%d err=%v", result, selections, err)
			}
		})
	}
}

func TestWorkspaceCatalogDefaultWithoutCWD(t *testing.T) {
	for _, tc := range []struct{ name, defaultTarget, selected, selectionError, want string }{
		{"url", "https://default.test/", "https://worker.test", "", "team_ops"},
		{"alias", "team_ops", "https://worker.test", "", "team_ops"},
		{"worker-selection-error", "team_ops", "", "worker rule unresolved", "team_ops"},
		{"no-default", "", "https://worker.test", "", ""},
		{"unknown-default", "missing", "https://worker.test", "", ""},
		{"unaliased-default", "https://missing.test", "https://worker.test", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(map[string]any{"ok": true, "result": map[string]any{
				"default": tc.defaultTarget, "base_url": tc.selected, "selection_error": tc.selectionError,
				"workspaces": []map[string]string{{"alias": "team_ops", "base_url": "https://default.test"}, {"alias": "worker", "base_url": "https://worker.test"}},
			}})
			if err != nil {
				t.Fatal(err)
			}
			got, err := workspaceFromCatalog(raw, true)
			if got != tc.want || (err == nil) != (tc.want != "") {
				t.Fatalf("got %q %v want %q", got, err, tc.want)
			}
		})
	}
}

func TestANXAliasGrammarThroughPrepareAndSend(t *testing.T) {
	for _, alias := range []string{"team_ops", strings.Repeat("a", 128)} {
		t.Run(alias, func(t *testing.T) {
			runner := &fakeRunner{}
			sender := &CLISender{run: func(ctx context.Context, argv []string, body []byte) ([]byte, error) {
				if strings.Contains(strings.Join(argv, " "), "config workspaces") {
					return []byte(`{"ok":true,"result":{"base_url":"https://workspace.test","workspaces":[{"alias":"` + alias + `","base_url":"https://workspace.test"}]}}`), nil
				}
				if !strings.Contains(strings.Join(argv, " "), "--workspace "+alias) {
					t.Fatalf("alias not forwarded: %v", argv)
				}
				return runner.run(ctx, argv, body)
			}}
			r := testRegistration("alias", PhaseStart, 1)
			r.CWD = "/fixture"
			prepared, err := sender.Prepare(context.Background(), r)
			if err != nil || prepared.WorkspaceAlias != alias {
				t.Fatalf("alias rejected: %+v %v", prepared, err)
			}
			if err := sender.Send(context.Background(), prepared); err != nil {
				t.Fatal(err)
			}
		})
	}
}
