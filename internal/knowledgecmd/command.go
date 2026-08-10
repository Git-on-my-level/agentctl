// Package knowledgecmd exposes the knowledge and context command surface as a
// small, side-effect-aware service.  The executable can call Run and render
// the returned result or text lines using its own output protocol.
package knowledgecmd

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	agentcontext "github.com/Git-on-my-level/agentctl/internal/context"
	"github.com/Git-on-my-level/agentctl/internal/knowledge"
)

// IO contains injectable dependencies.  A nil Git runner uses native Git;
// GitRunner is particularly useful for tests and for embedders which already
// own a constrained Git executable.
type IO struct {
	Git       knowledge.GitRunner
	GitRunner knowledge.GitRunner // alias for callers that prefer the interface name
	Now       func() time.Time
	Clock     func() time.Time // alias for callers that model time as a clock
}

// ValidateResult is intentionally metadata-only: validating a source does not
// return the source document or any corpus content.
type ValidateResult struct {
	Valid       bool   `json:"valid"`
	SourceID    string `json:"source_id"`
	Slug        string `json:"slug"`
	Mode        string `json:"mode"`
	Ref         string `json:"ref"`
	Subpath     string `json:"subpath"`
	Sensitivity string `json:"sensitivity"`
}

type SyncResult struct {
	SourceID    string `json:"source_id"`
	CheckoutDir string `json:"checkout_dir"`
	Commit      string `json:"commit"`
	TreeDigest  string `json:"tree_digest"`
	Planned     bool   `json:"planned,omitempty"`
}

type CompileResult struct {
	OutputDir      string `json:"output_dir"`
	BundleRevision string `json:"bundle_revision"`
	SourceCount    int    `json:"source_count"`
	RecordCount    int    `json:"record_count"`
	Planned        bool   `json:"planned,omitempty"`
}

type VerifyResult struct {
	BundleRevision string `json:"bundle_revision"`
	SourceCount    int    `json:"source_count"`
	AssetCount     int    `json:"asset_count"`
}

type InstallResult struct {
	Destination    string `json:"destination"`
	BundleRevision string `json:"bundle_revision"`
	Planned        bool   `json:"planned,omitempty"`
}

// ContextMatch is a bounded provenance summary.  Record text is deliberately
// omitted from the JSON result; callers can request a bounded rendered file.
type ContextMatch struct {
	ID            string   `json:"id"`
	SourceRepoID  string   `json:"source_repo_id"`
	Path          string   `json:"path"`
	StartLine     int      `json:"start_line"`
	EndLine       int      `json:"end_line"`
	Reason        []string `json:"reason"`
	SourceCommit  string   `json:"source_commit"`
	ContentDigest string   `json:"content_digest"`
}

type ContextResult struct {
	BundleRevision string         `json:"bundle_revision"`
	Matches        []ContextMatch `json:"matches"`
	RenderPath     string         `json:"render_path,omitempty"`
	RenderBytes    int            `json:"render_bytes,omitempty"`
}

const defaultRenderMaxBytes = 64 * 1024

// Run executes one command.  The result is a compact typed value; textLines
// are stable key=value lines for a text-mode CLI.  Read commands never invoke
// Git fetch or write a cache.  Sync, compile, and install are the only commands
// which can write, and each accepts --plan to resolve without doing so.
func Run(ctx context.Context, args []string, deps IO) (result any, textLines []string, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if len(args) == 0 {
		return nil, nil, errors.New("command is required")
	}
	switch args[0] {
	case "knowledge":
		if len(args) < 2 {
			return nil, nil, errors.New("knowledge requires validate, sync, compile, verify, or install")
		}
		switch args[1] {
		case "validate":
			return runValidate(args[2:])
		case "sync":
			return runSync(ctx, args[2:], deps)
		case "compile":
			return runCompile(ctx, args[2:], deps)
		case "verify":
			return runVerify(args[2:])
		case "install":
			return runInstall(args[2:])
		default:
			return nil, nil, fmt.Errorf("unknown knowledge command %q", args[1])
		}
	case "context":
		return runContext(args[1:], deps)
	case "validate":
		return runValidate(args[1:])
	case "sync":
		return runSync(ctx, args[1:], deps)
	case "compile":
		return runCompile(ctx, args[1:], deps)
	case "verify":
		return runVerify(args[1:])
	case "install":
		return runInstall(args[1:])
	default:
		return nil, nil, fmt.Errorf("unknown command %q", args[0])
	}
}

