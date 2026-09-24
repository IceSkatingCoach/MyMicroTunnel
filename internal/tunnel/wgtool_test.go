// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestSetConfIsWhatWgReads(t *testing.T) {
	got := SetConf(Config{
		PrivateKey: "cHJpdmF0ZQ==",
		Peers: []Peer{{
			PublicKey:           "cHVibGlj",
			Endpoint:            "203.0.113.7:51820",
			AllowedIPs:          []string{"10.100.0.1/32", "10.0.0.0/16"},
			PersistentKeepalive: 25,
		}},
	})
	want := "[Interface]\nPrivateKey = cHJpdmF0ZQ==\n\n[Peer]\nPublicKey = cHVibGlj\n" +
		"AllowedIPs = 10.100.0.1/32, 10.0.0.0/16\nEndpoint = 203.0.113.7:51820\nPersistentKeepalive = 25\n"
	if got != want {
		t.Errorf("got\n%s\nwant\n%s", got, want)
	}
	// wg setconf rejects wg-quick's keys outright.
	for _, key := range []string{"Address", "MTU", "PostUp"} {
		if strings.Contains(got, key) {
			t.Errorf("%s is a wg-quick key and makes `wg setconf` fail", key)
		}
	}
}

func TestParseDumpReadsPeers(t *testing.T) {
	now := time.Now().Unix()
	dump := "priv\tpub\t51820\toff\n" +
		"bPeer\t(none)\t(none)\t10.0.0.0/16\t0\t0\t0\t25\n" +
		"aPeer\t(none)\t203.0.113.7:51820\t10.100.0.1/32\t" + strconv.FormatInt(now, 10) + "\t1024\t2048\t25\n"
	status, err := ParseDump(dump)
	if err != nil {
		t.Fatal(err)
	}
	if status.ListenPort != 51820 || len(status.Peers) != 2 {
		t.Fatalf("got %+v", status)
	}
	connected, silent := status.Peers[0], status.Peers[1]
	if connected.PublicKey != "aPeer" || connected.Endpoint != "203.0.113.7:51820" ||
		connected.ReceivedBytes != 1024 || connected.SentBytes != 2048 {
		t.Errorf("first peer read wrongly: %+v", connected)
	}
	if !connected.Connected(time.Now()) {
		t.Error("a fresh handshake should count as connected")
	}
	if silent.Endpoint != "" || !silent.LastHandshake.IsZero() || silent.Connected(time.Now()) {
		t.Errorf("a peer that never handshook should be disconnected with no endpoint: %+v", silent)
	}
}

func TestParseDumpRefusesNothing(t *testing.T) {
	if _, err := ParseDump(""); err == nil {
		t.Error("an empty dump should be an error, not a tunnel with no peers")
	}
}
