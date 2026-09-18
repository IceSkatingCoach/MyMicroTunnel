// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"sort"
	"strings"
	"testing"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/infra"
)

// This is the test the repository most needed and did not have.
//
// CloudFormation is indifferent to a parameter the installer sends under a name
// the template does not declare — the change set is rejected — and to a
// required parameter nobody sends, which fails the same way five minutes into a
// deploy. Renaming one side and not the other is a one-character mistake with a
// several-minute feedback loop.
func TestEveryParameterTheInstallerSendsIsDeclared(t *testing.T) {
	declared := map[string]bool{}
	for _, parameter := range infra.Parameters() {
		declared[parameter.Name] = true
	}

	var unknown []string
	for name := range StackParameters(valid()) {
		if !declared[name] {
			unknown = append(unknown, name)
		}
	}
	sort.Strings(unknown)

	if len(unknown) > 0 {
		t.Errorf("the installer sends parameters the template does not declare: %s",
			strings.Join(unknown, ", "))
	}
}

func TestEveryRequiredParameterIsSent(t *testing.T) {
	sent := StackParameters(valid())

	var missing []string
	for _, parameter := range infra.Parameters() {
		if !parameter.Required {
			continue
		}
		if _, present := sent[parameter.Name]; !present {
			missing = append(missing, parameter.Name)
		}
	}
	sort.Strings(missing)

	if len(missing) > 0 {
		t.Errorf("the template requires parameters the installer never sends: %s",
			strings.Join(missing, ", "))
	}
}

// A required parameter sent as an empty string fails as late as one that is not
// sent at all, and reads as a working install right up until it does.
func TestNoRequiredParameterIsSentEmpty(t *testing.T) {
	sent := StackParameters(valid())

	for _, parameter := range infra.Parameters() {
		if !parameter.Required {
			continue
		}
		if strings.TrimSpace(sent[parameter.Name]) == "" {
			t.Errorf("%s is required and a fully discovered deployment sends it empty", parameter.Name)
		}
	}
}

func TestListParametersAreSentCommaSeparated(t *testing.T) {
	s := valid()
	s.SubnetIDs = []string{"subnet-a", "subnet-b", "subnet-c"}
	s.RouteTableIDs = []string{"rtb-a", "rtb-b"}

	sent := StackParameters(s)
	if sent["SubnetIds"] != "subnet-a,subnet-b,subnet-c" {
		t.Errorf("SubnetIds is %q", sent["SubnetIds"])
	}
	if sent["RouteTableIds"] != "rtb-a,rtb-b" {
		t.Errorf("RouteTableIds is %q", sent["RouteTableIds"])
	}
}

func TestAlarmParametersAreStringsCloudFormationAccepts(t *testing.T) {
	s := valid()
	s.AlarmOnTunnelDown = true
	if got := StackParameters(s)["AlarmOnTunnelDown"]; got != "true" {
		t.Errorf("AlarmOnTunnelDown is %q, want \"true\"", got)
	}

	s.AlarmOnTunnelDown = false
	if got := StackParameters(s)["AlarmOnTunnelDown"]; got != "false" {
		t.Errorf("AlarmOnTunnelDown is %q, want \"false\"", got)
	}
}
