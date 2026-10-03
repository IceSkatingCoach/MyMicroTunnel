// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// fakeRunDir points the tunnel at a run directory of the test's own.
//
// It is relative, with the test inside its parent: a socket path under
// t.TempDir() on macOS is longer than the 104 bytes a Unix socket allows.
func fakeRunDir(t *testing.T) {
	t.Helper()
	t.Chdir(t.TempDir())
	// runDetached stages the daemon's output in a temporary file.
	t.Setenv("TMPDIR", t.TempDir())
	previous := runDir
	runDir = "run"
	t.Cleanup(func() { runDir = previous })
	if err := os.MkdirAll(runDir, 0o700); err != nil {
		t.Fatal(err)
	}
}

// fakeDaemon answers on a device's UAPI socket the way wireguard-go does: one
// request per connection, answered once the caller closes its write half.
type fakeDaemon struct {
	mu       sync.Mutex
	requests []string
}

func listen(t *testing.T, device string, answer string) *fakeDaemon {
	t.Helper()
	listener, err := net.Listen("unix", socketPath(device))
	if err != nil {
		t.Fatal(err)
	}
	daemon := &fakeDaemon{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			connection, err := listener.Accept()
			if err != nil {
				return
			}
			request, _ := io.ReadAll(connection)
			daemon.mu.Lock()
			daemon.requests = append(daemon.requests, string(request))
			daemon.mu.Unlock()
			_, _ = io.WriteString(connection, answer)
			connection.Close()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return daemon
}

func (d *fakeDaemon) heard() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.requests...)
}

func writeName(t *testing.T, name, device string) {
	t.Helper()
	if err := os.WriteFile(namePath(name), []byte(device+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fakeEngine is a wireguard-go that does what the real one does as far as Up
// can tell: it writes the device it got into WG_TUN_NAME_FILE and returns.
// What it was started with is written beside it, for the test to read.
func fakeEngine(t *testing.T, device string) (engine, said string) {
	t.Helper()
	directory := t.TempDir()
	engine = filepath.Join(directory, "wireguard-go")
	said = filepath.Join(directory, "said")
	script := "#!/bin/sh\n" +
		"echo \"$* LOG_LEVEL=$LOG_LEVEL\" > '" + said + "'\n" +
		"echo " + device + " > \"$WG_TUN_NAME_FILE\"\n"
	if err := os.WriteFile(engine, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return engine, said
}

func TestUpStartsWireGuardGoAndConfiguresTheInterface(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, succeed)
	daemon := listen(t, "utun9", "errno=0\n\n")
	engine, said := fakeEngine(t, "utun9")
	config := testConfig(t)

	// A name file left by a daemon that was killed must not be read as this
	// run's answer.
	writeName(t, "wg0", "utun3")

	err := Up(Options{Name: "wg0", Engine: engine, Address: "10.100.0.2", MTU: 1380, Config: config})
	if err != nil {
		t.Fatal(err)
	}

	started, err := os.ReadFile(said)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(started)); got != "utun LOG_LEVEL=error" {
		t.Errorf("wireguard-go was started as %q", got)
	}
	if Device("wg0") != "utun9" {
		t.Errorf("Device(wg0) = %q after Up", Device("wg0"))
	}

	want, _ := config.UAPIRequest()
	if heard := daemon.heard(); len(heard) != 1 || heard[0] != want {
		t.Errorf("the daemon was sent %q, want %q", heard, want)
	}

	expectCommands(t, commands,
		"ifconfig utun9 inet 10.100.0.2 10.100.0.2 netmask 255.255.255.255 alias",
		"ifconfig utun9 mtu 1380",
		"ifconfig utun9 up",
		"route -q -n add -inet 10.100.0.0/24 -interface utun9",
		"route -q -n add -inet 172.31.0.0/16 -interface utun9",
	)
}

func TestUpReconfiguresATunnelThatIsAlreadyUp(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, func(call invocation) (string, int) {
		// The routes are still there from the first time.
		if filepath.Base(call.name) == "route" {
			return "add net 10.100.0.0: gateway utun4: File exists", 1
		}
		return "", 0
	})
	daemon := listen(t, "utun4", "errno=0\n\n")
	writeName(t, "wg0", "utun4")

	// An engine that does not exist proves nothing tried to start a second one.
	err := Up(Options{Name: "wg0", Engine: "/nonexistent/wireguard-go", Config: Config{
		Peers: []Peer{{PublicKey: key(t), AllowedIPs: []string{"10.100.0.0/24"}}},
	}})
	if err != nil {
		t.Fatalf("re-running Up on a live tunnel failed: %v", err)
	}
	if len(daemon.heard()) != 1 {
		t.Errorf("the live tunnel was configured %d times, want once", len(daemon.heard()))
	}
	// No address was asked for, so ifconfig has nothing to do.
	expectCommands(t, commands, "route -q -n add -inet 10.100.0.0/24 -interface utun4")
}

func TestUpRefusesATunnelWithNoName(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, succeed)
	if err := Up(Options{}); err == nil {
		t.Error("a tunnel with no name was raised")
	}
	expectCommands(t, commands)
}

