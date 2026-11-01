// Package nativecatalog asks installed native CLIs which configured models and
// settings they offer now. It runs only on an explicit request: each probe
// starts a native CLI, and none sends a prompt or runs a model turn.
package nativecatalog

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

const (
	StatusAvailable   = "available"
	StatusUnavailable = "unavailable"
	StatusUnverified  = "unverified"
	StatusPartial     = "partial"
	StatusEmpty       = "not_configured"

	probeTimeout  = 30 * time.Second
	maxProbeBytes = 16 << 20
)

// Entry is one configured tuple to check. Executable is the configured
// override or the harness default.
type Entry struct {
	Index      int
	Harness    string
	Model      string
	Effort     string
	Executable string
}

// Check reports one Entry. An unverified entry is neither available nor
// missing: no reviewed catalog exists, or the probe could not answer.
type Check struct {
	Index          int    `json:"index"`
	Harness        string `json:"harness"`
	Model          string `json:"model"`
	Effort         string `json:"effort,omitempty"`
	Status         string `json:"status"`
	DiagnosticCode string `json:"diagnostic_code,omitempty"`
	Diagnostic     string `json:"diagnostic,omitempty"`
}

// Report is the live_catalog member of config doctor --live.
type Report struct {
	Status string   `json:"status"`
	Checks []Check  `json:"checks"`
	Limits []string `json:"limits"`
}

// catalog is what one harness offers. A nil efforts function means the harness
// has no effort catalog to compare against.
type catalog struct {
	models  map[string]bool
	efforts func(model string) map[string]bool
}

// prober reads one harness catalog through its executable.
type prober func(ctx context.Context, executable string) (catalog, error)

var probers = map[string]prober{
	"codex":  probeCodex,
	"cursor": probeCursor,
	"devin":  probeDevin,
}

var unprobed = map[string]string{
	"devin-cloud": "reading cloud options opens a remote session",
	"claude-code": "no reviewed native model catalog",
	"zcode":       "no reviewed native model catalog",
}

// CheckEntries probes each harness once and compares every entry with its catalog.
func CheckEntries(ctx context.Context, entries []Entry, lookPath func(string) (string, error)) Report {
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	report := Report{Status: StatusEmpty, Checks: []Check{}, Limits: []string{
		"each probed CLI is started once; no prompt is sent and no model turn runs",
		"a Devin probe opens one idle local ACP session and then stops it",
		"unverified means no reviewed catalog exists or the probe could not answer, such as a locked keychain in an SSH session",
		"availability is for this host, account and CLI version only; Multica runtimes are not checked",
	}}
	type key struct{ harness, executable string }
	type probed struct {
		catalog catalog
		err     error
	}
	cache := map[key]probed{}
	for _, entry := range entries {
		check := Check{Index: entry.Index, Harness: entry.Harness, Model: entry.Model, Effort: entry.Effort}
		switch resolved, err := lookPath(entry.Executable); {
		case err != nil:
			check.Status, check.DiagnosticCode = StatusUnavailable, "adapter_missing"
			check.Diagnostic = fmt.Sprintf("executable %q is not installed on this host", entry.Executable)
		case unprobed[entry.Harness] != "":
			check.Status, check.DiagnosticCode, check.Diagnostic = StatusUnverified, "catalog_unsupported", unprobed[entry.Harness]
		case probers[entry.Harness] == nil:
			check.Status, check.DiagnosticCode, check.Diagnostic = StatusUnverified, "catalog_unsupported", "no reviewed native model catalog"
		default:
			k := key{entry.Harness, resolved}
			result, seen := cache[k]
			if !seen {
				probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
				result.catalog, result.err = probers[entry.Harness](probeCtx, resolved)
				cancel()
				cache[k] = result
			}
			compare(&check, result.catalog, result.err)
		}
		report.Checks = append(report.Checks, check)
	}
	report.Status = summarize(report.Checks)
	return report
}

func compare(check *Check, found catalog, err error) {
	switch {
	case err != nil:
		check.Status, check.DiagnosticCode, check.Diagnostic = StatusUnverified, "catalog_probe_failed", err.Error()
	case !found.models[check.Model]:
		check.Status, check.DiagnosticCode = StatusUnavailable, "model_unavailable"
		check.Diagnostic = "the native catalog does not list this model"
	case check.Effort != "" && found.efforts != nil && !found.efforts(check.Model)[check.Effort]:
		check.Status, check.DiagnosticCode = StatusUnavailable, "effort_unavailable"
		check.Diagnostic = "the native catalog does not list this effort for this model"
	default:
		check.Status = StatusAvailable
	}
}

func summarize(checks []Check) string {
	if len(checks) == 0 {
		return StatusEmpty
	}
	status := StatusAvailable
	for _, check := range checks {
		switch check.Status {
		case StatusUnavailable:
			return StatusUnavailable
		case StatusUnverified:
			status = StatusPartial
		}
	}
	return status
}

// runOutput never returns native output in its error: a failed CLI may print
// account details.
func runOutput(ctx context.Context, executable string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, executable, args...)
	cmd.WaitDelay = 2 * time.Second
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("native catalog command failed: %s", probeFailure(ctx, err))
	}
	if len(out) > maxProbeBytes {
		return nil, errors.New("native catalog output is too large")
	}
	return out, nil
}

