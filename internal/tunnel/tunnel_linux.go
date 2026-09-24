// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Linux has WireGuard in the kernel, so there is no daemon to start and no
// socket to talk to. The interface is created with `ip`, configured with `wg`
// from wireguard-tools, and carries the logical name itself: unlike a utun, a
// Linux interface can be called wg0.

// Device returns the interface when it exists, or "" when it is not up.
func Device(name string) string {
	if name == "" {
		return ""
	}
	// Only a WireGuard interface counts. An unrelated wg0 — a hand-made
	// wg-quick tunnel, say — is not this product's to reconfigure.
	kind, err := os.ReadFile(filepath.Join("/sys/class/net", name, "uevent"))
	if err != nil || !strings.Contains(string(kind), "DEVTYPE=wireguard") {
		return ""
	}
	return name
}

// Up raises the interface and applies the configuration.
//
// Re-running it on a tunnel that is already up reconfigures that tunnel rather
// than failing or building a second one, as on macOS.
func Up(options Options) error {
	if options.Name == "" {
		return errors.New("the tunnel has no name")
	}

	if Device(options.Name) == "" {
		if err := ip("link", "add", "dev", options.Name, "type", "wireguard"); err != nil {
			return fmt.Errorf("%w\n  Is the wireguard kernel module available? Try `modprobe wireguard`.", err)
		}
	}

	if err := setconf(options.Name, options.Config); err != nil {
		return err
	}
	if options.Address != "" {
		// A /32, for the reason the macOS side spells out: the tunnel owns
		// exactly its own address, and the ranges it carries are routes.
		if err := ip("address", "replace", options.Address+"/32", "dev", options.Name); err != nil {
			return err
		}
	}
	if options.MTU > 0 {
		if err := ip("link", "set", "dev", options.Name, "mtu", strconv.Itoa(options.MTU)); err != nil {
			return err
		}
	}
	if err := ip("link", "set", "dev", options.Name, "up"); err != nil {
		return err
	}
	return routes(options.Name, options.Config)
}

// setconf hands the configuration to `wg` on stdin, so the private key is
// never written to a second file.
func setconf(device string, config Config) error {
	command := exec.Command(wgTool(), "setconf", device, "/dev/stdin")
	command.Stdin = strings.NewReader(SetConf(config))
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("configuring %s: %w\n%s", device, err, strings.TrimSpace(string(output)))
	}
	return nil
}

// routes sends every range the peers claim at this interface. `replace` rather
// than `add`, so a re-run is not an error.
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
			if err := ip("route", "replace", allowed, "dev", device); err != nil {
				return fmt.Errorf("routing %s to %s: %w", allowed, device, err)
			}
		}
	}
	return nil
}

// Down deletes the interface. Its routes and address go with it.
func Down(name string) error {
	if Device(name) == "" {
		return nil
	}
	return ip("link", "delete", "dev", name)
}

// Report asks the kernel what it knows.
func Report(name string) (Status, error) {
	if Device(name) == "" {
		return Status{}, nil
	}
	output, err := exec.Command(wgTool(), "show", name, "dump").CombinedOutput()
	if err != nil {
		return Status{}, fmt.Errorf("wg show %s: %w\n%s", name, err, strings.TrimSpace(string(output)))
	}
	return ParseDump(string(output))
}

// AddressPresent reports whether the tunnel's address is assigned to any
// interface on this machine. Needs no privilege, which is the point: `status`
// run without sudo cannot ask `wg` anything.
func AddressPresent(address string) bool {
	if address == "" {
		return false
	}
	interfaces, err := net.Interfaces()
	if err != nil {
		return false
	}
	for _, device := range interfaces {
		addresses, err := device.Addrs()
		if err != nil {
			continue
		}
		for _, candidate := range addresses {
			if network, ok := candidate.(*net.IPNet); ok && network.IP.String() == address {
				return true
			}
		}
	}
	return false
}

func ip(args ...string) error {
	output, err := exec.Command(ipTool(), args...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("ip %s: %w\n%s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return nil
}

// The tools are looked up at fixed paths first: the supervisor runs under
// systemd with a minimal PATH, and sudo resets PATH to secure_path.
func ipTool() string { return firstTool("ip", "/usr/sbin/ip", "/usr/bin/ip", "/sbin/ip") }
func wgTool() string { return firstTool("wg", "/usr/bin/wg", "/usr/sbin/wg") }

func firstTool(name string, candidates ...string) string {
	for _, candidate := range candidates {
		if executable(candidate) {
			return candidate
		}
	}
	if found, err := exec.LookPath(name); err == nil {
		return found
	}
	return name
}

// Tools reports which of the commands the Linux tunnel needs are missing.
func Tools() []string {
	var missing []string
	for name, path := range map[string]string{"ip (iproute2)": ipTool(), "wg (wireguard-tools)": wgTool()} {
		if !filepath.IsAbs(path) {
			missing = append(missing, name)
		}
	}
	return missing
}
