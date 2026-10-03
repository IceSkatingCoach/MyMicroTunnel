// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"encoding/base64"
	"encoding/hex"
	"net"
	"testing"
)

func TestGenerateKeyIsAClampedCurve25519Key(t *testing.T) {
	seen := map[string]bool{}
	for range 16 {
		private, err := GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		if seen[private] {
			t.Fatal("GenerateKey returned the same key twice")
		}
		seen[private] = true

		raw, err := base64.StdEncoding.DecodeString(private)
		if err != nil || len(raw) != 32 {
			t.Fatalf("%q is not 32 bytes of base64", private)
		}
		// A key that skips clamping is accepted by the encoder and rejected by
		// the handshake.
		if raw[0]&7 != 0 || raw[31]&128 != 0 || raw[31]&64 == 0 {
			t.Errorf("%q is not clamped", private)
		}
	}
}

func TestPublicKeyMatchesTheCurve25519TestVector(t *testing.T) {
	// Alice's keypair from RFC 7748, section 6.1.
	private := hexToBase64(t, "77076d0a7318a57d3c16c17251b26645df4c2f87ebc0992ab177fba51db92c2a")
	want := hexToBase64(t, "8520f0098930a754748b7ddcb43ef75a0dbf3a0d26381af4eba4a98eaa9b4e6a")

	// Surrounding whitespace is how a key read from a file arrives.
	public, err := PublicKey(" " + private + "\n")
	if err != nil {
		t.Fatal(err)
	}
	if public != want {
		t.Errorf("PublicKey = %q, want %q", public, want)
	}
}

func hexToBase64(t *testing.T, value string) string {
	t.Helper()
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(raw)
}

func TestPublicKeyRejectsSomethingThatIsNotAKey(t *testing.T) {
	for name, bad := range map[string]string{
		"not base64": "not a key",
		"too short":  base64.StdEncoding.EncodeToString(make([]byte, 31)),
	} {
		if _, err := PublicKey(bad); err == nil {
			t.Errorf("%s was accepted as a private key", name)
		}
	}
}

func TestANetworkNamesItsInterface(t *testing.T) {
	_, network, _ := net.ParseCIDR("192.168.1.0/24")
	if got := (Network{Interface: "en0", Net: network}).String(); got != "en0 192.168.1.0/24" {
		t.Errorf("String() = %q", got)
	}
}

func TestOverlapsOfAMissingRangeIsFalse(t *testing.T) {
	_, network, _ := net.ParseCIDR("10.0.0.0/8")
	if Overlaps(nil, network) || Overlaps(network, nil) {
		t.Error("a missing range was reported as overlapping")
	}
}
