package main

import (
	"path/filepath"

	"github.com/Git-on-my-level/agentctl/internal/anxrun"
	"github.com/Git-on-my-level/agentctl/internal/ids"
)

// Read legacy link evidence even when current registration is disabled. Pending,
// dead and delivered registrations all protect their journal target. Inspect
// both the conventional spool and a configured override without creating either.
func (a *app) anxRetentionLinks(c common) ([]ids.ExecutionID, error) {
	path, err := a.journalPath(c)
	if err != nil {
		return nil, err
	}
	roots := []string{filepath.Join(filepath.Dir(path), "anx-run-links")}
	if settings, ok := a.anxSettings(c); ok && settings.SpoolDir != roots[0] {
		roots = append(roots, settings.SpoolDir)
	}
	var links []ids.ExecutionID
	for _, root := range roots {
		spool, present, err := anxrun.OpenSpoolIfPresent(root)
		if err != nil {
			return nil, err
		}
		if !present {
			continue
		}
		ids, err := spool.LinkedExecutions()
		if err != nil {
			return nil, err
		}
		links = append(links, ids...)
	}
	return links, nil
}
