// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/infra"
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

// Every slot is sent on every deploy, including the empty ones. A port that
// the user removed has to arrive as 0 for its listener to be torn down: left
// out, CloudFormation carries the previous value forward with
// UsePreviousValue and the port stays open on a deployment that no longer
// claims to expose it.
func TestEveryPortSlotIsSentEvenWhenUnused(t *testing.T) {
	s := valid()
	s.TcpPorts = []string{"5432", "6379"}
	sent := StackParameters(s)

	if sent["TcpPort1"] != "5432" || sent["TcpPort2"] != "6379" {
		t.Errorf("the asked-for ports are %q and %q", sent["TcpPort1"], sent["TcpPort2"])
	}
	for slot := 3; slot <= MaxTcpPorts; slot++ {
		key := fmt.Sprintf("TcpPort%d", slot)
		if sent[key] != "0" {
			t.Errorf("%s is %q, want \"0\" so the slot is torn down", key, sent[key])
		}
	}
}

func TestIdleTimeoutAndWakePrincipalReachTheTemplate(t *testing.T) {
	s := valid()
	s.IdleTimeoutMinutes = 30
	s.AppPrincipalArn = "arn:aws:iam::123456789012:role/Someone"

	sent := StackParameters(s)
	if sent["IdleTimeoutMinutes"] != "30" {
		t.Errorf("IdleTimeoutMinutes is %q", sent["IdleTimeoutMinutes"])
	}
	if sent["AppPrincipalArn"] != s.AppPrincipalArn {
		t.Errorf("AppPrincipalArn is %q", sent["AppPrincipalArn"])
	}
}

// The outputs are pairs because a slot with no port has no target group at
// all, so the installer cannot reconstruct the names it should register in.
func TestTcpTargetGroupsReadsThePairsBack(t *testing.T) {
	groups := TcpTargetGroups(map[string]string{
		"TcpTarget1":      "5432=arn:aws:elasticloadbalancing:::targetgroup/a",
		"TcpTarget2":      "6379=arn:aws:elasticloadbalancing:::targetgroup/b",
		"TargetGroupArn":  "arn:aws:elasticloadbalancing:::targetgroup/primary",
		"GatewayPublicIp": "203.0.113.7",
	})

	if len(groups) != 2 {
		t.Fatalf("%d groups, want 2: %v", len(groups), groups)
	}
	if groups["5432"] != "arn:aws:elasticloadbalancing:::targetgroup/a" {
		t.Errorf("5432 maps to %q", groups["5432"])
	}
	// The primary target group is not one of these: it is reached through the
	// TLS listener and registered separately.
	if _, present := groups["primary"]; present {
		t.Error("the primary target group was read as an exposed TCP port")
	}
}

// Every published port is a mapping, including the TLS one. The listener
// takes the published half and the target group the local half; swapping them
// builds a load balancer that answers on the wrong port and forwards to a
// port nothing is listening on.
func TestPortMappingsReachTheTemplateTheRightWayRound(t *testing.T) {
	s := valid()
	s.ServicePort = "3000"
	s.PublishedPort = "8443"
	s.TcpPorts = []string{"5432", "3000:8080"}

	sent := StackParameters(s)

	if sent["ServicePort"] != "3000" || sent["TlsListenerPort"] != "8443" {
		t.Errorf("the service is %s published on %s", sent["ServicePort"], sent["TlsListenerPort"])
	}
	// A bare number is the same port on both sides.
	if sent["TcpPort1"] != "5432" || sent["TcpTargetPort1"] != "5432" {
		t.Errorf("slot 1 is %s -> %s", sent["TcpPort1"], sent["TcpTargetPort1"])
	}
	// And a mapping is not.
	if sent["TcpPort2"] != "8080" || sent["TcpTargetPort2"] != "3000" {
		t.Errorf("slot 2 publishes %s and reaches %s", sent["TcpPort2"], sent["TcpTargetPort2"])
	}
	// Unused slots still have a legal target port, or CloudFormation rejects
	// the parameter before it notices the slot is switched off.
	if sent["TcpTargetPort3"] != "1" {
		t.Errorf("an unused slot has target port %q", sent["TcpTargetPort3"])
	}
}

func TestPortMappingsAreReadBackWithBothHalves(t *testing.T) {
	groups := TcpTargetGroups(map[string]string{
		"TcpTarget1": "5432:5432=arn:aws:elasticloadbalancing:::targetgroup/a",
		"TcpTarget2": "3000:8080=arn:aws:elasticloadbalancing:::targetgroup/b",
	})

	// Keyed by the mapping: the local port alone is not unique, because two
	// published ports may reach one service.
	if groups["3000:8080"] == "" {
		t.Fatalf("the mapping was not read back: %v", groups)
	}
	mapping, err := ParsePortMapping("3000:8080")
	if err != nil || mapping.Local != 3000 || mapping.Published != 8080 {
		t.Errorf("parsed %+v (%v); registration would use the wrong port", mapping, err)
	}
}

// Two listeners cannot share a port; two services can share a local one.
func TestValidateRejectsAPublishedPortTwiceButAllowsALocalOne(t *testing.T) {
	s := valid()
	s.TcpPorts = []string{"3000:8080", "4000:8080"}
	if err := s.Validate(); err == nil {
		t.Error("two listeners were accepted on port 8080")
	}

	s.TcpPorts = []string{"3000:8080", "3000:9090"}
	if err := s.Validate(); err != nil {
		t.Errorf("publishing one service on two ports was refused: %v", err)
	}

	// And nothing may collide with the TLS listener, wherever it was put.
	s.PublishedPort = "8443"
	s.TcpPorts = []string{"3000:8443"}
	if err := s.Validate(); err == nil {
		t.Error("a port collided with the TLS listener and was accepted")
	}
}
