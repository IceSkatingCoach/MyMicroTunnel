// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func sample() File {
	return File{
		Address:        "10.100.0.2",
		MTU:            1380,
		PrivateKeyPath: "/etc/wireguard/client.key",
		Config: Config{
			Peers: []Peer{{
				PublicKey:           "kO3T8x9qLQvQ0Yr5Z3nF2mNb8yWc1dEfGhIjKlMnOpQ=",
				Endpoint:            "203.0.113.10:51820",
				AllowedIPs:          []string{"10.100.0.0/24", "172.31.0.0/16"},
				PersistentKeepalive: 25,
			}},
		},
	}
}

func TestConfigSurvivesARoundTrip(t *testing.T) {
	original := sample()

	parsed, err := Parse(Marshal(original))
	if err != nil {
		t.Fatalf("parsing what Marshal wrote: %v", err)
	}

	if parsed.Address != original.Address {
		t.Errorf("address is %q, want %q", parsed.Address, original.Address)
	}
	if parsed.MTU != original.MTU {
		t.Errorf("MTU is %d, want %d", parsed.MTU, original.MTU)
	}
	if parsed.PrivateKeyPath != original.PrivateKeyPath {
		t.Errorf("key path is %q, want %q", parsed.PrivateKeyPath, original.PrivateKeyPath)
	}
	if len(parsed.Config.Peers) != 1 {
		t.Fatalf("got %d peers, want 1", len(parsed.Config.Peers))
	}

	peer := parsed.Config.Peers[0]
	want := original.Config.Peers[0]
	if peer.PublicKey != want.PublicKey {
		t.Errorf("public key is %q", peer.PublicKey)
	}
	if peer.Endpoint != want.Endpoint {
		t.Errorf("endpoint is %q", peer.Endpoint)
	}
	if peer.PersistentKeepalive != 25 {
		t.Errorf("keepalive is %d", peer.PersistentKeepalive)
	}
	if strings.Join(peer.AllowedIPs, ",") != strings.Join(want.AllowedIPs, ",") {
		t.Errorf("allowed ranges are %v, want %v", peer.AllowedIPs, want.AllowedIPs)
	}
}

// The file this writes has to stay a file wg-quick understands, so a machine
// that does have the Homebrew tools can be used to diagnose one that is broken.
func TestMarshalIsAWgQuickConfig(t *testing.T) {
	written := Marshal(sample())

	for _, expected := range []string{
		"[Interface]",
		"Address = 10.100.0.2/32",
		"MTU = 1380",
		"PostUp = wg set %i private-key /etc/wireguard/client.key",
		"[Peer]",
		"AllowedIPs = 10.100.0.0/24, 172.31.0.0/16",
		"Endpoint = 203.0.113.10:51820",
		"PersistentKeepalive = 25",
	} {
		if !strings.Contains(written, expected) {
			t.Errorf("the config has no %q:\n%s", expected, written)
		}
	}

	// The key lives in one file, and this is not it.
	if strings.Contains(written, "PrivateKey =") {
		t.Errorf("the config contains a private key:\n%s", written)
	}
}

func TestParseAcceptsAHandWrittenConfig(t *testing.T) {
	parsed, err := Parse(`
# A config somebody typed, with the spacing and casing they felt like.
[Interface]
address=10.100.0.9/32
  MTU = 1420
ListenPort = 51821

[Peer]
publickey = kO3T8x9qLQvQ0Yr5Z3nF2mNb8yWc1dEfGhIjKlMnOpQ=
AllowedIPs = 10.100.0.0/24 , 192.168.50.0/24
Endpoint   = vpn.example.com:51820
`)
	if err != nil {
		t.Fatalf("a hand-written config was rejected: %v", err)
	}

	if parsed.Address != "10.100.0.9" {
		t.Errorf("address is %q", parsed.Address)
	}
	if parsed.MTU != 1420 || parsed.Config.ListenPort != 51821 {
		t.Errorf("MTU %d, listen port %d", parsed.MTU, parsed.Config.ListenPort)
	}
	if len(parsed.Config.Peers) != 1 {
		t.Fatalf("got %d peers", len(parsed.Config.Peers))
	}
	if got := parsed.Config.Peers[0].AllowedIPs; len(got) != 2 || got[1] != "192.168.50.0/24" {
		t.Errorf("allowed ranges are %v", got)
	}
}

func TestParseKeepsSeveralPeersApart(t *testing.T) {
	parsed, err := Parse(`
[Interface]
Address = 10.100.0.2/32

[Peer]
PublicKey = AAAA
AllowedIPs = 10.0.0.0/24

[Peer]
PublicKey = BBBB
AllowedIPs = 10.1.0.0/24
`)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Config.Peers) != 2 {
		t.Fatalf("got %d peers, want 2", len(parsed.Config.Peers))
	}
	if parsed.Config.Peers[0].PublicKey != "AAAA" || parsed.Config.Peers[1].PublicKey != "BBBB" {
		t.Errorf("the peers were mixed up: %+v", parsed.Config.Peers)
	}
	if parsed.Config.Peers[0].AllowedIPs[0] != "10.0.0.0/24" {
		t.Errorf("an allowed range landed on the wrong peer: %+v", parsed.Config.Peers)
	}
}

func TestParseRejectsSomethingThatIsNotAConfig(t *testing.T) {
	if _, err := Parse("this file is a note to self\n"); err == nil {
		t.Error("a line that is neither a section nor a setting was accepted")
	}
}

func TestLoadReadsTheKeyTheConfigPointsAt(t *testing.T) {
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "client.key")
	configPath := filepath.Join(directory, "wg0.conf")

	// Trailing newline included on purpose: `wg genkey > file` writes one, and
	// a key with a newline on the end is not a key.
	if err := os.WriteFile(keyPath, []byte("qK8vN2xR5tY7uI0oP3aS6dF9gH1jK4lZ7xC0vB3nM5Q=\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	file := sample()
	file.PrivateKeyPath = keyPath
	if err := os.WriteFile(configPath, []byte(Marshal(file)), 0o600); err != nil {
		t.Fatal(err)
	}

	loaded, err := Load(configPath)
	if err != nil {
		t.Fatalf("loading: %v", err)
	}
	if loaded.Config.PrivateKey != "qK8vN2xR5tY7uI0oP3aS6dF9gH1jK4lZ7xC0vB3nM5Q=" {
		t.Errorf("the key came back as %q", loaded.Config.PrivateKey)
	}

	// It has to be usable as-is, which is the whole point of loading it.
	if _, err := loaded.Config.UAPIRequest(); err != nil {
		t.Errorf("the loaded config cannot be applied: %v", err)
	}
}

func TestLoadSaysWhichFileIsMissing(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "wg0.conf")

	file := sample()
	file.PrivateKeyPath = filepath.Join(directory, "gone.key")
	if err := os.WriteFile(configPath, []byte(Marshal(file)), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := Load(configPath)
	if err == nil {
		t.Fatal("a config naming a key that does not exist was loaded")
	}
	// "no such file or directory" on its own does not say which of the two.
	if !strings.Contains(err.Error(), "gone.key") {
		t.Errorf("the message does not name the missing key: %v", err)
	}
}

func TestLoadRefusesAConfigWithNoKeyAtAll(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg0.conf")
	if err := os.WriteFile(path, []byte("[Interface]\nAddress = 10.100.0.2/32\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Error("a config with no key was loaded")
	}
}