func TestUpSaysWhyTheRunDirectoryCannotBeMade(t *testing.T) {
	fakeRunDir(t)
	if err := os.WriteFile("blocker", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	runDir = filepath.Join("blocker", "run")

	err := Up(Options{Name: "wg0"})
	if err == nil || !strings.Contains(err.Error(), "creating "+runDir) {
		t.Errorf("err = %v, want one naming %s", err, runDir)
	}
}

func TestUpReportsAnEngineThatCannotBeFound(t *testing.T) {
	fakeRunDir(t)
	err := Up(Options{Name: "wg0", Engine: "/nonexistent/wireguard-go"})
	if err == nil || !strings.Contains(err.Error(), "/nonexistent/wireguard-go") {
		t.Errorf("err = %v, want one naming the missing engine", err)
	}
}

func TestUpPassesOnWhyWireGuardGoWouldNotStart(t *testing.T) {
	fakeRunDir(t)
	engine := filepath.Join(t.TempDir(), "wireguard-go")
	if err := os.WriteFile(engine, []byte("#!/bin/sh\necho 'cannot open utun' >&2\nexit 1\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	err := Up(Options{Name: "wg0", Engine: engine})
	if err == nil {
		t.Fatal("an engine that exited 1 was reported as started")
	}
	for _, want := range []string{"wireguard-go would not start", "cannot open utun"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
}

func TestUpStopsWhenTheInterfaceRefusesTheConfiguration(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, succeed)
	listen(t, "utun4", "errno=22\n\n")
	writeName(t, "wg0", "utun4")

	err := Up(Options{Name: "wg0", Address: "10.100.0.2", Config: testConfig(t)})
	if err == nil || !strings.Contains(err.Error(), "configuring utun4") ||
		!strings.Contains(err.Error(), "errno 22") {
		t.Errorf("err = %v, want the interface's errno", err)
	}
	// An interface that is not configured is not given an address either.
	expectCommands(t, commands)
}

func TestUpRefusesAConfigurationWithABadKey(t *testing.T) {
	fakeRunDir(t)
	daemon := listen(t, "utun4", "errno=0\n\n")
	writeName(t, "wg0", "utun4")

	if err := Up(Options{Name: "wg0", Config: Config{PrivateKey: "short"}}); err == nil {
		t.Error("a configuration with a malformed key was applied")
	}
	if len(daemon.heard()) != 0 {
		t.Error("a malformed configuration reached the daemon")
	}
}

func TestUpReportsADaemonThatIsNotAnswering(t *testing.T) {
	fakeRunDir(t)
	writeName(t, "wg0", "utun4")
	// The socket file outlived the daemon that was listening on it.
	if err := os.WriteFile(socketPath("utun4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	err := Up(Options{Name: "wg0", Config: testConfig(t)})
	if err == nil || !strings.Contains(err.Error(), "utun4 is not answering") {
		t.Errorf("err = %v, want one saying the daemon is not answering", err)
	}
}

func TestUpReportsWhichAddressingStepFailed(t *testing.T) {
	for _, failing := range []string{"inet", "mtu", "up"} {
		t.Run(failing, func(t *testing.T) {
			fakeRunDir(t)
			commands := fakeCommandsFor(t, func(call invocation) (string, int) {
				if len(call.args) > 1 && call.args[1] == failing {
					return "ifconfig: refused", 1
				}
				return "", 0
			})
			listen(t, "utun4", "errno=0\n\n")
			writeName(t, "wg0", "utun4")

			err := Up(Options{Name: "wg0", Address: "10.100.0.2", MTU: 1380, Config: testConfig(t)})
			if err == nil {
				t.Fatalf("ifconfig %s failed and Up succeeded", failing)
			}
			for _, want := range []string{"ifconfig utun4 " + failing, "ifconfig: refused"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the error has no %q: %v", want, err)
				}
			}
			for _, issued := range commands.issued() {
				if strings.HasPrefix(issued, "route") {
					t.Errorf("a route was added to an interface that has no address: %s", issued)
				}
			}
		})
	}
}

func TestUpReportsARouteThatCannotBeAdded(t *testing.T) {
	fakeRunDir(t)
	fakeCommandsFor(t, func(call invocation) (string, int) {
		if filepath.Base(call.name) == "route" {
			return "route: writing to routing socket: Network is unreachable", 1
		}
		return "", 0
	})
	listen(t, "utun4", "errno=0\n\n")
	writeName(t, "wg0", "utun4")

	err := Up(Options{Name: "wg0", Config: testConfig(t)})
	if err == nil {
		t.Fatal("a failed route was reported as success")
	}
	for _, want := range []string{"routing 10.100.0.0/24 to utun4", "Network is unreachable"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error has no %q: %v", want, err)
		}
	}
}

func TestUpRefusesARangeThatIsNotANetwork(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, succeed)
	listen(t, "utun4", "errno=0\n\n")
	writeName(t, "wg0", "utun4")

	err := Up(Options{Name: "wg0", Config: Config{
		Peers: []Peer{{PublicKey: key(t), AllowedIPs: []string{"10.100.0.300/24"}}},
	}})
	if err == nil || !strings.Contains(err.Error(), "10.100.0.300/24") {
		t.Errorf("err = %v, want one naming the bad range", err)
	}
	expectCommands(t, commands)
}

func TestDeviceIgnoresANameFileWhoseDaemonHasGone(t *testing.T) {
	fakeRunDir(t)

	if Device("wg0") != "" || IsUp("wg0") {
		t.Error("a tunnel with no name file is up")
	}

	writeName(t, "wg0", "")
	if Device("wg0") != "" {
		t.Error("an empty name file names a device")
	}

	writeName(t, "wg0", "utun4")
	if Device("wg0") != "" {
		t.Error("a name file with no socket beside it names a device")
	}

	if err := os.WriteFile(socketPath("utun4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if Device("wg0") != "utun4" || !IsUp("wg0") {
		t.Errorf("Device(wg0) = %q, want utun4", Device("wg0"))
	}
}

func TestDownRemovesTheSocketAndWaitsForTheDevice(t *testing.T) {
	fakeRunDir(t)
	// ifconfig failing is the device having gone.
	commands := fakeCommandsFor(t, func(invocation) (string, int) {
		return "ifconfig: interface utun4 does not exist", 1
	})
	writeName(t, "wg0", "utun4")
	if err := os.WriteFile(socketPath("utun4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Down("wg0"); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{socketPath("utun4"), namePath("wg0")} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s is still there after Down", path)
		}
	}
	expectCommands(t, commands, "ifconfig utun4")
}

func TestDownTwiceEndsTheSameWayAsOnce(t *testing.T) {
	fakeRunDir(t)
	commands := fakeCommandsFor(t, succeed)
	// A name file with no socket is what a crashed daemon leaves.
	writeName(t, "wg0", "utun4")

	if err := Down("wg0"); err != nil {
		t.Fatal(err)
	}
	if err := Down("wg0"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(namePath("wg0")); !errors.Is(err, os.ErrNotExist) {
		t.Error("the stale name file survived Down")
	}
	expectCommands(t, commands)
}

func TestDownReportsASocketItCannotRemove(t *testing.T) {
	fakeRunDir(t)
	writeName(t, "wg0", "utun4")
	// A non-empty directory where the socket should be cannot be removed.
	if err := os.MkdirAll(filepath.Join(socketPath("utun4"), "inside"), 0o700); err != nil {
		t.Fatal(err)
	}

	err := Down("wg0")
	if err == nil || !strings.Contains(err.Error(), "removing "+socketPath("utun4")) {
		t.Errorf("err = %v, want one naming the socket", err)
	}
}

func TestReportAsksTheDaemonForItsStatus(t *testing.T) {
	fakeRunDir(t)
	daemon := listen(t, "utun4", "listen_port=51820\npublic_key="+strings.Repeat("ab", 32)+
		"\nrx_bytes=10\nerrno=0\n\n")
	writeName(t, "wg0", "utun4")

	status, err := Report("wg0")
	if err != nil {
		t.Fatal(err)
	}
	if heard := daemon.heard(); len(heard) != 1 || heard[0] != "get=1\n\n" {
		t.Errorf("the daemon was asked %q, want a get", heard)
	}
	if status.ListenPort != 51820 || len(status.Peers) != 1 || status.Peers[0].ReceivedBytes != 10 {
		t.Errorf("status = %+v", status)
	}
}

func TestReportOfATunnelThatIsDownIsEmpty(t *testing.T) {
	fakeRunDir(t)
	status, err := Report("wg0")
	if err != nil || status.ListenPort != 0 || len(status.Peers) != 0 {
		t.Errorf("Report = %+v, %v; want an empty status", status, err)
	}
}

func TestReportSaysWhenTheDaemonIsNotAnswering(t *testing.T) {
	fakeRunDir(t)
	writeName(t, "wg0", "utun4")
	if err := os.WriteFile(socketPath("utun4"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Report("wg0"); err == nil {
		t.Error("a daemon that is not listening reported a status")
	}
}

func TestAddressPresentReadsIfconfig(t *testing.T) {
	commands := fakeCommandsFor(t, func(invocation) (string, int) {
		return "lo0: flags=8049<UP,LOOPBACK,RUNNING,MULTICAST> mtu 16384\n" +
			"\tinet 127.0.0.1 netmask 0xff000000\n" +
			"utun4: flags=8051<UP,POINTOPOINT,RUNNING,MULTICAST> mtu 1380\n" +
			"\tinet 10.100.0.2 --> 10.100.0.2 netmask 0xffffffff\n", 0
	})

	if !AddressPresent("10.100.0.2") {
		t.Error("an address ifconfig lists was not found")
	}
	// Only an inet field counts, not the peer side of the arrow.
	if AddressPresent("10.100.0.3") || AddressPresent("0xffffffff") {
		t.Error("an address ifconfig does not list was found")
	}
	if AddressPresent("") {
		t.Error("the empty address was found")
	}
	expectCommands(t, commands, "ifconfig ", "ifconfig ", "ifconfig ")
}

func TestAddressPresentIsFalseWhenIfconfigFails(t *testing.T) {
	fakeCommandsFor(t, func(invocation) (string, int) {
		return "\tinet 10.100.0.2 --> 10.100.0.2", 1
	})
	if AddressPresent("10.100.0.2") {
		t.Error("an address was found in the output of an ifconfig that failed")
	}
}
