package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestRouteSSHHostsValidation(t *testing.T) {
	if err := (Route{SSHHosts: map[string]string{"studio": "user@ssh-alias"}}).Validate(); err != nil {
		t.Fatal(err)
	}
	cases := []map[string]string{{"": "alias"}, {" ": "alias"}, {"studio": ""}, {"studio": " "}, {strings.Repeat("x", 129): "alias"}, {"studio": strings.Repeat("x", 129)}, {"studio": "bad\ndestination"}}
	many := map[string]string{}
	for i := 0; i < 65; i++ {
		many[fmt.Sprint(i)] = "alias"
	}
	cases = append(cases, many)
	for _, hosts := range cases {
		if err := (Route{SSHHosts: hosts}).Validate(); err == nil {
			t.Fatalf("accepted %#v", hosts)
		}
	}
}
