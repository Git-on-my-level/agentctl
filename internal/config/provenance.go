package config

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// ProvenanceCheck is one deterministic, read-only doctor observation.
type ProvenanceCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Status string `json:"status"`
	Value  string `json:"value,omitempty"`
	Detail string `json:"detail,omitempty"`
}

// ExecutableProvenance records executable resolution and digest without
// exposing command output, arguments, or credentials.
type ExecutableProvenance struct {
	Configured string `json:"configured"`
	Resolved   string `json:"resolved,omitempty"`
	Digest     string `json:"digest,omitempty"`
	Resolvable bool   `json:"resolvable"`
	DigestOK   bool   `json:"digest_ok"`
	Error      string `json:"error,omitempty"`
}

// ProvenanceReport is suitable for a doctor command's text or JSON renderer.
// It contains only local observations. In particular, checks never contact
// server_url/app_url and never update an executable/cache.
type ProvenanceReport struct {
	Valid            bool                            `json:"valid"`
	Profile          string                          `json:"profile,omitempty"`
	Checks           []ProvenanceCheck               `json:"checks"`
	Errors           []string                        `json:"errors,omitempty"`
	Adapters         map[string]ExecutableProvenance `json:"adapters,omitempty"`
	Multica          *ExecutableProvenance           `json:"multica,omitempty"`
	AgentPreferences *AgentPreferences               `json:"agent_preferences,omitempty"`
	Bundle           *BundleProvenance               `json:"bundle,omitempty"`
	Composition      []string                        `json:"composition_order,omitempty"`
	Source           *SourceStatus                   `json:"source,omitempty"`
}

// ProvenanceOptions injects deterministic local functions for tests and
// doctor callers that already have a resolver. The defaults use exec.LookPath
// and SHA-256 over the resolved executable bytes.
type ProvenanceOptions struct {
	ResolveExecutable func(string) (string, error)
	DigestExecutable  func(string) (string, error)
}

// CheckProvenance performs read-only provenance checks on a profile.
func CheckProvenance(profile Profile) ProvenanceReport {
	return CheckProfileProvenance(profile)
}

// CheckProfileProvenance performs read-only provenance checks on a profile.
// Optional options are useful for deterministic tests; omitting them never
// reads Multica state or any cache.
func CheckProfileProvenance(profile Profile, options ...ProvenanceOptions) ProvenanceReport {
	var opts ProvenanceOptions
	if len(options) > 0 {
		opts = options[0]
	}
	if opts.ResolveExecutable == nil {
		opts.ResolveExecutable = exec.LookPath
	}
	if opts.DigestExecutable == nil {
		opts.DigestExecutable = digestExecutable
	}

	report := ProvenanceReport{Valid: true, Adapters: make(map[string]ExecutableProvenance)}
	if profile.AgentPreferences != nil {
		preferences := cloneProfile(Profile{AgentPreferences: profile.AgentPreferences}).AgentPreferences
		report.AgentPreferences = preferences
	}
	if err := validateProfileForReport(profile); err != nil {
		report.addError(err)
	}

	names := make([]string, 0, len(profile.Adapters))
	for name := range profile.Adapters {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		adapter := profile.Adapters[name]
		entry := inspectExecutable(adapter.Executable, opts)
		report.Adapters[name] = entry
		report.addCheck(ProvenanceCheck{
			Name:   "adapter." + name + ".executable",
			OK:     entry.Resolvable && entry.DigestOK,
			Status: status(entry.Resolvable && entry.DigestOK),
			Value:  entry.Resolved,
			Detail: entry.Error,
		})
	}

	if profile.Multica != nil {
		m := profile.Multica
		entry := inspectExecutable(m.Executable, opts)
		report.Multica = &entry
		report.addCheck(ProvenanceCheck{
			Name:   "multica.executable",
			OK:     entry.Resolvable && entry.DigestOK,
			Status: status(entry.Resolvable && entry.DigestOK),
			Value:  entry.Resolved,
			Detail: entry.Error,
		})
		addRequiredCheck(&report, "multica.profile", m.Profile)
		addRequiredCheck(&report, "multica.workspace_id", m.WorkspaceID)
		addURLCheck(&report, "multica.server_url", m.ServerURL)
		addURLCheck(&report, "multica.app_url", m.AppURL)
	}

	return report
}