type stringList []string

func (s *stringList) String() string { return strings.Join(*s, ",") }
func (s *stringList) Set(v string) error {
	if strings.TrimSpace(v) == "" {
		return errors.New("value must not be empty")
	}
	*s = append(*s, v)
	return nil
}

func flagSet(name string) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(io.Discard)
	return f
}

func runValidate(args []string) (any, []string, error) {
	f := flagSet("knowledge validate")
	source := f.String("source", "", "source registration")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if *source == "" {
		return nil, nil, errors.New("--source is required")
	}
	reg, err := knowledge.ParseSourceRegistrationFile(*source)
	if err != nil {
		return nil, nil, err
	}
	r := ValidateResult{Valid: true, SourceID: reg.ID, Slug: reg.Slug, Mode: string(reg.Mode), Ref: reg.Ref, Subpath: reg.Subpath, Sensitivity: string(reg.Sensitivity)}
	return r, []string{line("knowledge.validate", map[string]any{"valid": true, "source_id": r.SourceID, "mode": r.Mode})}, nil
}

type sourceSpec struct {
	Registration knowledge.SourceRegistration
	CheckoutDir  string
}

func parseSourceSpec(spec string) (sourceSpec, error) {
	file, checkout, ok := strings.Cut(spec, "=")
	if !ok || strings.TrimSpace(file) == "" || strings.TrimSpace(checkout) == "" {
		return sourceSpec{}, errors.New("--source must be FILE=CHECKOUT")
	}
	reg, err := knowledge.ParseSourceRegistrationFile(file)
	if err != nil {
		return sourceSpec{}, err
	}
	return sourceSpec{Registration: reg, CheckoutDir: checkout}, nil
}

func runSync(ctx context.Context, args []string, deps IO) (any, []string, error) {
	f := flagSet("knowledge sync")
	source := f.String("source", "", "source registration")
	checkout := f.String("checkout", "", "checkout directory")
	plan := f.Bool("plan", false, "resolve without writing")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if *source == "" || *checkout == "" {
		return nil, nil, errors.New("--source and --checkout are required")
	}
	reg, err := knowledge.ParseSourceRegistrationFile(*source)
	if err != nil {
		return nil, nil, err
	}
	if *plan {
		r := SyncResult{SourceID: reg.ID, CheckoutDir: *checkout, Planned: true}
		return r, []string{line("knowledge.sync", map[string]any{"planned": true, "source_id": reg.ID, "checkout_dir": *checkout})}, nil
	}
	git := gitRunner(deps)
	if git == nil {
		git = knowledge.NativeGit{}
	}
	r, err := knowledge.SyncSourceWithGit(ctx, git, reg, *checkout)
	if err != nil {
		return nil, nil, err
	}
	out := SyncResult{SourceID: reg.ID, CheckoutDir: r.CheckoutDir, Commit: r.Commit, TreeDigest: r.TreeDigest}
	return out, []string{line("knowledge.sync", map[string]any{"source_id": out.SourceID, "checkout_dir": out.CheckoutDir, "commit": out.Commit, "tree_digest": out.TreeDigest})}, nil
}

