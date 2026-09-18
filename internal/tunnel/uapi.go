// SPDX-License-Identifier: GPL-3.0-or-later
// Package tunnel raises and drops the WireGuard interface on this machine
// without wg-quick and without anything installed from Homebrew.
//
// WHY NOT wg-quick
// ----------------
// macOS has no kernel WireGuard, so wg-quick(8) on darwin is a bash script that
// drives wireguard-go, wg(8), ifconfig(8) and route(8). Shipping it means
// shipping three things this product would rather not:
//
//   - bash 4. The script declares associative arrays at the top level, and
//     macOS has shipped bash 3.2 since 2007 for licensing reasons. Homebrew's
//     formula depends on a newer bash for exactly this reason.
//   - wg(8), which is GPLv2-only. This product is GPLv3, and the two are not
//     compatible: GPLv2-only code cannot be combined into a GPLv3 work.
//   - Homebrew itself, or else a second installer before the installer.
//
// What is left once those go is small: wireguard-go is MIT, it is the only
// piece that has to be native, and everything wg(8) does over its UAPI socket
// is a few lines of text. That is what this package writes.
//
// The protocol is documented at https://www.wireguard.com/xplatform/.
package tunnel

import (
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Peer is one end of the tunnel as this machine sees it.
type Peer struct {
	// PublicKey is base64, the way WireGuard configuration files and the AWS
	// parameter store spell it. The UAPI wants hex; that conversion is this
	// package's job rather than the caller's.
	PublicKey           string
	Endpoint            string
	AllowedIPs          []string
	PersistentKeepalive int
}

// Config is everything the interface needs to know. It is deliberately the same
// set of facts a wg-quick .conf file holds, so the two can be compared by
// reading them.
type Config struct {
	PrivateKey string
	ListenPort int
	Peers      []Peer
}

// UAPIRequest renders the "set" command wireguard-go accepts on its socket.
//
// replace_peers is always sent: this is the whole intended peer list, not a
// patch, and applying it as a patch would leave a retired workstation's key
// configured on the interface after it had been removed everywhere else.
func (c Config) UAPIRequest() (string, error) {
	var builder strings.Builder
	builder.WriteString("set=1\n")

	if c.PrivateKey != "" {
		key, err := hexKey(c.PrivateKey)
		if err != nil {
			return "", fmt.Errorf("the private key is not a WireGuard key: %w", err)
		}
		fmt.Fprintf(&builder, "private_key=%s\n", key)
	}
	if c.ListenPort != 0 {
		fmt.Fprintf(&builder, "listen_port=%d\n", c.ListenPort)
	}
	builder.WriteString("replace_peers=true\n")

	for _, peer := range c.Peers {
		key, err := hexKey(peer.PublicKey)
		if err != nil {
			return "", fmt.Errorf("the public key of peer %q is not a WireGuard key: %w", peer.Endpoint, err)
		}
		fmt.Fprintf(&builder, "public_key=%s\n", key)
		if peer.Endpoint != "" {
			fmt.Fprintf(&builder, "endpoint=%s\n", peer.Endpoint)
		}
		if peer.PersistentKeepalive > 0 {
			fmt.Fprintf(&builder, "persistent_keepalive_interval=%d\n", peer.PersistentKeepalive)
		}
		// Sent even when empty, so a peer whose allowed ranges shrank does not
		// keep the ones it used to have.
		builder.WriteString("replace_allowed_ips=true\n")
		for _, allowed := range peer.AllowedIPs {
			fmt.Fprintf(&builder, "allowed_ip=%s\n", strings.TrimSpace(allowed))
		}
	}

	// The blank line is the end of the command; without it the daemon waits.
	builder.WriteString("\n")
	return builder.String(), nil
}

// hexKey converts the base64 spelling every human-facing file uses into the hex
// the UAPI wants, and rejects anything that is not a 32-byte key. A key that is
// one character short is accepted by a naive encoder and produces a tunnel that
// comes up and never handshakes.
func hexKey(base64Key string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(base64Key))
	if err != nil {
		return "", err
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("a WireGuard key is 32 bytes, this one is %d", len(raw))
	}
	return hex.EncodeToString(raw), nil
}

// --- status ----------------------------------------------------------------

// PeerStatus is what the interface reports about one peer. It is the answer to
// the only question that matters once a tunnel is up: is anything crossing it.
type PeerStatus struct {
	PublicKey     string
	Endpoint      string
	LastHandshake time.Time
	ReceivedBytes int64
	SentBytes     int64
}

// Connected is the definition used everywhere in this product: a handshake
// inside three minutes. PersistentKeepalive is 25 seconds, so three minutes is
// several missed chances rather than one unlucky moment.
func (p PeerStatus) Connected(now time.Time) bool {
	if p.LastHandshake.IsZero() {
		return false
	}
	return now.Sub(p.LastHandshake) < 3*time.Minute
}

type Status struct {
	ListenPort int
	Peers      []PeerStatus
}

// Connected reports whether any peer is answering.
func (s Status) Connected(now time.Time) bool {
	for _, peer := range s.Peers {
		if peer.Connected(now) {
			return true
		}
	}
	return false
}

// ParseStatus reads the response to a "get" command.
//
// Unknown keys are ignored rather than rejected: the daemon is free to report
// more than this product asks about, and a new field in a future wireguard-go
// should not be the thing that stops the tunnel from coming up.
func ParseStatus(response string) (Status, error) {
	var (
		status  Status
		current *PeerStatus
	)

	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, found := strings.Cut(line, "=")
		if !found {
			continue
		}

		switch key {
		case "errno":
			if value != "0" {
				return Status{}, fmt.Errorf("the interface reported errno %s", value)
			}
		case "listen_port":
			status.ListenPort, _ = strconv.Atoi(value)
		case "public_key":
			// Each public_key line starts a new peer's section.
			raw, err := hex.DecodeString(value)
			if err != nil {
				return Status{}, fmt.Errorf("the interface reported an unreadable public key: %w", err)
			}
			status.Peers = append(status.Peers, PeerStatus{
				PublicKey: base64.StdEncoding.EncodeToString(raw),
			})
			current = &status.Peers[len(status.Peers)-1]
		case "endpoint":
			if current != nil {
				current.Endpoint = value
			}
		case "last_handshake_time_sec":
			if current != nil {
				if seconds, err := strconv.ParseInt(value, 10, 64); err == nil && seconds > 0 {
					current.LastHandshake = time.Unix(seconds, 0)
				}
			}
		case "rx_bytes":
			if current != nil {
				current.ReceivedBytes, _ = strconv.ParseInt(value, 10, 64)
			}
		case "tx_bytes":
			if current != nil {
				current.SentBytes, _ = strconv.ParseInt(value, 10, 64)
			}
		}
	}

	sort.Slice(status.Peers, func(left, right int) bool {
		return status.Peers[left].PublicKey < status.Peers[right].PublicKey
	})
	return status, nil
}
