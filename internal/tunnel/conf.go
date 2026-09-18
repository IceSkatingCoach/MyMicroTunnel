// SPDX-License-Identifier: GPL-3.0-or-later
package tunnel

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// File is /etc/wireguard/<name>.conf: the tunnel as it is written down.
//
// The format is wg-quick's, not a private one, for two reasons. It is the
// format every piece of WireGuard documentation shows, so an operator reading
// the file recognises it; and a machine that does have the Homebrew tools
// installed can still run `wg-quick up wg0` against it, which is a useful thing
// to be able to fall back on when something is wrong.
type File struct {
	Address string
	MTU     int

	// PrivateKeyPath is where the key actually lives. The config does not
	// contain the key itself: it is written as a PostUp line, which is both how
	// wg-quick loads it and how this package finds it, so there is exactly one
	// file on disk holding the secret.
	PrivateKeyPath string

	Config Config
}

// Marshal renders the file. The comments are part of the output on purpose: the
// next person to read this file will be reading it because something is broken.
func Marshal(file File) string {
	lines := []string{
		"[Interface]",
		"Address = " + file.Address + "/32",
	}
	if file.MTU > 0 {
		lines = append(lines, "MTU = "+strconv.Itoa(file.MTU))
	}
	if file.PrivateKeyPath != "" {
		lines = append(lines,
			"# Reads the key from the file the installer created, so the private key is",
			"# not duplicated into this config.",
			"PostUp = wg set %i private-key "+file.PrivateKeyPath,
		)
	}

	for _, peer := range file.Config.Peers {
		lines = append(lines, "", "[Peer]", "PublicKey = "+peer.PublicKey)
		if len(peer.AllowedIPs) > 0 {
			lines = append(lines, "AllowedIPs = "+strings.Join(peer.AllowedIPs, ", "))
		}
		if peer.Endpoint != "" {
			lines = append(lines, "Endpoint = "+peer.Endpoint)
		}
		if peer.PersistentKeepalive > 0 {
			lines = append(lines,
				"# Keeps the NAT mapping open, and re-pins the tunnel when this machine's",
				"# public address changes.",
				"PersistentKeepalive = "+strconv.Itoa(peer.PersistentKeepalive),
			)
		}
	}

	return strings.Join(append(lines, ""), "\n")
}

// Parse reads the file back. Unknown keys are ignored rather than rejected,
// because a hand-edited config with an extra line in it should still raise a
// tunnel rather than refuse to.
func Parse(text string) (File, error) {
	var (
		file    File
		section string
		peer    *Peer
	)

	for number, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.ToLower(strings.Trim(line, "[]"))
			if section == "peer" {
				file.Config.Peers = append(file.Config.Peers, Peer{})
				peer = &file.Config.Peers[len(file.Config.Peers)-1]
			}
			continue
		}

		key, value, found := strings.Cut(line, "=")
		if !found {
			return File{}, fmt.Errorf("line %d is neither a section nor a setting: %q", number+1, raw)
		}
		key = strings.ToLower(strings.TrimSpace(key))
		value = strings.TrimSpace(value)

		switch section {
		case "interface":
			switch key {
			case "address":
				// Stored without the prefix length: the interface is a /32 on a
				// point-to-point link, and carrying the suffix around only
				// creates two spellings of one address.
				file.Address = strings.TrimSpace(strings.Split(value, "/")[0])
			case "mtu":
				file.MTU, _ = strconv.Atoi(value)
			case "listenport":
				file.Config.ListenPort, _ = strconv.Atoi(value)
			case "privatekey":
				file.Config.PrivateKey = value
			case "postup":
				// `wg set %i private-key /path` — wg-quick's way of loading a
				// key that is not in this file, and this package's way of
				// finding out where it is.
				if fields := strings.Fields(value); len(fields) >= 4 {
					for index, field := range fields {
						if field == "private-key" && index+1 < len(fields) {
							file.PrivateKeyPath = fields[index+1]
						}
					}
				}
			}

		case "peer":
			if peer == nil {
				continue
			}
			switch key {
			case "publickey":
				peer.PublicKey = value
			case "endpoint":
				peer.Endpoint = value
			case "persistentkeepalive":
				peer.PersistentKeepalive, _ = strconv.Atoi(value)
			case "allowedips":
				for _, allowed := range strings.Split(value, ",") {
					if allowed = strings.TrimSpace(allowed); allowed != "" {
						peer.AllowedIPs = append(peer.AllowedIPs, allowed)
					}
				}
			}
		}
	}

	return file, nil
}

// Load reads a config and the key it points at, returning something ready to
// hand to Up.
func Load(path string) (File, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		return File{}, err
	}

	file, err := Parse(string(content))
	if err != nil {
		return File{}, fmt.Errorf("%s: %w", path, err)
	}

	if file.Config.PrivateKey == "" && file.PrivateKeyPath != "" {
		key, err := os.ReadFile(file.PrivateKeyPath)
		if err != nil {
			return File{}, fmt.Errorf("%s names a key at %s that cannot be read: %w",
				path, file.PrivateKeyPath, err)
		}
		file.Config.PrivateKey = strings.TrimSpace(string(key))
	}
	if file.Config.PrivateKey == "" {
		return File{}, fmt.Errorf("%s has no private key and names no file holding one", path)
	}

	return file, nil
}
