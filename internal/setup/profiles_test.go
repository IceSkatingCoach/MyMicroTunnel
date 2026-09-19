// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"strings"
	"testing"
)

// The default name is what a customer sees in the CloudFormation console, and
// it is derived from the account and the region because one fixed name makes
// the second deployment in an account collide with the first — silently, by
// updating it.
func TestDefaultStackNameIsPerAccountAndRegion(t *testing.T) {
	name := DefaultStackName("123456789012", "us-east-2")
	if name != "mymicrotunnel-123456789012-us-east-2" {
		t.Errorf("the default stack name is %q", name)
	}
	if !stackNamePattern.MatchString(name) {
		t.Errorf("CloudFormation would reject %q as a stack name", name)
	}

	// Nothing useful can be built before the credentials resolve, and a
	// half-built name would deploy into a stack called "microtunnel--".
	if DefaultStackName("", "us-east-2") != "" || DefaultStackName("1234", "") != "" {
		t.Error("a stack name was invented from an unknown account or region")
	}
}

// Two profiles on one interface would both write /etc/wireguard/wg0.conf, and
// the second install would take the first deployment down without saying so.
func TestASecondProfileGetsItsOwnInterface(t *testing.T) {
	if got := NextFreeInterface(nil); got != "wg0" {
		t.Errorf("the first profile got %q", got)
	}
	if got := NextFreeInterface([]string{"wg0"}); got != "wg1" {
		t.Errorf("the second profile got %q", got)
	}
	if got := NextFreeInterface([]string{"wg1", "wg0", "wg2"}); got != "wg3" {
		t.Errorf("the fourth profile got %q", got)
	}
}

func TestCollidingProfilesAreRefusedBeforeAnythingIsDeployed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := valid()
	first.ProfileName = "work"
	first.InterfaceName = "wg0"
	first.ClientAddress = "10.100.0.2"
	writeProfileForTest(t, first)

	cases := map[string]func(*Settings){
		"the same interface": func(s *Settings) { s.InterfaceName = "wg0"; s.ClientAddress = "10.101.0.2" },
		"the same address":   func(s *Settings) { s.InterfaceName = "wg1"; s.ClientAddress = "10.100.0.2" },
	}
	for name, collide := range cases {
		t.Run(name, func(t *testing.T) {
			second := valid()
			second.ProfileName = "personal"
			collide(&second)
			if err := second.ConflictsWithOtherProfiles(); err == nil {
				t.Errorf("a second profile with %s was accepted", name)
			}
		})
	}

	// And a profile that collides with nothing but itself is a re-run, which
	// has to keep working.
	again := first
	if err := again.ConflictsWithOtherProfiles(); err != nil {
		t.Errorf("re-running an installed profile was refused: %v", err)
	}
}

// One key per interface. Sharing one between two deployments means either of
// them revoking it takes the other down.
func TestEachProfileHasItsOwnPrivateKey(t *testing.T) {
	first := (Settings{InterfaceName: "wg0"}).ClientKeyPath()
	second := (Settings{InterfaceName: "wg1"}).ClientKeyPath()

	if first != "/etc/wireguard/wg0.key" || second != "/etc/wireguard/wg1.key" {
		t.Errorf("the two tunnels keep their keys at %s and %s", first, second)
	}
}

// Two profiles whose tunnel subnets overlap is the collision that is easiest
// to create — two installs, both accepting the default 10.100.0.0/24 — and
// hardest to diagnose: both tunnels come up, and the routing table sends one
// deployment's traffic down the other's.
func TestProfilesWithOverlappingTunnelSubnetsAreRefused(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	first := valid()
	first.ProfileName = "work"
	first.InterfaceName = "wg0"
	first.VpnCidr = "10.100.0.0/24"
	first.ClientAddress = "10.100.0.2"
	writeProfileForTest(t, first)

	// A different interface and a different address, but inside the same
	// range: still one routing table, still a collision.
	second := valid()
	second.ProfileName = "lab"
	second.InterfaceName = "wg1"
	second.StackName = "mymicrotunnel-123456789012-eu-west-1"
	second.VpnCidr = "10.100.0.0/25"
	second.ClientAddress = "10.100.0.9"

	err := second.ConflictsWithOtherProfiles()
	if err == nil {
		t.Fatal("a second profile inside the first one's tunnel subnet was accepted")
	}
	if !strings.Contains(err.Error(), "--vpn-cidr") {
		t.Errorf("the refusal does not say how to fix it: %v", err)
	}

	// Its own range, well clear of the first: accepted.
	second.VpnCidr = "10.110.0.0/24"
	second.ClientAddress = "10.110.0.2"
	if err := second.ConflictsWithOtherProfiles(); err != nil {
		t.Errorf("a profile with its own tunnel subnet was refused: %v", err)
	}
}

// Choosing a tunnel subnet for a second profile must not then be refused for
// two addresses the user never typed. The subnet is the choice; the gateway
// takes its first usable address and this machine the second.
func TestTunnelAddressesFollowTheSubnet(t *testing.T) {
	s := valid()
	s.VpnCidr = "10.110.0.0/24"
	s.GatewayAddress = "10.100.0.1"
	s.ClientAddress = "10.100.0.2"

	s.AlignAddressesToVpnCidr()

	if s.GatewayAddress != "10.110.0.1" || s.ClientAddress != "10.110.0.2" {
		t.Fatalf("the addresses stayed at %s and %s", s.GatewayAddress, s.ClientAddress)
	}
	if err := s.Validate(); err != nil {
		t.Errorf("a profile with a subnet of its own was still refused: %v", err)
	}
}

// A second Mac on one deployment is given .3 by hand. Dragging it back to .2
// would collide with the first machine, which is the bug this avoids.
func TestAnAddressInsideTheSubnetIsLeftAlone(t *testing.T) {
	s := valid()
	s.VpnCidr = "10.100.0.0/24"
	s.ClientAddress = "10.100.0.3"

	s.AlignAddressesToVpnCidr()

	if s.ClientAddress != "10.100.0.3" {
		t.Errorf("a deliberate address was moved to %s", s.ClientAddress)
	}
}
