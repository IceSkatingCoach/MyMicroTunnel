// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakeSysClassNet gives the tunnel a /sys/class/net of the test's own, holding
// the interfaces named, each with the uevent given.
func fakeSysClassNet(t *testing.T, interfaces map[string]string) {
	t.Helper()
	directory := t.TempDir()
	for name, uevent := range interfaces {
		if err := os.MkdirAll(filepath.Join(directory, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, name, "uevent"), []byte(uevent), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	previous := sysClassNet
	sysClassNet = directory
	t.Cleanup(func() { sysClassNet = previous })
}

const wireguardUevent = "DEVTYPE=wireguard\nINTERFACE=wg0\nIFINDEX=7\n"

func TestUpCreatesConfiguresAndRoutesTheInterface(t *testing.T) {
	fakeSysClassNet(t, nil)
	commands := fakeCommandsFor(t, succeed)
	config := testConfig(t)

	if err := Up(Options{Name: "wg0", Address: "10.100.0.2", MTU: 1380, Config: config}); err != nil {
		t.Fatal(err)
	}

	expectCommands(t, commands,
		"ip link add dev wg0 type wireguard",
		"wg setconf wg0 /dev/stdin",
		"ip address replace 10.100.0.2/32 dev wg0",
		"ip link set dev wg0 mtu 1380",
		"ip link set dev wg0 up",
		"ip route replace 10.100.0.0/24 dev wg0",
		"ip route replace 172.31.0.0/16 dev wg0",
	)
	// The private key travels on stdin, never in a second file.
	if got := commands.stdin(1); got != SetConf(config) {
		t.Errorf("wg setconf read %q, want %q", got, SetConf(config))
	}
}

func TestUpReconfiguresAnInterfaceThatIsAlreadyThere(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	commands := fakeCommandsFor(t, succeed)

	if err := Up(Options{Name: "wg0", Config: Config{Peers: []Peer{{PublicKey: key(t)}}}}); err != nil {
		t.Fatal(err)
	}
	expectCommands(t, commands,
		"wg setconf wg0 /dev/stdin",
		"ip link set dev wg0 up",
	)
}

func TestUpRefusesATunnelWithNoName(t *testing.T) {
	commands := fakeCommandsFor(t, succeed)
	if err := Up(Options{}); err == nil {
		t.Error("a tunnel with no name was raised")
	}
	expectCommands(t, commands)
}

func TestUpSuggestsTheKernelModuleWhenTheInterfaceCannotBeCreated(t *testing.T) {
	fakeSysClassNet(t, nil)
	commands := fakeCommandsFor(t, func(invocation) (string, int) {
		return "Error: Unknown device type.", 2
	})

	err := Up(Options{Name: "wg0", Config: testConfig(t)})
	if err == nil {
		t.Fatal("Up succeeded without an interface")
	}
	for _, want := range []string{"ip link add dev wg0 type wireguard", "Unknown device type", "modprobe wireguard"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
	expectCommands(t, commands, "ip link add dev wg0 type wireguard")
}

func TestUpReportsWhatWgSaidWhenTheConfigurationIsRefused(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	commands := fakeCommandsFor(t, func(call invocation) (string, int) {
		if filepath.Base(call.name) == "wg" {
			return "Key is not the correct length or format", 1
		}
		return "", 0
	})

	err := Up(Options{Name: "wg0", Address: "10.100.0.2", Config: testConfig(t)})
	if err == nil {
		t.Fatal("a refused configuration was reported as success")
	}
	for _, want := range []string{"configuring wg0", "not the correct length"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
	// An interface that is not configured is not given an address either.
	expectCommands(t, commands, "wg setconf wg0 /dev/stdin")
}

func TestUpStopsAtTheFirstStepThatFails(t *testing.T) {
	steps := []string{
		"ip address replace 10.100.0.2/32 dev wg0",
		"ip link set dev wg0 mtu 1380",
		"ip link set dev wg0 up",
		"ip route replace 10.100.0.0/24 dev wg0",
	}
	for index, failing := range steps {
		t.Run(failing, func(t *testing.T) {
			fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
			commands := fakeCommandsFor(t, func(call invocation) (string, int) {
				if call.String() == failing {
					return "RTNETLINK answers: Operation not permitted", 2
				}
				return "", 0
			})

			err := Up(Options{Name: "wg0", Address: "10.100.0.2", MTU: 1380, Config: testConfig(t)})
			if err == nil {
				t.Fatalf("%s failed and Up succeeded", failing)
			}
			for _, want := range []string{failing, "Operation not permitted"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error has no %q: %v", want, err)
				}
			}
			want := append([]string{"wg setconf wg0 /dev/stdin"}, steps[:index+1]...)
			expectCommands(t, commands, want...)
		})
	}
}

func TestUpRefusesARangeThatIsNotANetwork(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	commands := fakeCommandsFor(t, succeed)

	err := Up(Options{Name: "wg0", Config: Config{
		Peers: []Peer{{PublicKey: key(t), AllowedIPs: []string{"10.100.0.300/24"}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "10.100.0.300/24") {
		t.Errorf("err = %v, want one naming the bad range", err)
	}
	for _, issued := range commands.issued() {
		if strings.HasPrefix(issued, "ip route") {
			t.Errorf("a malformed range was routed: %s", issued)
		}
	}
}

func TestDeviceOnlyCountsAWireGuardInterface(t *testing.T) {
	fakeSysClassNet(t, map[string]string{
		"wg0":  wireguardUevent,
		"wg1":  "INTERFACE=wg1\nIFINDEX=8\n",
		"eth0": "INTERFACE=eth0\n",
	})

	cases := map[string]string{"wg0": "wg0", "wg1": "", "eth0": "", "wg9": "", "": ""}
	for name, want := range cases {
		if got := Device(name); got != want {
			t.Errorf("Device(%q) = %q, want %q", name, got, want)
		}
	}
	if !IsUp("wg0") || IsUp("wg1") {
		t.Error("IsUp disagrees with Device")
	}
}

func TestDownDeletesTheInterface(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	commands := fakeCommandsFor(t, succeed)

	if err := Down("wg0"); err != nil {
		t.Fatal(err)
	}
	expectCommands(t, commands, "ip link delete dev wg0")
}

func TestDownOfAnInterfaceThatIsNotThereDoesNothing(t *testing.T) {
	// An unrelated wg1 is not this product's to delete.
	fakeSysClassNet(t, map[string]string{"wg1": "INTERFACE=wg1\n"})
	commands := fakeCommandsFor(t, succeed)

	for _, name := range []string{"wg0", "wg1"} {
		if err := Down(name); err != nil {
			t.Errorf("Down(%s): %v", name, err)
		}
	}
	expectCommands(t, commands)
}

func TestReportReadsWgShowDump(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	commands := fakeCommandsFor(t, func(invocation) (string, int) {
		return "cHJpdmF0ZQ==\tcHVibGlj\t51820\toff\n" +
			"cGVlcg==\t(none)\t203.0.113.10:51820\t10.100.0.0/24\t1700000000\t2048\t1024\t25\n", 0
	})

	status, err := Report("wg0")
	if err != nil {
		t.Fatal(err)
	}
	expectCommands(t, commands, "wg show wg0 dump")
	if status.ListenPort != 51820 || len(status.Peers) != 1 ||
		status.Peers[0].Endpoint != "203.0.113.10:51820" || status.Peers[0].ReceivedBytes != 2048 {
		t.Errorf("status = %+v", status)
	}
}

func TestReportOfAnInterfaceThatIsNotThereIsEmpty(t *testing.T) {
	fakeSysClassNet(t, nil)
	commands := fakeCommandsFor(t, succeed)

	status, err := Report("wg0")
	if err != nil || status.ListenPort != 0 || len(status.Peers) != 0 {
		t.Errorf("Report = %+v, %v; want an empty status", status, err)
	}
	expectCommands(t, commands)
}

func TestReportSaysWhatWgSaidWhenItFails(t *testing.T) {
	fakeSysClassNet(t, map[string]string{"wg0": wireguardUevent})
	fakeCommandsFor(t, func(invocation) (string, int) {
		return "Unable to access interface: Operation not permitted", 1
	})

	_, err := Report("wg0")
	if err == nil {
		t.Fatal("a failed wg show was reported as a status")
	}
	for _, want := range []string{"wg show wg0", "Operation not permitted"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
}

func TestAddressPresentLooksAtEveryInterface(t *testing.T) {
	if !AddressPresent("127.0.0.1") {
		t.Error("the loopback address was not found")
	}
	if AddressPresent("192.0.2.123") {
		t.Error("a documentation address was found on this machine")
	}
	if AddressPresent("") {
		t.Error("the empty address was found")
	}
}

func TestFirstToolPrefersTheFixedPaths(t *testing.T) {
	directory := t.TempDir()
	fixed := writeExecutable(t, filepath.Join(directory, "ip"))
	onPath := t.TempDir()
	writeExecutable(t, filepath.Join(onPath, "ip"))
	t.Setenv("PATH", onPath)

	missing := filepath.Join(directory, "missing")
	if got := firstTool("ip", missing, fixed); got != fixed {
		t.Errorf("firstTool = %q, want the fixed path %q", got, fixed)
	}
	// systemd and sudo both reset PATH, so it is only the fallback.
	if got := firstTool("ip", missing); got != filepath.Join(onPath, "ip") {
		t.Errorf("firstTool = %q, want the one on PATH", got)
	}
	// A bare name is what Tools reports as missing.
	t.Setenv("PATH", t.TempDir())
	if got := firstTool("ip", missing); got != "ip" {
		t.Errorf("firstTool = %q, want the bare name", got)
	}
}

func TestToolsReportsWhatCouldNotBeFound(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	missing := Tools()

	for name, path := range map[string]string{"ip (iproute2)": ipTool(), "wg (wireguard-tools)": wgTool()} {
		if filepath.IsAbs(path) == slices.Contains(missing, name) {
			t.Errorf("%s resolves to %q, and Tools reports missing %q", name, path, missing)
		}
	}
}
