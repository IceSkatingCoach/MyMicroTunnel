// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func key(t *testing.T) string {
	t.Helper()
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestUAPIRequestIsWhatTheDaemonExpects(t *testing.T) {
	private := key(t)
	public := key(t)

	request, err := Config{
		PrivateKey: private,
		Peers: []Peer{{
			PublicKey:           public,
			Endpoint:            "203.0.113.10:51820",
			AllowedIPs:          []string{"10.100.0.0/24", "172.31.0.0/16"},
			PersistentKeepalive: 25,
		}},
	}.UAPIRequest()
	if err != nil {
		t.Fatalf("rendering: %v", err)
	}

	for _, expected := range []string{
		"set=1\n",
		"replace_peers=true\n",
		"endpoint=203.0.113.10:51820\n",
		"persistent_keepalive_interval=25\n",
		"replace_allowed_ips=true\n",
		"allowed_ip=10.100.0.0/24\n",
		"allowed_ip=172.31.0.0/16\n",
	} {
		if !strings.Contains(request, expected) {
			t.Errorf("the request has no %q:\n%s", expected, request)
		}
	}

	// The command ends at a blank line. Without it the daemon waits for more
	// and the caller waits for an answer.
	if !strings.HasSuffix(request, "\n\n") {
		t.Errorf("the request is not terminated by a blank line:\n%q", request[len(request)-4:])
	}

	// Keys go over the wire as hex, however they are spelled everywhere else.
	if strings.Contains(request, private) || strings.Contains(request, public) {
		t.Error("a base64 key reached the request; the daemon reads hex")
	}
}

func TestUAPIRequestRejectsAKeyThatIsNotOne(t *testing.T) {
	cases := map[string]string{
		"not base64 at all":  "this is not a key",
		"one byte too short": base64.StdEncoding.EncodeToString(make([]byte, 31)),
		"one byte too long":  base64.StdEncoding.EncodeToString(make([]byte, 33)),
	}

	for name, bad := range cases {
		t.Run(name, func(t *testing.T) {
			// A key of the wrong length is accepted by a naive encoder and
			// produces a tunnel that comes up and never handshakes, which is
			// the most expensive failure this product has.
			if _, err := (Config{PrivateKey: bad}).UAPIRequest(); err == nil {
				t.Errorf("%s was accepted as a private key", name)
			}
			if _, err := (Config{Peers: []Peer{{PublicKey: bad}}}).UAPIRequest(); err == nil {
				t.Errorf("%s was accepted as a public key", name)
			}
		})
	}
}

func TestUAPIRequestOmitsWhatWasNotAsked(t *testing.T) {
	request, err := Config{Peers: []Peer{{PublicKey: key(t)}}}.UAPIRequest()
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"private_key=", "listen_port=", "endpoint=", "persistent_keepalive"} {
		if strings.Contains(request, absent) {
			t.Errorf("the request carries %q for a config that did not set it:\n%s", absent, request)
		}
	}
}

func TestParseStatusReadsAConnectedPeer(t *testing.T) {
	handshake := time.Now().Add(-30 * time.Second)

	status, err := ParseStatus(strings.Join([]string{
		"private_key=0000000000000000000000000000000000000000000000000000000000000000",
		"listen_port=51820",
		"public_key=" + strings.Repeat("ab", 32),
		"endpoint=203.0.113.10:51820",
		"last_handshake_time_sec=" + strconv.FormatInt(handshake.Unix(), 10),
		"last_handshake_time_nsec=0",
		"rx_bytes=2048",
		"tx_bytes=1024",
		"errno=0",
		"",
		"",
	}, "\n"))
	if err != nil {
		t.Fatalf("parsing: %v", err)
	}

	if status.ListenPort != 51820 {
		t.Errorf("listen port is %d", status.ListenPort)
	}
	if len(status.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(status.Peers))
	}

	peer := status.Peers[0]
	if peer.Endpoint != "203.0.113.10:51820" {
		t.Errorf("endpoint is %q", peer.Endpoint)
	}
	if peer.ReceivedBytes != 2048 || peer.SentBytes != 1024 {
		t.Errorf("counters are rx=%d tx=%d", peer.ReceivedBytes, peer.SentBytes)
	}
	// Reported back in the spelling the rest of the product uses.
	if _, err := base64.StdEncoding.DecodeString(peer.PublicKey); err != nil {
		t.Errorf("the public key came back as %q, which is not base64", peer.PublicKey)
	}
	if !peer.Connected(time.Now()) {
		t.Error("a peer that handshook 30 seconds ago is not reported as connected")
	}
	if !status.Connected(time.Now()) {
		t.Error("an interface with a live peer is not reported as connected")
	}
}