// CheckConfigProvenance resolves profileName (or Config.DefaultProfile when
// it is empty) and returns its local provenance report. No network request is
// made, even for configured URLs.
func CheckConfigProvenance(cfg Config, profileName string, options ...ProvenanceOptions) (ProvenanceReport, error) {
	if err := cfg.Validate(); err != nil {
		return ProvenanceReport{Valid: false, Errors: []string{err.Error()}}, err
	}
	name, profile, err := cfg.ResolveProfile(profileName)
	if err != nil {
		return ProvenanceReport{Valid: false, Errors: []string{err.Error()}}, err
	}
	var report ProvenanceReport
	if len(options) > 0 {
		report = CheckProfileProvenance(profile, options[0])
	} else {
		report = CheckProfileProvenance(profile)
	}
	report.Profile = name
	if !report.Valid {
		return report, errors.New("config provenance checks failed")
	}
	return report, nil
}

// Doctor is a context-aware alias for CheckConfigProvenance. Context is
// checked before local work so callers can cancel a report without any side
// effect. It intentionally accepts only local provenance options.
func Doctor(ctx context.Context, cfg Config, profileName string, options ...ProvenanceOptions) (ProvenanceReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-ctx.Done():
		return ProvenanceReport{Valid: false, Errors: []string{ctx.Err().Error()}}, ctx.Err()
	default:
	}
	return CheckConfigProvenance(cfg, profileName, options...)
}

func (r *ProvenanceReport) addCheck(check ProvenanceCheck) {
	if check.Status == "" {
		check.Status = status(check.OK)
	}
	r.Checks = append(r.Checks, check)
	if !check.OK {
		r.Valid = false
		if check.Detail != "" {
			r.Errors = append(r.Errors, check.Name+": "+check.Detail)
		}
	}
}

func (r *ProvenanceReport) addError(err error) {
	if err == nil {
		return
	}
	r.Valid = false
	r.Errors = append(r.Errors, err.Error())
}

func addRequiredCheck(report *ProvenanceReport, name, value string) {
	value = strings.TrimSpace(value)
	report.addCheck(ProvenanceCheck{
		Name:   name,
		OK:     value != "",
		Status: status(value != ""),
		Value:  value,
		Detail: requiredDetail(value),
	})
}

func addURLCheck(report *ProvenanceReport, name, raw string) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		report.addCheck(ProvenanceCheck{Name: name, Status: "error", Detail: "URL is required"})
		return
	}
	parsed, err := parseAbsoluteURL(raw)
	if err != nil {
		report.addCheck(ProvenanceCheck{Name: name, Status: "error", Value: raw, Detail: err.Error()})
		return
	}
	report.addCheck(ProvenanceCheck{Name: name, OK: true, Status: "ok", Value: parsed})
}

func inspectExecutable(configured string, options ProvenanceOptions) ExecutableProvenance {
	entry := ExecutableProvenance{Configured: strings.TrimSpace(configured)}
	if entry.Configured == "" {
		entry.Error = "executable is required"
		return entry
	}
	resolved, err := options.ResolveExecutable(entry.Configured)
	if err != nil {
		entry.Error = "resolve executable: " + err.Error()
		return entry
	}
	entry.Resolved = resolved
	entry.Resolvable = true
	digest, err := options.DigestExecutable(resolved)
	if err != nil {
		entry.Error = "digest executable: " + err.Error()
		return entry
	}
	entry.Digest = digest
	entry.DigestOK = digest != ""
	if !entry.DigestOK {
		entry.Error = "digest executable: empty digest"
	}
	return entry
}

func validateProfileForReport(profile Profile) error {
	// A report keeps field-level failures below, but this catches malformed
	// adapter names and malformed Multica URLs in its Errors section.
	return validateProfile(profile)
}

func parseAbsoluteURL(raw string) (string, error) {
	parsed, err := urlParse(raw)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("URL must be absolute")
	}
	if parsed.User != nil {
		return "", errors.New("URL must not contain credentials")
	}
	return raw, nil
}

// urlParse is a small seam kept local so URL checks remain read-only and
// deterministic.
var urlParse = func(raw string) (*url.URL, error) { return url.Parse(raw) }

func digestExecutable(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() {
		return "", errors.New("resolved executable is not a regular file")
	}
	if info.Mode().Perm()&0o111 == 0 {
		return "", errors.New("resolved file is not executable")
	}
	file, err := os.Open(filepath.Clean(path))
	if err != nil {
		return "", err
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func requiredDetail(value string) string {
	if strings.TrimSpace(value) == "" {
		return "value is required"
	}
	return ""
}

func status(ok bool) string {
	if ok {
		return "ok"
	}
	return "error"
}
