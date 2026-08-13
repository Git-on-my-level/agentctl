package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/output"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

func (a *app) dataCommand(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	if len(args) == 0 {
		return output.NewError(output.CodeUsage, "data requires inventory or cleanup", false)
	}
	switch args[0] {
	case "inventory":
		if len(args) != 1 {
			return output.NewError(output.CodeUsage, "usage: agentctl data inventory", false)
		}
		return a.dataInventory(ctx, renderer, c)
	case "cleanup":
		return a.dataCleanup(ctx, renderer, c, args[1:])
	default:
		return output.NewError(output.CodeUsage, "data requires inventory or cleanup", false).WithDetail("operation", args[0])
	}
}

func (a *app) dataInventory(ctx context.Context, renderer output.Renderer, c common) *output.Error {
	journal, problem := a.openRead(c)
	if problem != nil {
		return problem
	}
	defer journal.Close()
	inventory, err := journal.Inventory(ctx)
	if err != nil {
		return mapStoreError("inventory journal data", err)
	}
	now := time.Now().UTC()
	if a.now != nil {
		now = a.now().UTC()
	}
	cutoff := now.Add(-30 * 24 * time.Hour).Format(time.RFC3339)
	action := output.NextAction{
		Label:   "Preview terminal execution cleanup before a cutoff",
		Argv:    []string{"agentctl", "data", "cleanup", "--before", cutoff, "--plan"},
		Mutates: false, SideEffectClass: output.ReadOnly, Preconditions: []string{"choose an appropriate retention cutoff"},
	}
	lines := []output.Line{
		{Lead: "data.inventory", Fields: []output.Field{{Name: "journal", Value: inventory.JournalPath}, {Name: "file_bytes", Value: inventory.FileBytes}, {Name: "logical_bytes", Value: inventory.LogicalBytes}}},
		{Lead: "executions", Fields: []output.Field{{Name: "total", Value: inventory.Executions.Total}, {Name: "terminal", Value: inventory.Executions.Terminal}, {Name: "nonterminal", Value: inventory.Executions.NonTerminal}, {Name: "events", Value: inventory.Executions.Events}, {Name: "outcomes", Value: inventory.Executions.Outcomes}, {Name: "result_bytes", Value: inventory.Executions.ResultBytes}}},
	}
	if err := renderer.Success(output.Success{Result: inventory, Lines: lines, NextActions: []output.NextAction{action}}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func (a *app) dataCleanup(ctx context.Context, renderer output.Renderer, c common, args []string) *output.Error {
	var beforeRaw, planDigest string
	var planOnly, apply bool
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--before":
			i++
			if i >= len(args) {
				return output.NewError(output.CodeUsage, "--before requires an RFC3339 timestamp", false)
			}
			beforeRaw = strings.TrimSpace(args[i])
		case "--plan":
			planOnly = true
		case "--apply":
			apply = true
		case "--plan-digest":
			i++
			if i >= len(args) {
				return output.NewError(output.CodeUsage, "--plan-digest requires the digest returned by --plan", false)
			}
			planDigest = strings.TrimSpace(args[i])
		default:
			return output.NewError(output.CodeUsage, "unknown data cleanup flag", false).WithDetail("flag", args[i])
		}
	}
	if beforeRaw == "" {
		return output.NewError(output.CodeUsage, "data cleanup requires --before with an RFC3339 timestamp", false)
	}
	if planOnly == apply {
		return output.NewError(output.CodeUsage, "data cleanup requires exactly one of --plan or --apply", false)
	}
	if planOnly && planDigest != "" {
		return output.NewError(output.CodeUsage, "--plan-digest is only valid with --apply", false)
	}
	if apply && planDigest == "" {
		return output.NewError(output.CodeUsage, "data cleanup --apply requires --plan-digest from the reviewed plan", false)
	}
	before, err := time.Parse(time.RFC3339, beforeRaw)
	if err != nil {
		return output.Wrap(output.CodeUsage, "invalid --before timestamp; use RFC3339", false, err).WithDetail("before", beforeRaw)
	}
	before = before.UTC()
	var plan store.CleanupPlan
	if planOnly {
		journal, problem := a.openRead(c)
		if problem != nil {
			return problem
		}
		defer journal.Close()
		plan, err = journal.PlanCleanup(ctx, before)
	} else {
		path, pathErr := a.journalPath(c)
		if pathErr != nil {
			return output.Wrap(output.CodeInternal, "resolve journal path", false, pathErr)
		}
		// Cleanup must not create an empty journal as a side effect.
		if _, statErr := os.Stat(path); statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return output.Wrap(output.CodeNotFound, "open journal", false, store.ErrNotFound)
			}
			return output.Wrap(output.CodeInternal, "stat journal", false, statErr)
		}
		journal, openErr := store.Open(path, store.Options{})
		if openErr != nil {
			return mapStoreError("open journal", openErr)
		}
		defer journal.Close()
		plan, err = journal.ApplyCleanup(ctx, before, planDigest)
	}
	if err != nil {
		return mapStoreError("cleanup journal data", err)
	}
	lines := []output.Line{
		{Lead: "data.cleanup", Fields: []output.Field{{Name: "mode", Value: cleanupMode(planOnly)}, {Name: "before", Value: before}, {Name: "eligible_executions", Value: len(plan.Eligible)}, {Name: "protected_executions", Value: len(plan.Protected)}, {Name: "records", Value: plan.Records.Total()}, {Name: "logical_bytes", Value: plan.LogicalBytes}, {Name: "plan_digest", Value: plan.PlanDigest}}},
	}
	warnings := []output.Warning{{Code: "journal_not_compacted", Message: "logical bytes describe selected records; the journal file is not compacted automatically"}}
	actions := []output.NextAction{}
	if planOnly {
		actions = append(actions, output.NextAction{
			Label:   "Apply this cleanup policy after reviewing eligible IDs",
			Argv:    []string{"agentctl", "data", "cleanup", "--before", before.Format(time.RFC3339), "--apply", "--plan-digest", plan.PlanDigest},
			Mutates: true, SideEffectClass: output.LocalOperationalWrite,
			Preconditions: []string{"review eligible execution IDs", "journal references may change before apply; apply recomputes atomically"},
		})
	}
	if err := renderer.Success(output.Success{Result: plan, Warnings: warnings, Lines: lines, NextActions: actions}); err != nil {
		return output.Wrap(output.CodeInternal, "write output", false, err)
	}
	return nil
}

func cleanupMode(planOnly bool) string {
	if planOnly {
		return "plan"
	}
	return "apply"
}