func runCompile(ctx context.Context, args []string, deps IO) (any, []string, error) {
	f := flagSet("knowledge compile")
	var sourceArgs stringList
	f.Var(&sourceArgs, "source", "source registration FILE=CHECKOUT")
	output := f.String("output", "", "bundle output directory")
	plan := f.Bool("plan", false, "compile without writing")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if len(sourceArgs) == 0 || *output == "" {
		return nil, nil, errors.New("--source FILE=CHECKOUT and --output are required")
	}
	inputs := make([]knowledge.SourceInput, 0, len(sourceArgs))
	for _, raw := range sourceArgs {
		spec, err := parseSourceSpec(raw)
		if err != nil {
			return nil, nil, err
		}
		inputs = append(inputs, knowledge.SourceInput{Registration: spec.Registration, CheckoutDir: spec.CheckoutDir})
	}
	git := gitRunner(deps)
	if git == nil {
		git = knowledge.NativeGit{}
	}
	if *plan {
		for _, in := range inputs {
			if _, _, err := knowledge.ResolveRevision(ctx, git, in.CheckoutDir); err != nil {
				return nil, nil, fmt.Errorf("source %s revision: %w", in.Registration.ID, err)
			}
		}
		r := CompileResult{OutputDir: *output, SourceCount: len(inputs), Planned: true}
		return r, []string{line("knowledge.compile", map[string]any{"planned": true, "output_dir": *output, "source_count": len(inputs)})}, nil
	}
	if err := refuseExisting(*output); err != nil {
		return nil, nil, err
	}
	b, err := (knowledge.Compiler{Git: git}).Compile(ctx, inputs, knowledge.CompileOptions{ReaderVersion: "1"})
	if err != nil {
		return nil, nil, err
	}
	if err := writeBundleAtomically(*output, b); err != nil {
		return nil, nil, err
	}
	r := CompileResult{OutputDir: *output, BundleRevision: b.Manifest.BundleRevision, SourceCount: len(b.SourcesLock.Sources), RecordCount: len(b.Records)}
	return r, []string{line("knowledge.compile", map[string]any{"output_dir": r.OutputDir, "bundle_revision": r.BundleRevision, "source_count": r.SourceCount, "record_count": r.RecordCount})}, nil
}

func runVerify(args []string) (any, []string, error) {
	f := flagSet("knowledge verify")
	bundle := f.String("bundle", "", "bundle directory")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if *bundle == "" {
		return nil, nil, errors.New("--bundle is required")
	}
	m, err := knowledge.VerifyBundle(*bundle)
	if err != nil {
		return nil, nil, err
	}
	r := VerifyResult{BundleRevision: m.BundleRevision, SourceCount: len(m.Sources), AssetCount: len(m.Assets)}
	return r, []string{line("knowledge.verify", map[string]any{"bundle_revision": r.BundleRevision, "source_count": r.SourceCount, "asset_count": r.AssetCount})}, nil
}

func runInstall(args []string) (any, []string, error) {
	f := flagSet("knowledge install")
	bundle := f.String("bundle", "", "bundle directory")
	destination := f.String("destination", "", "destination directory")
	plan := f.Bool("plan", false, "verify without writing")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if *bundle == "" || *destination == "" {
		return nil, nil, errors.New("--bundle and --destination are required")
	}
	m, err := knowledge.VerifyBundle(*bundle)
	if err != nil {
		return nil, nil, err
	}
	if *plan {
		r := InstallResult{Destination: *destination, BundleRevision: m.BundleRevision, Planned: true}
		return r, []string{line("knowledge.install", map[string]any{"planned": true, "destination": *destination, "bundle_revision": m.BundleRevision})}, nil
	}
	if err := knowledge.InstallBundle(*bundle, *destination); err != nil {
		return nil, nil, err
	}
	r := InstallResult{Destination: *destination, BundleRevision: m.BundleRevision}
	return r, []string{line("knowledge.install", map[string]any{"destination": r.Destination, "bundle_revision": r.BundleRevision})}, nil
}

type contextArgs struct {
	bundle, repository, project, issue, run, host, taskKind, sideEffect, query, render, renderPath string
	remotes, hostRoles, resources                                                                  stringList
	max, maxBytes                                                                                  int
}