func TestParseStatusTreatsASilentPeerAsDisconnected(t *testing.T) {
	stale := time.Now().Add(-10 * time.Minute)

	status, err := ParseStatus(strings.Join([]string{
		"public_key=" + strings.Repeat("cd", 32),
		"last_handshake_time_sec=" + strconv.FormatInt(stale.Unix(), 10),
		"errno=0",
		"",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	// An interface can exist for hours after the far end has gone. This is the
	// difference the supervisor acts on.
	if status.Connected(time.Now()) {
		t.Error("a peer last heard from ten minutes ago is reported as connected")
	}
}

func TestParseStatusTreatsANeverConnectedPeerAsDisconnected(t *testing.T) {
	status, err := ParseStatus("public_key=" + strings.Repeat("ef", 32) + "\nlast_handshake_time_sec=0\nerrno=0\n\n")
	if err != nil {
		t.Fatal(err)
	}
	if status.Connected(time.Now()) {
		t.Error("a peer that has never handshaken is reported as connected")
	}
}

func TestParseStatusSurfacesAnError(t *testing.T) {
	if _, err := ParseStatus("errno=1\n\n"); err == nil {
		t.Error("errno=1 was read as success")
	}
}

func TestParseStatusIgnoresKeysItDoesNotKnow(t *testing.T) {
	// A future wireguard-go reporting more than this product asks about must
	// not be the reason a tunnel refuses to come up.
	status, err := ParseStatus("something_new=42\npublic_key=" + strings.Repeat("ab", 32) + "\nerrno=0\n\n")
	if err != nil {
		t.Fatalf("an unknown key was rejected: %v", err)
	}
	if len(status.Peers) != 1 {
		t.Errorf("got %d peers, want 1", len(status.Peers))
	}
}

func TestParseStatusAssignsFieldsToTheRightPeer(t *testing.T) {
	first := strings.Repeat("11", 32)
	second := strings.Repeat("22", 32)

	status, err := ParseStatus(strings.Join([]string{
		"public_key=" + first,
		"rx_bytes=100",
		"public_key=" + second,
		"rx_bytes=200",
		"errno=0",
		"",
	}, "\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(status.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(status.Peers))
	}
	// Sorted by key, so the assertion does not depend on report order.
	if status.Peers[0].ReceivedBytes+status.Peers[1].ReceivedBytes != 300 {
		t.Errorf("counters were mixed up: %+v", status.Peers)
	}
	for _, peer := range status.Peers {
		if peer.ReceivedBytes != 100 && peer.ReceivedBytes != 200 {
			t.Errorf("a counter landed on the wrong peer: %+v", peer)
		}
	}
}

// The address a tunnel takes is its own and nothing else.
//
// ifconfig with no netmask falls back to the classful default, and every
// address this product hands out is a class A — 10.100.0.2 became
// 10.0.0.0/8, sixteen million addresses, and the first tunnel on a machine
// made every other private range look occupied. Deploying a second profile
// was then refused by the collision check, correctly and uselessly, against
// the first profile's own interface.
func TestTheTunnelClaimsOneAddressNotTheWholeClassA(t *testing.T) {
	source, err := os.ReadFile("tunnel.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(source), `"netmask", "255.255.255.255"`) {
		t.Error("the interface address is set without an explicit netmask, so macOS will " +
			"widen it to the class A and the tunnel will claim 10.0.0.0/8")
	}
}
