// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"fmt"
	"net"
	"strings"
)

// A tunnel claims routes. Every address in its AllowedIPs is an address this
// machine will send down the tunnel instead of onto the local network, and the
// route is installed for as long as the tunnel is up.
//
// That is fine until one of those ranges is a network this machine is already
// on. A laptop whose home LAN is 10.100.0.0/24, joining a tunnel that claims
// 10.100.0.0/24, loses its printer, its NAS and — if the router is in that
// range — its default route's next hop. The tunnel comes up, the symptom is
// "the internet stopped working", and nothing in the WireGuard output says
// why, because from WireGuard's point of view everything succeeded.
//
// So the claim is checked against what the machine is actually attached to,
// twice: before a deployment is created, where the answer is to choose another
// --vpn-cidr, and before a tunnel is raised, where the answer is to move off
// that network or pick another profile. Refusing is deliberate. A warning here
// would be read after the damage, if at all.

// Network is one address range this machine is attached to.
type Network struct {
	Interface string
	Net       *net.IPNet
}

func (n Network) String() string { return n.Interface + " " + n.Net.String() }

// Collision is one claimed range that would take over a local network.
type Collision struct {
	Claimed string
	Local   Network
}

// LocalNetworks lists the IPv4 ranges this machine is on.
//
// Loopback is skipped: 127.0.0.0/8 is not a network anyone tunnels, and it is
// on every machine. Interfaces that are down are skipped too — a laptop with a
// dormant Ethernet port should not be refused a tunnel because of a network it
// is not on.
//
// IPv6 is not considered. Nothing in this product routes it: the tunnel
// carries IPv4 only, so an IPv6 range cannot be taken over by a claim that
// never mentions it.
func LocalNetworks(exclude []string) ([]Network, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}

	skip := map[string]bool{}
	for _, name := range exclude {
		if name != "" {
			skip[name] = true
		}
	}

	var found []Network
	for _, device := range interfaces {
		if device.Flags&net.FlagUp == 0 || device.Flags&net.FlagLoopback != 0 {
			continue
		}
		if skip[device.Name] {
			continue
		}
		addresses, err := device.Addrs()
		if err != nil {
			continue
		}
		for _, address := range addresses {
			network, ok := address.(*net.IPNet)
			if !ok || network.IP.To4() == nil {
				continue
			}
			// Masked, so 192.168.1.42/24 is reported as the network
			// 192.168.1.0/24 rather than as one host's own address.
			found = append(found, Network{
				Interface: device.Name,
				Net:       &net.IPNet{IP: network.IP.Mask(network.Mask), Mask: network.Mask},
			})
		}
	}
	return found, nil
}

// Collisions reports which of the claimed ranges overlap a local network.
//
// Overlap, not equality: a tunnel claiming 10.0.0.0/8 on a machine whose LAN is
// 10.1.2.0/24 swallows that LAN just as completely as an exact match would,
// and the reverse — a /16 LAN and a /24 claim — is the more common of the two.
func Collisions(claimed []string, local []Network) []Collision {
	var found []Collision
	for _, raw := range claimed {
		_, network, err := net.ParseCIDR(strings.TrimSpace(raw))
		if err != nil {
			// A malformed range claims nothing, and is rejected by the
			// validation that owns that question rather than reported here as
			// a collision it is not.
			continue
		}
		for _, candidate := range local {
			if Overlaps(network, candidate.Net) {
				found = append(found, Collision{Claimed: network.String(), Local: candidate})
			}
		}
	}
	return found
}

// Overlaps reports whether two ranges share any address. Containment in either
// direction is enough: the smaller one is inside the larger one.
func Overlaps(left, right *net.IPNet) bool {
	if left == nil || right == nil {
		return false
	}
	return left.Contains(right.IP) || right.Contains(left.IP)
}

// Explain turns collisions into the message the user sees. Each line names the
// interface, because "10.100.0.0/24 is in use" is not actionable and "en0 is
// on 10.100.0.0/24" is.
func Explain(collisions []Collision) string {
	lines := make([]string, 0, len(collisions))
	for _, collision := range collisions {
		lines = append(lines, fmt.Sprintf("  · %s is already on %s, which %s would take over",
			collision.Local.Interface, collision.Local.Net, collision.Claimed))
	}
	return strings.Join(lines, "\n")
}

// ClaimedBy is every range a configuration would route into the tunnel: the
// address it takes for itself, and every peer's AllowedIPs.
func ClaimedBy(file File) []string {
	var claimed []string
	if file.Address != "" {
		claimed = append(claimed, file.Address+"/32")
	}
	for _, peer := range file.Config.Peers {
		claimed = append(claimed, peer.AllowedIPs...)
	}
	return claimed
}
