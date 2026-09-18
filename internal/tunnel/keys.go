// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// Keys are generated here rather than by shelling out to `wg genkey`, which is
// the only thing wg(8) was still needed for. A Curve25519 keypair is twenty
// lines of standard library, and not depending on a GPLv2 binary to produce one
// is what lets this package ship without one.

// GenerateKey returns a new private key in the base64 spelling every WireGuard
// config and every AWS parameter uses.
func GenerateKey() (string, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return "", err
	}

	// Clamping, as Curve25519 requires: clear the three low bits, clear the top
	// bit, set the second-highest. `wg genkey` does exactly this, and a key that
	// skips it is accepted by the encoder and rejected by the handshake.
	key[0] &= 248
	key[31] &= 127
	key[31] |= 64

	return base64.StdEncoding.EncodeToString(key[:]), nil
}

// PublicKey derives the public half. It is also the check that a file said to
// hold a private key holds one.
func PublicKey(privateKey string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(privateKey))
	if err != nil {
		return "", fmt.Errorf("a WireGuard key is base64: %w", err)
	}
	if len(raw) != 32 {
		return "", fmt.Errorf("a WireGuard key is 32 bytes, this one is %d", len(raw))
	}

	public, err := curve25519.X25519(raw, curve25519.Basepoint)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(public), nil
}
