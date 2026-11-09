package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/Git-on-my-level/agentctl/internal/ids"
	"github.com/Git-on-my-level/agentctl/internal/store"
)

const journalRetentionAge = 30 * 24 * time.Hour

// This aggregate report is replaced atomically on each preview pass. It never
// contains identifiers, prompts, results or diagnostic paths.
type journalRetentionReport struct {
	SchemaVersion     int                       `json:"schema_version"`
	Mode              string                    `json:"mode"`
	GeneratedAt       time.Time                 `json:"generated_at"`
	EnforceAt         time.Time                 `json:"enforce_at"`
	Before            time.Time                 `json:"before"`
	Records           store.CleanupRecordCounts `json:"would_delete"`
	DiagnosticFiles   int                       `json:"diagnostic_files"`
	DiagnosticBytes   int64                     `json:"diagnostic_bytes"`
	Oldest            *time.Time                `json:"oldest_terminal_at"`
	Newest            *time.Time                `json:"newest_terminal_at"`
	ProtectedByReason map[string]int            `json:"protected_by_reason"`
}

func writeRetentionReport(path string, plan store.CleanupPlan, now, enforceAt time.Time) error {
	report := journalRetentionReport{SchemaVersion: 1, Mode: "plan_only", GeneratedAt: now.UTC(), EnforceAt: enforceAt, Before: plan.Before, Records: plan.Records, DiagnosticFiles: plan.DiagnosticFiles, DiagnosticBytes: plan.DiagnosticBytes, ProtectedByReason: map[string]int{}}
	for _, execution := range plan.Eligible {
		at := execution.TerminalAt
		if report.Oldest == nil || at.Before(*report.Oldest) {
			report.Oldest = &at
		}
		if report.Newest == nil || at.After(*report.Newest) {
			report.Newest = &at
		}
	}
	for _, execution := range plan.Protected {
		for _, reason := range execution.Reasons {
			report.ProtectedByReason[reason]++
		}
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".retention-report-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(append(data, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

// Only the explicitly running supervisor performs retention. Read-only CLI
// commands never initialize policy or maintain the journal as a side effect.
func maintainJournal(ctx context.Context, path string, now time.Time, immediate bool, links []ids.ExecutionID, log io.Writer) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	read, err := openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: true}, true, 250*time.Millisecond)
	if err != nil {
		return 0, err
	}
	enforceAt, err := read.RetentionEnforceAt(ctx)
	if err != nil {
		read.Close()
		return 0, err
	}
	if enforceAt.IsZero() {
		// Initialize an upgraded journal only from maintenance's writable phase.
		read.Close()
		write, err := openJournalWithBusyBudget(ctx, path, store.Options{Clock: func() time.Time { return now }}, true, 250*time.Millisecond)
		if err != nil {
			return 0, err
		}
		enforceAt, err = write.RetentionEnforceAt(ctx)
		write.Close()
		if err != nil {
			return 0, err
		}
		read, err = openJournalWithBusyBudget(ctx, path, store.Options{ReadOnly: true}, true, 250*time.Millisecond)
		if err != nil {
			return 0, err
		}
	}
	if !immediate && (enforceAt.IsZero() || now.Before(enforceAt)) {
		// Preview the entire eligible set, not just the first bounded batch.
		plan, err := read.PlanCleanup(ctx, now.Add(-journalRetentionAge), store.CleanupOptions{ANXRunLinks: links})
		read.Close()
		if err != nil {
			return 0, err
		}
		reportPath := path + ".retention-report.json"
		if err := writeRetentionReport(reportPath, plan, now, enforceAt); err != nil {
			return 0, err
		}
		if log != nil {
			fmt.Fprintf(log, "agentctl: journal retention plan-only: would delete %d executions, %d outcomes, %d events; report=%s enforce_at=%s\n", plan.Records.Executions, plan.Records.Outcomes, plan.Records.Events, reportPath, enforceAt.Format(time.RFC3339Nano))
		}
		return 0, nil
	}
	batch, err := read.PlanCleanupBatch(ctx, now.Add(-journalRetentionAge), links...)
	read.Close()
	if err != nil || batch.Executions() == 0 {
		return 0, err
	}
	write, err := openJournalWithBusyBudget(ctx, path, store.Options{}, true, 250*time.Millisecond)
	if err != nil {
		return 0, err
	}
	defer write.Close()
	return write.ApplyCleanupBatch(ctx, batch)
}

func journalMaintenanceLog(w io.Writer, removed int, err error) {
	if w == nil {
		return
	}
	if errors.Is(err, store.ErrBusy) || errors.Is(err, store.ErrConflict) {
		return
	}
	if err != nil {
		fmt.Fprintf(w, "agentctl: journal retention deferred: %v\n", err)
	} else if removed > 0 {
		fmt.Fprintf(w, "agentctl: journal retention removed %d terminal executions; pages reusable\n", removed)
	}
}
