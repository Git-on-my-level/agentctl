package supervisor

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
)

// ServicePlan is pure data describing a requested host service installation.
// Rendering or returning a plan never writes files, calls launchctl/systemctl,
// or enables a service.
type ServicePlan struct {
	Name        string
	Description string
	Executable  string
	Arguments   []string
	SocketPath  string
	StateDir    string
	Environment map[string]string
}

// LaunchdPlan is the launchd-specific projection of ServicePlan.
type LaunchdPlan struct {
	Label            string
	ProgramArguments []string
	Environment      map[string]string
	RunAtLoad        bool
	KeepAlive        bool
}

// SystemdPlan is the systemd-specific projection of ServicePlan.
type SystemdPlan struct {
	UnitName    string
	Description string
	ExecStart   string
	Environment map[string]string
	Restart     string
	WantedBy    string
}

// LaunchdInstallPlan describes a plist path and its exact bytes without
// touching the host.  Callers may present it in a --plan output or hand it to
// an explicit host manager.
type LaunchdInstallPlan struct {
	Path     string
	Contents []byte
	Service  LaunchdPlan
}

// SystemdInstallPlan describes a unit path and exact bytes without touching
// the host.
type SystemdInstallPlan struct {
	Path     string
	Contents []byte
	Service  SystemdPlan
}

// LaunchdPlanFor returns deterministic launchd data.  The command receives
// no shell string; each argument remains one argv element.
func LaunchdPlanFor(service ServicePlan) LaunchdPlan {
	args := append([]string{service.Executable}, service.Arguments...)
	if service.SocketPath != "" {
		args = append(args, "--socket", service.SocketPath)
	}
	if service.StateDir != "" {
		args = append(args, "--state-dir", service.StateDir)
	}
	return LaunchdPlan{
		Label:            service.Name,
		ProgramArguments: args,
		Environment:      cloneEnvironment(service.Environment),
		RunAtLoad:        true,
		KeepAlive:        true,
	}
}

// SystemdPlanFor returns deterministic systemd data.  ExecStart is rendered
// as a shell-free argv-safe escaped string by RenderSystemdUnit.
func SystemdPlanFor(service ServicePlan) SystemdPlan {
	args := append([]string{service.Executable}, service.Arguments...)
	if service.SocketPath != "" {
		args = append(args, "--socket", service.SocketPath)
	}
	if service.StateDir != "" {
		args = append(args, "--state-dir", service.StateDir)
	}
	return SystemdPlan{
		UnitName:    service.Name,
		Description: service.Description,
		ExecStart:   joinSystemdArgs(args),
		Environment: cloneEnvironment(service.Environment),
		Restart:     "on-failure",
		WantedBy:    "default.target",
	}
}

// RenderLaunchdPlist renders a complete plist in a deterministic order.
func RenderLaunchdPlist(plan LaunchdPlan) ([]byte, error) {
	if strings.TrimSpace(plan.Label) == "" {
		return nil, fmt.Errorf("launchd label is required")
	}
	if len(plan.ProgramArguments) == 0 || strings.TrimSpace(plan.ProgramArguments[0]) == "" {
		return nil, fmt.Errorf("launchd program arguments are required")
	}
	root := launchdPlist{
		XMLName: xml.Name{Local: "plist"},
		Version: "1.0",
		Dict: launchdDict{
			Label:            plan.Label,
			ProgramArguments: append([]string(nil), plan.ProgramArguments...),
			Environment:      cloneEnvironment(plan.Environment),
			RunAtLoad:        plan.RunAtLoad,
			KeepAlive:        plan.KeepAlive,
		},
	}
	rendered, err := xml.MarshalIndent(root, "", "  ")
	if err != nil {
		return nil, err
	}
	// encoding/xml expands empty elements to paired tags. plutil accepts that
	// form for plist booleans, but launchd's bootstrap parser rejects it as an
	// invalid property list. Emit the canonical empty boolean element that both
	// parsers accept.
	rendered = bytes.ReplaceAll(rendered, []byte("<true></true>"), []byte("<true/>"))
	return rendered, nil
}

// RenderLaunchdUnit is an alias for callers that use “unit” terminology.
func RenderLaunchdUnit(plan LaunchdPlan) ([]byte, error) { return RenderLaunchdPlist(plan) }

