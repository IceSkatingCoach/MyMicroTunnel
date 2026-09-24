// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// The Linux tunnel speaks to the kernel through wireguard-tools rather than a
// UAPI socket. The two formats below are wg(8)'s, and live outside the Linux
// file so they are tested on every platform the repository builds on.

// SetConf renders the configuration as `wg setconf` reads it. Unlike the
// wg-quick file Marshal writes, this format has no Address, MTU or PostUp: those
// are wg-quick's, and the Linux tunnel applies them itself.
func SetConf(config Config) string {
	lines := []string{"[Interface]"}
	if config.PrivateKey != "" {
		lines = append(lines, "PrivateKey = "+config.PrivateKey)
	}
	if config.ListenPort > 0 {
		lines = append(lines, "ListenPort = "+strconv.Itoa(config.ListenPort))
	}
	for _, peer := range config.Peers {
		lines = append(lines, "", "[Peer]", "PublicKey = "+peer.PublicKey)
		if len(peer.AllowedIPs) > 0 {
			lines = append(lines, "AllowedIPs = "+strings.Join(peer.AllowedIPs, ", "))
		}
		if peer.Endpoint != "" {
			lines = append(lines, "Endpoint = "+peer.Endpoint)
		}
		if peer.PersistentKeepalive > 0 {
			lines = append(lines, "PersistentKeepalive = "+strconv.Itoa(peer.PersistentKeepalive))
		}
	}
	return strings.Join(append(lines, ""), "\n")
}

// ParseDump reads `wg show <interface> dump`: one tab-separated line for the
// interface, then one per peer.
//
//	private-key public-key listen-port fwmark
//	public-key preshared-key endpoint allowed-ips latest-handshake rx tx keepalive
func ParseDump(output string) (Status, error) {
	var status Status
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) == 0 || strings.TrimSpace(lines[0]) == "" {
		return Status{}, fmt.Errorf("wg reported nothing about the interface")
	}

	head := strings.Split(lines[0], "\t")
	if len(head) < 3 {
		return Status{}, fmt.Errorf("wg reported an interface line it did not recognise: %q", lines[0])
	}
	status.ListenPort, _ = strconv.Atoi(head[2])

	for _, line := range lines[1:] {
		fields := strings.Split(line, "\t")
		if len(fields) < 7 {
			continue
		}
		peer := PeerStatus{PublicKey: fields[0]}
		if fields[2] != "(none)" {
			peer.Endpoint = fields[2]
		}
		if seconds, err := strconv.ParseInt(fields[4], 10, 64); err == nil && seconds > 0 {
			peer.LastHandshake = time.Unix(seconds, 0)
		}
		peer.ReceivedBytes, _ = strconv.ParseInt(fields[5], 10, 64)
		peer.SentBytes, _ = strconv.ParseInt(fields[6], 10, 64)
		status.Peers = append(status.Peers, peer)
	}

	sort.Slice(status.Peers, func(left, right int) bool {
		return status.Peers[left].PublicKey < status.Peers[right].PublicKey
	})
	return status, nil
}
