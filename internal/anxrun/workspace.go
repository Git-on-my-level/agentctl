package anxrun

import (
	"context"
	"encoding/json"
	"sort"
	"strings"
)

// Prepare pins routing before the first remote call. Flush persists this result
// before Send so retries and dead-letter recovery retain the same workspace.
// Legacy registrations recover cwd from the private journal when available.
func (s *CLISender) Prepare(ctx context.Context, r Registration) (Registration, error) {
	if r.WorkspaceAlias != "" {
		return r, nil
	}
	if s.options.WorkspaceAlias != "" {
		r.WorkspaceAlias = s.options.WorkspaceAlias
	} else {
		cwd := r.CWD
		if s.Resolve != nil {
			resolved, _, err := s.Resolve(ctx, r)
			if err != nil {
				return r, err
			}
			cwd = resolved.CWD
		}
		// Never resolve against the flush worker's unrelated current directory.
		if cwd == "" {
			return r, &SendError{Code: "protocol_error"}
		}
		options := s.options.normalized()
		argv := []string{options.Executable, "--json"}
		if options.ConfigDir != "" {
			argv = append(argv, "--config-dir", options.ConfigDir)
		}
		argv = append(argv, "--timeout", options.Timeout.String(), "config", "workspaces")
		callCtx, cancel := context.WithTimeout(ctx, options.Timeout)
		defer cancel()
		var raw []byte
		var err error
		if s.run != nil {
			raw, err = s.run(callCtx, argv, nil)
		} else {
			raw, err = runAnxProcessAt(callCtx, argv, nil, cwd, true)
		}
		if callCtx.Err() != nil {
			return r, callCtx.Err()
		}
		if err != nil {
			return r, &SendError{NumericCode: processCode(err)}
		}
		alias, err := workspaceFromCatalog(raw)
		if err != nil {
			return r, err
		}
		r.WorkspaceAlias = alias
	}
	if code := workspaceAliasCode(r.WorkspaceAlias); code != 0 {
		return r, egressError(code)
	}
	return r, nil
}

// ANX owns directory matching (including symlinks and filesystem case rules).
// Its local config command returns the selected URL and discovered aliases.
// Multiple aliases for that URL are equivalent: choose lexical order.
func workspaceFromCatalog(raw []byte) (string, error) {
	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			BaseURL        string `json:"base_url"`
			SelectionError string `json:"selection_error"`
			Workspaces     []struct {
				Alias   string `json:"alias"`
				BaseURL string `json:"base_url"`
			} `json:"workspaces"`
		} `json:"result"`
	}
	if json.Unmarshal(raw, &response) != nil || !response.OK || response.Result.BaseURL == "" || response.Result.SelectionError != "" {
		return "", &SendError{Code: "protocol_error"}
	}
	aliases := []string{}
	for _, workspace := range response.Result.Workspaces {
		if strings.TrimRight(workspace.BaseURL, "/") == strings.TrimRight(response.Result.BaseURL, "/") && ValidWorkspaceAlias(workspace.Alias) {
			aliases = append(aliases, workspace.Alias)
		}
	}
	if len(aliases) == 0 {
		return "", &SendError{Code: "protocol_error"}
	}
	sort.Strings(aliases)
	return aliases[0], nil
}