func runContext(args []string, deps IO) (any, []string, error) {
	f := flagSet("context")
	var a contextArgs
	f.StringVar(&a.bundle, "bundle", "", "bundle directory")
	f.StringVar(&a.repository, "repository", "", "repository root")
	f.StringVar(&a.repository, "repository-root", "", "repository root")
	f.Var(&a.remotes, "remote", "Git remote (repeatable)")
	f.Var(&a.remotes, "remotes", "Git remote (repeatable)")
	f.StringVar(&a.project, "project", "", "project")
	f.StringVar(&a.issue, "issue", "", "issue")
	f.StringVar(&a.run, "run", "", "run")
	f.StringVar(&a.host, "host", "", "host")
	f.Var(&a.hostRoles, "host-role", "host role (repeatable)")
	f.Var(&a.hostRoles, "host-roles", "host role (repeatable)")
	f.StringVar(&a.taskKind, "task-kind", "", "task kind")
	f.StringVar(&a.sideEffect, "side-effect", "", "side-effect boundary")
	f.StringVar(&a.sideEffect, "side-effect-boundary", "", "side-effect boundary")
	f.Var(&a.resources, "resource", "resource (repeatable)")
	f.Var(&a.resources, "resources", "resource (repeatable)")
	f.StringVar(&a.query, "query", "", "lexical query")
	f.IntVar(&a.max, "max", 0, "maximum records")
	f.IntVar(&a.max, "max-records", 0, "maximum records")
	f.IntVar(&a.maxBytes, "max-bytes", 0, "maximum rendered bytes")
	f.IntVar(&a.maxBytes, "render-max-bytes", 0, "maximum rendered bytes")
	f.StringVar(&a.render, "render", "", "render output path")
	f.StringVar(&a.renderPath, "render-path", "", "render output path")
	if err := f.Parse(args); err != nil {
		return nil, nil, err
	}
	if f.NArg() != 0 {
		return nil, nil, fmt.Errorf("unexpected argument %q", f.Arg(0))
	}
	if a.bundle == "" {
		return nil, nil, errors.New("--bundle is required")
	}
	if a.max < 0 || a.maxBytes < 0 {
		return nil, nil, errors.New("--max and --max-bytes must be non-negative")
	}
	if a.render != "" && a.renderPath != "" {
		return nil, nil, errors.New("--render and --render-path are mutually exclusive")
	}
	if a.renderPath != "" {
		a.render = a.renderPath
	}
	a.remotes = splitCommaValues(a.remotes)
	a.hostRoles = splitCommaValues(a.hostRoles)
	a.resources = splitCommaValues(a.resources)
	b, err := knowledge.LoadBundle(a.bundle)
	if err != nil {
		return nil, nil, err
	}
	selection := agentcontext.Select(b, agentcontext.SelectionInput{RepositoryRoot: a.repository, Remotes: a.remotes, Project: a.project, Issue: a.issue, Run: a.run, Host: a.host, HostRoles: a.hostRoles, TaskKind: a.taskKind, SideEffectBoundary: a.sideEffect, Resources: a.resources, Query: a.query, MaxRecords: a.max, Now: now(deps)})
	r := ContextResult{BundleRevision: selection.BundleRevision, Matches: make([]ContextMatch, 0, len(selection.Matches))}
	for _, m := range selection.Matches {
		r.Matches = append(r.Matches, ContextMatch{ID: m.Record.ID, SourceRepoID: m.Record.SourceRepoID, Path: m.Record.Provenance.Path, StartLine: m.Record.Provenance.StartLine, EndLine: m.Record.Provenance.EndLine, Reason: append([]string(nil), m.Reason...), SourceCommit: m.SourceCommit, ContentDigest: m.ContentDigest})
	}
	if a.render != "" {
		renderLimit := a.maxBytes
		if renderLimit == 0 {
			renderLimit = defaultRenderMaxBytes
		}
		rendered := agentcontext.Render(selection, renderLimit)
		if err := writeRenderAtomically(a.render, []byte(rendered)); err != nil {
			return nil, nil, err
		}
		r.RenderPath = a.render
		r.RenderBytes = len([]byte(rendered))
	}
	fields := map[string]any{"bundle_revision": r.BundleRevision, "matches": len(r.Matches)}
	if r.RenderPath != "" {
		fields["render_path"] = r.RenderPath
		fields["render_bytes"] = r.RenderBytes
	}
	lines := []string{line("context", fields)}
	for _, m := range r.Matches {
		lines = append(lines, line("context_match", map[string]any{"id": m.ID, "source_repo_id": m.SourceRepoID, "path": m.Path, "start_line": m.StartLine, "end_line": m.EndLine, "reason": strings.Join(m.Reason, ",")}))
	}
	return r, lines, nil
}