func probeFailure(ctx context.Context, err error) string {
	if ctx.Err() != nil {
		return "timed out"
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return fmt.Sprintf("exit code %d", exit.ExitCode())
	}
	return "could not start"
}

func probeCodex(ctx context.Context, executable string) (catalog, error) {
	out, err := runOutput(ctx, executable, "debug", "models")
	if err != nil {
		return catalog{}, err
	}
	var parsed struct {
		Models []struct {
			Slug   string `json:"slug"`
			Levels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
		} `json:"models"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed.Models) == 0 {
		return catalog{}, errors.New("native catalog output has no models")
	}
	found := catalog{models: map[string]bool{}}
	efforts := map[string]map[string]bool{}
	for _, model := range parsed.Models {
		found.models[model.Slug] = true
		efforts[model.Slug] = map[string]bool{}
		for _, level := range model.Levels {
			efforts[model.Slug][level.Effort] = true
		}
	}
	found.efforts = func(model string) map[string]bool { return efforts[model] }
	return found, nil
}

// Cursor lists models as "id - Display name" lines under a heading.
func probeCursor(ctx context.Context, executable string) (catalog, error) {
	out, err := runOutput(ctx, executable, "--list-models")
	if err != nil {
		return catalog{}, err
	}
	found := catalog{models: map[string]bool{}}
	for _, line := range strings.Split(string(out), "\n") {
		if id, _, ok := strings.Cut(strings.TrimSpace(line), " - "); ok && !strings.ContainsAny(id, " \t") {
			found.models[id] = true
		}
	}
	if len(found.models) == 0 {
		return catalog{}, errors.New("native catalog output has no models")
	}
	return found, nil
}

type acpOptionValue struct {
	Value   string           `json:"value"`
	Options []acpOptionValue `json:"options"`
}

// probeDevin reads the session options that a Devin ACP launch verifies
// against. `devin models list` can show ids that ACP refuses.
func probeDevin(ctx context.Context, executable string) (catalog, error) {
	cmd := exec.CommandContext(ctx, executable, "acp")
	cmd.WaitDelay = 2 * time.Second
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return catalog{}, errors.New("native catalog command could not start")
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return catalog{}, errors.New("native catalog command could not start")
	}
	if err := cmd.Start(); err != nil {
		return catalog{}, errors.New("native catalog command could not start")
	}
	defer func() {
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	}()
	// A kill on timeout ends the read below.
	type answer struct {
		options map[string]map[string]bool
		err     error
	}
	done := make(chan answer, 1)
	go func() {
		// The temp directory keeps the idle session out of the project's list.
		options, err := readDevinOptions(stdin, stdout, os.TempDir())
		done <- answer{options, err}
	}()
	select {
	case got := <-done:
		if got.err != nil {
			return catalog{}, got.err
		}
		if len(got.options["model"]) == 0 {
			return catalog{}, errors.New("native catalog output has no models")
		}
		found := catalog{models: got.options["model"]}
		found.efforts = func(string) map[string]bool { return got.options["thought_level"] }
		return found, nil
	case <-ctx.Done():
		return catalog{}, errors.New("native catalog command failed: timed out")
	}
}

func readDevinOptions(stdin io.Writer, stdout io.Reader, cwd string) (map[string]map[string]bool, error) {
	reader := bufio.NewScanner(stdout)
	reader.Buffer(make([]byte, 64<<10), maxProbeBytes)
	request := func(id int, method string, params any) (json.RawMessage, error) {
		line, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
		if err != nil {
			return nil, errors.New("native catalog request failed")
		}
		if _, err := stdin.Write(append(line, '\n')); err != nil {
			return nil, errors.New("native catalog request failed")
		}
		for reader.Scan() {
			var message struct {
				ID     *int            `json:"id"`
				Result json.RawMessage `json:"result"`
				Error  json.RawMessage `json:"error"`
			}
			if json.Unmarshal(reader.Bytes(), &message) != nil || message.ID == nil || *message.ID != id {
				continue
			}
			if len(message.Error) > 0 || len(message.Result) == 0 {
				return nil, errors.New("native catalog request was refused")
			}
			return message.Result, nil
		}
		return nil, errors.New("native catalog command ended before answering")
	}
	if _, err := request(1, "initialize", map[string]any{"protocolVersion": 1, "clientCapabilities": map[string]any{}}); err != nil {
		return nil, err
	}
	result, err := request(2, "session/new", map[string]any{"cwd": cwd, "mcpServers": []any{}})
	if err != nil {
		return nil, err
	}
	var session struct {
		Options []struct {
			ID      string           `json:"id"`
			Options []acpOptionValue `json:"options"`
		} `json:"configOptions"`
	}
	if json.Unmarshal(result, &session) != nil {
		return nil, errors.New("native catalog output has no models")
	}
	options := map[string]map[string]bool{}
	for _, option := range session.Options {
		values := map[string]bool{}
		for _, value := range option.Options {
			if value.Value != "" {
				values[value.Value] = true
			}
			for _, nested := range value.Options {
				if nested.Value != "" {
					values[nested.Value] = true
				}
			}
		}
		options[option.ID] = values
	}
	return options, nil
}
