package supervisor

import (
	"encoding/xml"
	"strings"
	"testing"
)

func testServicePlan() ServicePlan {
	return ServicePlan{
		Name:        "io.agentctl.supervisor",
		Description: "agentctl host-local supervisor",
		Executable:  "/opt/agentctl",
		Arguments:   []string{"supervisor", "run"},
		SocketPath:  "/state/agentctl/supervisor.sock",
		StateDir:    "/state/agentctl",
		LogDir:      "/logs/agentctl",
	}
}

// decodePlist reads the rendered plist as ordered key/value tokens so the test
// asserts on the emitted document rather than on the plan struct it came from.
func decodePlist(t *testing.T, contents []byte) map[string]string {
	t.Helper()
	decoder := xml.NewDecoder(strings.NewReader(string(contents)))
	values := map[string]string{}
	key := ""
	expectKey := false
	for {
		token, err := decoder.Token()
		if err != nil {
			break
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "key":
				expectKey = true
			case "string", "integer":
				var value string
				if err := decoder.DecodeElement(&value, &element); err != nil {
					t.Fatal(err)
				}
				if key != "" {
					values[key] = value
					key = ""
				}
			case "true":
				if key != "" {
					values[key] = "true"
					key = ""
				}
			}
		case xml.CharData:
			if expectKey {
				if text := strings.TrimSpace(string(element)); text != "" {
					key = text
				}
				expectKey = false
			}
		}
	}
	return values
}

func TestEveryDerivedUnitDeclaresBothLogPaths(t *testing.T) {
	service := testServicePlan()

	launchd, err := BuildLaunchdInstallPlan(service, "/Users/example/Library/LaunchAgents")
	if err != nil {
		t.Fatal(err)
	}
	values := decodePlist(t, launchd.Contents)
	if values["StandardOutPath"] != "/logs/agentctl/supervisor.out.log" || values["StandardErrorPath"] != "/logs/agentctl/supervisor.err.log" {
		t.Fatalf("launchd plist omitted log paths: %#v", values)
	}
	if values["ThrottleInterval"] != "10" {
		t.Fatalf("launchd plist omitted the respawn throttle: %#v", values)
	}
	if launchd.Service.StandardOutPath == "" || launchd.Service.StandardErrorPath == "" {
		t.Fatalf("launchd service projection omitted log paths: %#v", launchd.Service)
	}

	systemd, err := BuildSystemdInstallPlan(service, "/home/example/.config/systemd/user")
	if err != nil {
		t.Fatal(err)
	}
	unit := string(systemd.Contents)
	if !strings.Contains(unit, "StandardOutput=append:/logs/agentctl/supervisor.out.log") ||
		!strings.Contains(unit, "StandardError=append:/logs/agentctl/supervisor.err.log") {
		t.Fatalf("systemd unit omitted log paths: %s", unit)
	}
	if !strings.Contains(unit, "RestartSec=10") {
		t.Fatalf("systemd unit omitted the respawn throttle: %s", unit)
	}
}

func TestDerivedUnitsFailClosedWithoutALogDirectory(t *testing.T) {
	service := testServicePlan()
	service.LogDir = ""
	if _, err := BuildLaunchdInstallPlan(service, "/Users/example/Library/LaunchAgents"); err == nil {
		t.Fatal("launchd plan rendered without log paths")
	}
	if _, err := BuildSystemdInstallPlan(service, "/home/example/.config/systemd/user"); err == nil {
		t.Fatal("systemd plan rendered without log paths")
	}
	service.LogDir = "relative/logs"
	if _, err := BuildLaunchdInstallPlan(service, "/Users/example/Library/LaunchAgents"); err == nil {
		t.Fatal("launchd plan accepted a relative log directory")
	}
}

func TestServiceLogBaseKeepsReverseDNSLabelsReadable(t *testing.T) {
	for name, want := range map[string]string{
		"io.agentctl.supervisor": "supervisor",
		"agentctl-supervisor":    "agentctl-supervisor",
		"":                       "service",
	} {
		if got := serviceLogBase(name); got != want {
			t.Fatalf("serviceLogBase(%q) = %q, want %q", name, got, want)
		}
	}
}