func splitCommaValues(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	out := make([]string, 0, len(values))
	for _, raw := range values {
		for _, value := range strings.Split(raw, ",") {
			if value = strings.TrimSpace(value); value != "" {
				out = append(out, value)
			}
		}
	}
	return out
}

func now(deps IO) time.Time {
	if deps.Now != nil {
		return deps.Now()
	}
	if deps.Clock != nil {
		return deps.Clock()
	}
	return time.Now().UTC()
}

func gitRunner(deps IO) knowledge.GitRunner {
	if deps.Git != nil {
		return deps.Git
	}
	return deps.GitRunner
}

func refuseExisting(path string) error {
	if path == "" {
		return errors.New("output directory is required")
	}
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("output path is a symlink: %s", path)
		}
		return fmt.Errorf("output already exists: %s", path)
	}
	if !os.IsNotExist(err) {
		return err
	}
	return nil
}

func writeBundleAtomically(destination string, bundle knowledge.Bundle) error {
	parent := filepath.Dir(destination)
	if err := rejectSymlinkPath(parent, true); err != nil {
		return err
	}
	if err := rejectSymlinkPath(destination, true); err != nil {
		return err
	}
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.MkdirTemp(parent, ".agentctl-knowledge-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if err := bundle.Write(tmp); err != nil {
		return err
	}
	if _, err := knowledge.VerifyBundle(tmp); err != nil {
		return fmt.Errorf("verify staged bundle: %w", err)
	}
	if err := refuseExisting(destination); err != nil {
		return err
	}
	if err := os.Rename(tmp, destination); err != nil {
		return err
	}
	return nil
}

func writeRenderAtomically(destination string, data []byte) error {
	if err := rejectSymlinkPath(filepath.Dir(destination), true); err != nil {
		return err
	}
	if err := rejectSymlinkPath(destination, true); err != nil {
		return err
	}
	if err := refuseExisting(destination); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(parent, ".agentctl-context-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := refuseExisting(destination); err != nil {
		return err
	}
	return os.Rename(tmpName, destination)
}

// rejectSymlinkPath keeps command-owned writes from following a symlink in a
// parent component.  The knowledge package applies the same invariant to
// bundles and checkouts; this local check also covers command output paths.
func rejectSymlinkPath(name string, allowMissing bool) error {
	abs, err := filepath.Abs(name)
	if err != nil {
		return err
	}
	volume := filepath.VolumeName(abs)
	part := volume + string(filepath.Separator)
	rest := abs[len(volume):]
	clean := filepath.Clean(rest)
	if clean == string(filepath.Separator) {
		return nil
	}
	for _, component := range strings.Split(strings.TrimPrefix(filepath.ToSlash(clean), "/"), "/") {
		if component == "" || component == "." {
			continue
		}
		part = filepath.Join(part, component)
		info, statErr := os.Lstat(part)
		if os.IsNotExist(statErr) {
			if allowMissing {
				return nil
			}
			return fmt.Errorf("path component does not exist: %s", part)
		}
		if statErr != nil {
			return statErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("symlink path component rejected: %s", part)
		}
	}
	return nil
}

func line(lead string, fields map[string]any) string {
	keys := make([]string, 0, len(fields))
	for key := range fields {
		keys = append(keys, key)
	}
	// Small local sort avoids depending on the executable's renderer and keeps
	// this package usable by adapters which do not import internal/output.
	sortStrings(keys)
	var b strings.Builder
	b.WriteString(lead)
	for _, key := range keys {
		value := fields[key]
		if value == nil {
			continue
		}
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(textValue(value))
	}
	return b.String()
}

func textValue(v any) string {
	switch x := v.(type) {
	case string:
		if x != "" && strings.IndexFunc(x, func(r rune) bool {
			return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:@+-", r))
		}) == -1 {
			return x
		}
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	default:
		return fmt.Sprint(v)
	}
}

func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j] < xs[j-1]; j-- {
			xs[j], xs[j-1] = xs[j-1], xs[j]
		}
	}
}