// RenderSystemdUnit renders a pure-data systemd unit.  Environment keys are
// sorted, values are escaped, and no shell evaluation is introduced.
func RenderSystemdUnit(plan SystemdPlan) []byte {
	var b strings.Builder
	b.WriteString("[Unit]\n")
	if plan.Description != "" {
		b.WriteString("Description=")
		b.WriteString(escapeSystemdValue(plan.Description))
		b.WriteByte('\n')
	}
	b.WriteString("\n[Service]\nType=simple\n")
	if plan.ExecStart != "" {
		b.WriteString("ExecStart=")
		b.WriteString(plan.ExecStart)
		b.WriteByte('\n')
	}
	if plan.Restart != "" {
		b.WriteString("Restart=")
		b.WriteString(plan.Restart)
		b.WriteByte('\n')
	}
	for _, key := range sortedKeys(plan.Environment) {
		b.WriteString("Environment=")
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(escapeSystemdValue(plan.Environment[key]))
		b.WriteByte('\n')
	}
	b.WriteString("\n[Install]\n")
	if plan.WantedBy != "" {
		b.WriteString("WantedBy=")
		b.WriteString(plan.WantedBy)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// RenderSystemdUnitText is useful for callers that want to print plan output.
func RenderSystemdUnitText(plan SystemdPlan) string { return string(RenderSystemdUnit(plan)) }

// BuildLaunchdInstallPlan computes the conventional per-user plist path.  It
// never creates the parent directory or invokes launchctl.
func BuildLaunchdInstallPlan(service ServicePlan, launchAgentsDir string) (LaunchdInstallPlan, error) {
	plist, err := RenderLaunchdPlist(LaunchdPlanFor(service))
	if err != nil {
		return LaunchdInstallPlan{}, err
	}
	if launchAgentsDir == "" {
		return LaunchdInstallPlan{}, fmt.Errorf("launchd agents directory is required")
	}
	return LaunchdInstallPlan{
		Path:     filepath.Join(launchAgentsDir, service.Name+".plist"),
		Contents: plist,
		Service:  LaunchdPlanFor(service),
	}, nil
}

// BuildSystemdInstallPlan computes the conventional per-user unit path and
// exact bytes.  It performs no installation or daemon reload.
func BuildSystemdInstallPlan(service ServicePlan, userConfigDir string) (SystemdInstallPlan, error) {
	if strings.TrimSpace(service.Name) == "" {
		return SystemdInstallPlan{}, fmt.Errorf("systemd unit name is required")
	}
	if userConfigDir == "" {
		return SystemdInstallPlan{}, fmt.Errorf("systemd user config directory is required")
	}
	plan := SystemdPlanFor(service)
	return SystemdInstallPlan{
		Path:     filepath.Join(userConfigDir, service.Name+".service"),
		Contents: RenderSystemdUnit(plan),
		Service:  plan,
	}, nil
}

type launchdPlist struct {
	XMLName xml.Name    `xml:"plist"`
	Version string      `xml:"version,attr"`
	Dict    launchdDict `xml:"dict"`
}

// launchdDict uses explicit XML elements to preserve plist's key/value shape.
type launchdDict struct {
	Label            string            `xml:"-"`
	ProgramArguments []string          `xml:"-"`
	Environment      map[string]string `xml:"-"`
	RunAtLoad        bool              `xml:"-"`
	KeepAlive        bool              `xml:"-"`
}

func (d launchdDict) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	if err := plistKeyString(e, "Label", d.Label); err != nil {
		return err
	}
	if err := plistKeyArray(e, "ProgramArguments", d.ProgramArguments); err != nil {
		return err
	}
	if len(d.Environment) > 0 {
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.CharData("EnvironmentVariables")); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "key"}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "dict"}}); err != nil {
			return err
		}
		for _, key := range sortedKeys(d.Environment) {
			if err := plistKeyString(e, key, d.Environment[key]); err != nil {
				return err
			}
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "dict"}}); err != nil {
			return err
		}
	}
	if d.RunAtLoad {
		if err := plistKeyBool(e, "RunAtLoad"); err != nil {
			return err
		}
	}
	if d.KeepAlive {
		if err := plistKeyBool(e, "KeepAlive"); err != nil {
			return err
		}
	}
	return e.EncodeToken(xml.EndElement{Name: start.Name})
}

func plistKeyString(e *xml.Encoder, key, value string) error {
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(key)); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(value)); err != nil {
		return err
	}
	return e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "string"}})
}

func plistKeyArray(e *xml.Encoder, key string, values []string) error {
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(key)); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "array"}}); err != nil {
		return err
	}
	for _, value := range values {
		if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "string"}}); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.CharData(value)); err != nil {
			return err
		}
		if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "string"}}); err != nil {
			return err
		}
	}
	return e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "array"}})
}

func plistKeyBool(e *xml.Encoder, key string) error {
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.CharData(key)); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "key"}}); err != nil {
		return err
	}
	if err := e.EncodeToken(xml.StartElement{Name: xml.Name{Local: "true"}}); err != nil {
		return err
	}
	return e.EncodeToken(xml.EndElement{Name: xml.Name{Local: "true"}})
}

func cloneEnvironment(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for key, value := range in {
		out[key] = value
	}
	return out
}

func sortedKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func joinSystemdArgs(args []string) string {
	parts := make([]string, len(args))
	for i, arg := range args {
		parts[i] = escapeSystemdArg(arg)
	}
	return strings.Join(parts, " ")
}

func escapeSystemdArg(value string) string {
	if value != "" && strings.IndexFunc(value, func(r rune) bool {
		return r == ' ' || r == '\t' || r == '\n' || r == '\\' || r == '"' || r == '\''
	}) == -1 {
		return value
	}
	return `"` + strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`) + `"`
}

func escapeSystemdValue(value string) string {
	return escapeSystemdArg(value)
}
