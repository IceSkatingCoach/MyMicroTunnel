// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// macOS has no kernel WireGuard, so the tunnel is wireguard-go on a utun
// device, configured over its UAPI socket and addressed with ifconfig.

func namePath(name string) string     { return filepath.Join(RunDir, name+".name") }
func socketPath(device string) string { return filepath.Join(RunDir, device+".sock") }

// Device returns the utun the logical interface is currently running on, or ""
// when it is not up.
func Device(name string) string {
	content, err := os.ReadFile(namePath(name))
	if err != nil {
		return ""
	}
	device := strings.TrimSpace(string(content))
	if device == "" {
		return ""
	}
	// The name file outlives a daemon that died; the socket does not.
	if _, err := os.Stat(socketPath(device)); err != nil {
		return ""
	}
	return device
}

// Up raises the interface and applies the configuration.
//
// Re-running it on a tunnel that is already up reconfigures that tunnel rather
// than failing or building a second one. That is what makes the supervisor safe
// to run on a timer and the installer safe to run twice.
func Up(options Options) error {
	if options.Name == "" {
		return errors.New("the tunnel has no name")
	}
	if err := os.MkdirAll(RunDir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %w", RunDir, err)
	}

	device := Device(options.Name)
	if device == "" {
		started, err := start(options)
		if err != nil {
			return err
		}
		device = started
	}

	if err := configure(device, options.Config); err != nil {
		return err
	}
	if err := address(device, options); err != nil {
		return err
	}
	return routes(device, options.Config)
}

// start launches wireguard-go and waits for it to say which device it got.
func start(options Options) (string, error) {
	engine, err := Engine(options.Engine)
	if err != nil {
		return "", err
	}

	// Removed first: a stale name file from a daemon that was killed rather
	// than shut down would be read below as this run's answer.
	_ = os.Remove(namePath(options.Name))

	output, err := runDetached(engine, []string{
		"WG_TUN_NAME_FILE=" + namePath(options.Name),
		"LOG_LEVEL=error",
	}, "utun")
	if err != nil {
		return "", fmt.Errorf("wireguard-go would not start: %w\n%s", err, output)
	}

	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if device := Device(options.Name); device != "" {
			return device, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return "", fmt.Errorf("wireguard-go started but never created %s", namePath(options.Name))
}

func configure(device string, config Config) error {
	request, err := config.UAPIRequest()
	if err != nil {
		return err
	}
	response, err := speak(device, request)
	if err != nil {
		return err
	}
	// The response is only errno; ParseStatus is what turns a non-zero one into
	// a message rather than a silent misconfiguration.
	if _, err := ParseStatus(response); err != nil {
		return fmt.Errorf("configuring %s: %w", device, err)
	}
	return nil
}

func address(device string, options Options) error {
	if options.Address == "" {
		return nil
	}
	// macOS wants the peer address as well as the local one on a point-to-point
	// interface, and wg-quick passes the same address twice for exactly this.
	//
	// The netmask is explicit because ifconfig otherwise falls back to the
	// classful default, and 10.100.0.2 is a class A: the interface then
	// claimed 10.0.0.0/8 — sixteen million addresses — and the first tunnel
	// on a machine made every other private range look occupied. The tunnel
	// owns exactly its own address; the ranges it carries are routes, added
	// separately from AllowedIPs.
	if err := ifconfig(device, "inet", options.Address, options.Address,
		"netmask", "255.255.255.255", "alias"); err != nil {
		return err
	}
	if options.MTU > 0 {
		if err := ifconfig(device, "mtu", fmt.Sprint(options.MTU)); err != nil {
			return err
		}
	}
	return ifconfig(device, "up")
}

// routes sends every range the peers claim at this interface. A route that is
// already there is not an error: Up is expected to be re-run.
func routes(device string, config Config) error {
	for _, peer := range config.Peers {
		for _, allowed := range peer.AllowedIPs {
			allowed = strings.TrimSpace(allowed)
			if allowed == "" {
				continue
			}
			if _, _, err := net.ParseCIDR(allowed); err != nil {
				return fmt.Errorf("%q is not a network this machine can route to", allowed)
			}
			output, err := exec.Command("/sbin/route", "-q", "-n", "add", "-inet", allowed,
				"-interface", device).CombinedOutput()
			if err != nil && !strings.Contains(string(output), "File exists") {
				return fmt.Errorf("routing %s to %s: %w\n%s", allowed, device, err,
					strings.TrimSpace(string(output)))
			}
		}
	}
	return nil
}

// Down drops the interface. Removing the socket is what stops wireguard-go: it
// is the daemon's only listener, and losing it is its shutdown signal. The
// routes and the address go with the device.
func Down(name string) error {
	device := Device(name)
	if device == "" {
		// Already down. An uninstall run twice has to end the same way as an
		// uninstall run once.
		_ = os.Remove(namePath(name))
		return nil
	}

	if err := os.Remove(socketPath(device)); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("removing %s: %w", socketPath(device), err)
	}
	_ = os.Remove(namePath(name))

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("/sbin/ifconfig", device).Run() != nil {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	// The device outliving the socket means the daemon is wedged rather than
	// gone; say so instead of reporting a tunnel that is down and is not.
	return fmt.Errorf("%s still exists after wireguard-go was told to stop", device)
}

// Report asks the running interface what it knows.
func Report(name string) (Status, error) {
	device := Device(name)
	if device == "" {
		return Status{}, nil
	}
	response, err := speak(device, "get=1\n\n")
	if err != nil {
		return Status{}, err
	}
	return ParseStatus(response)
}

func ifconfig(device string, args ...string) error {
	output, err := exec.Command("/sbin/ifconfig", append([]string{device}, args...)...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ifconfig %s %s: %w\n%s", device, strings.Join(args, " "), err,
			strings.TrimSpace(string(output)))
	}
	return nil
}

// AddressPresent reports whether the tunnel's address is assigned to any
// interface on this machine.
//
// It exists because /var/run/wireguard is mode 0700 and root-owned, so an
// unprivileged process — the menu bar app, or `status` run without sudo —
// cannot see the name file or the socket at all. Reading ifconfig proves less
// than a handshake does, but it needs no privilege and it is enough to draw a
// padlock the right way round.
func AddressPresent(address string) bool {
	if address == "" {
		return false
	}
	output, err := exec.Command("/sbin/ifconfig").CombinedOutput()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(output), "\n") {
		fields := strings.Fields(line)
		for index, field := range fields {
			// "inet 10.100.0.2 --> 10.100.0.2 netmask 0xffffffff"
			if field == "inet" && index+1 < len(fields) && fields[index+1] == address {
				return true
			}
		}
	}
	return false
}
