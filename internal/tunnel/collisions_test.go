// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"net"
	"strings"
	"testing"
)

func mustNet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, network, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("%s: %v", cidr, err)
	}
	return network
}

// Equality is the easy half. The half that matters is containment in either
// direction: a /8 claim swallows a /24 LAN, and a /16 LAN swallows a /24
// claim, and neither pair is equal.
func TestOverlapsIsNotJustEquality(t *testing.T) {
	cases := []struct {
		left, right string
		overlap     bool
	}{
		{"10.100.0.0/24", "10.100.0.0/24", true},
		{"10.0.0.0/8", "10.1.2.0/24", true},
		{"192.168.0.0/16", "192.168.4.0/24", true},
		{"10.100.0.0/24", "10.101.0.0/24", false},
		{"10.100.0.0/24", "192.168.1.0/24", false},
		// Adjacent, not overlapping. Off by one here would refuse a perfectly
		// good tunnel subnet.
		{"10.100.0.0/24", "10.100.1.0/24", false},
	}

	for _, test := range cases {
		got := Overlaps(mustNet(t, test.left), mustNet(t, test.right))
		if got != test.overlap {
			t.Errorf("Overlaps(%s, %s) = %t, want %t", test.left, test.right, got, test.overlap)
		}
	}
}

// The tunnel claims its own address and every AllowedIPs entry. All of them
// are routes, so all of them can take a local network away — the VPC range
// included, which is the one people forget because they did not choose it.
func TestEveryRouteTheTunnelWouldClaimIsChecked(t *testing.T) {
	file := File{
		Address: "10.100.0.2",
		Config: Config{Peers: []Peer{{
			AllowedIPs: []string{"10.100.0.0/24", "172.31.0.0/16"},
		}}},
	}

	claimed := ClaimedBy(file)
	if len(claimed) != 3 || claimed[0] != "10.100.0.2/32" {
		t.Fatalf("the tunnel claims %v", claimed)
	}

	local := []Network{{Interface: "en0", Net: mustNet(t, "172.31.8.0/22")}}
	collisions := Collisions(claimed, local)
	if len(collisions) != 1 {
		t.Fatalf("%d collisions, want the VPC range: %+v", len(collisions), collisions)
	}
	if collisions[0].Claimed != "172.31.0.0/16" {
		t.Errorf("the reported claim is %s", collisions[0].Claimed)
	}

	// The message has to name the interface. "172.31.0.0/16 is in use" is not
	// something anyone can act on.
	if explained := Explain(collisions); !strings.Contains(explained, "en0") {
		t.Errorf("the explanation does not say where the conflict is:\n%s", explained)
	}
}

func TestANonOverlappingTunnelIsNotRefused(t *testing.T) {
	local := []Network{
		{Interface: "en0", Net: mustNet(t, "192.168.1.0/24")},
		{Interface: "bridge100", Net: mustNet(t, "192.168.64.0/24")},
	}
	if collisions := Collisions([]string{"10.100.0.2/32", "10.100.0.0/24"}, local); len(collisions) != 0 {
		t.Errorf("a usable tunnel was refused: %+v", collisions)
	}
}

// A range that is not a range claims nothing. Reporting it here would attach
// the wrong explanation to it; the validation that owns the question says what
// is wrong with it.
func TestAMalformedRangeIsNotReportedAsACollision(t *testing.T) {
	local := []Network{{Interface: "en0", Net: mustNet(t, "192.168.1.0/24")}}
	if collisions := Collisions([]string{"not-a-cidr", "192.168.1.0/33"}, local); len(collisions) != 0 {
		t.Errorf("a malformed range was reported as a collision: %+v", collisions)
	}
}

// This machine's own tunnel is not a network to be protected from itself:
// re-raising a tunnel that is already up must not be refused.
func TestTheTunnelsOwnInterfaceCanBeExcluded(t *testing.T) {
	all, err := LocalNetworks(nil)
	if err != nil {
		t.Skipf("this machine's interfaces cannot be listed: %v", err)
	}
	if len(all) == 0 {
		t.Skip("this machine is on no IPv4 network")
	}

	without, err := LocalNetworks([]string{all[0].Interface})
	if err != nil {
		t.Fatal(err)
	}
	for _, network := range without {
		if network.Interface == all[0].Interface {
			t.Errorf("%s was excluded and came back anyway", all[0].Interface)
		}
	}
}
