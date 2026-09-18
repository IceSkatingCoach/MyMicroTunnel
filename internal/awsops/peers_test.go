// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import "testing"

func addresses(peers []Peer) []string {
	list := make([]string, 0, len(peers))
	for _, peer := range peers {
		list = append(list, peer.Address)
	}
	return list
}

func TestMergePeerAddsAWorkstationWithoutDisturbingTheOthers(t *testing.T) {
	existing := []Peer{
		{PublicKey: "aaa=", Address: "10.100.0.2", Label: "laptop"},
		{PublicKey: "bbb=", Address: "10.100.0.3", Label: "desktop"},
	}

	merged := MergePeer(existing, Peer{PublicKey: "ccc=", Address: "10.100.0.4", Label: "builder"})

	if len(merged) != 3 {
		t.Fatalf("got %d peers, want 3: %v", len(merged), addresses(merged))
	}
	// Adding the third workstation must not take the first two offline; that
	// was the whole reason peers stopped being a stack parameter.
	for _, want := range []string{"10.100.0.2", "10.100.0.3", "10.100.0.4"} {
		found := false
		for _, peer := range merged {
			if peer.Address == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%s is missing after the merge", want)
		}
	}
}

func TestMergePeerReplacesARegeneratedKeyAtTheSameAddress(t *testing.T) {
	existing := []Peer{{PublicKey: "old=", Address: "10.100.0.2", Label: "laptop"}}

	merged := MergePeer(existing, Peer{PublicKey: "new=", Address: "10.100.0.2", Label: "laptop"})

	if len(merged) != 1 {
		t.Fatalf("got %d peers, want 1: %v", len(merged), merged)
	}
	// Two peers claiming one address is resolved by the gateway in favour of
	// whichever handshook last, which is a coin toss rather than a decision.
	if merged[0].PublicKey != "new=" {
		t.Errorf("the stale key survived: %v", merged[0])
	}
}

func TestMergePeerMovesAKeyToANewAddress(t *testing.T) {
	existing := []Peer{{PublicKey: "aaa=", Address: "10.100.0.2"}}

	merged := MergePeer(existing, Peer{PublicKey: "aaa=", Address: "10.100.0.7"})

	if len(merged) != 1 {
		t.Fatalf("the same key is registered twice: %v", addresses(merged))
	}
	if merged[0].Address != "10.100.0.7" {
		t.Errorf("the address did not move: %v", merged[0])
	}
}

func TestMergePeerIntoAnEmptyRegistry(t *testing.T) {
	merged := MergePeer(nil, Peer{PublicKey: "aaa=", Address: "10.100.0.2"})
	if len(merged) != 1 {
		t.Fatalf("got %d peers, want 1", len(merged))
	}
}

func TestWithoutPeer(t *testing.T) {
	existing := []Peer{
		{PublicKey: "aaa=", Address: "10.100.0.2"},
		{PublicKey: "bbb=", Address: "10.100.0.3"},
	}

	kept := WithoutPeer(existing, "10.100.0.2")
	if len(kept) != 1 || kept[0].Address != "10.100.0.3" {
		t.Fatalf("got %v, want only 10.100.0.3", addresses(kept))
	}

	// Retiring a workstation that is already gone is not an error; an
	// uninstall run twice has to end the same way as an uninstall run once.
	if len(WithoutPeer(existing, "10.100.0.9")) != 2 {
		t.Error("removing an unknown address changed the list")
	}
}
