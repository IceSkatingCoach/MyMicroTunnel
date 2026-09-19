// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
)

// A VPN profile is one deployment as this machine sees it: one stack, one
// WireGuard interface, one tunnel subnet, one set of exposed ports, one
// desired-state file.
//
// The single-deployment version of this product kept all of that in fixed
// paths — one config.json, one desired-state, /etc/wireguard/client.key — which
// made a second deployment on the same laptop impossible: installing it
// overwrote the first one's config and left a tunnel nobody could describe.
// Everything a profile owns now hangs off its name:
//
//	~/Library/Application Support/MyMicroTunnel/profiles/<name>/config.json
//	                                                           /settings.json
//	                                                           /client.pub
//	                                                           /desired-state
//
// What stays shared is the parts of the machine there is only one of: the
// sudoers file, the helper, and the supervisor daemon. Each of those is
// written from *every* profile rather than from the one being installed, which
// is why installing a second profile rewrites all three.

const DefaultProfileName = "default"

func AppConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "MyMicroTunnel")
}

func ProfilesDir() string {
	return filepath.Join(AppConfigDir(), "profiles")
}

// ProfileDir is everything one profile owns. An empty name is the default
// profile rather than the root of the store, because a caller that forgot to
// set the name should not end up writing config.json over the profiles
// directory.
func ProfileDir(name string) string {
	if name == "" {
		name = DefaultProfileName
	}
	return filepath.Join(ProfilesDir(), name)
}

func ProfileConfigPath(name string) string {
	return filepath.Join(ProfileDir(name), "config.json")
}

// ProfileSettingsPath holds the whole deployment, not just the part the app
// reads. It is what lets `peers`, `diagnose` and `uninstall` work out which
// stack a profile means without being told again on the command line.
func ProfileSettingsPath(name string) string {
	return filepath.Join(ProfileDir(name), "settings.json")
}

// PublicKeyPath records the public half where the user can read it. Public keys
// are not secret, and keeping a copy outside /etc/wireguard is what lets a
// re-run recognise an existing tunnel without asking for root first. Without
// it, a non-interactive run would mint a new keypair and register a second peer
// whose private half this machine does not have.
func PublicKeyPath(profileName string) string {
	return filepath.Join(ProfileDir(profileName), "client.pub")
}

func DesiredStatePathFor(profileName string) string {
	return filepath.Join(ProfileDir(profileName), "desired-state")
}

func (s Settings) DesiredStatePath() string {
	return DesiredStatePathFor(s.ProfileName)
}

// ListProfiles names every profile this machine holds, in a stable order so
// two runs of `profiles` print the same thing.
func ListProfiles() []string {
	entries, err := os.ReadDir(ProfilesDir())
	if err != nil {
		return nil
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		// A directory with no config is a half-written install, not a profile
		// anything should act on.
		if _, err := os.Stat(ProfileConfigPath(entry.Name())); err != nil {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

// SaveProfileSettings records the deployment beside the app's own config.
//
// Secrets are not in Settings — the access key has a `json:"-"` tag and the
// private key never leaves /etc/wireguard — so this file is the deployment's
// description rather than its credentials. It is still 0600: it names the
// account, the region and the stack.
func SaveProfileSettings(s Settings) error {
	if err := os.MkdirAll(ProfileDir(s.ProfileName), 0o755); err != nil {
		return err
	}
	return s.Write(ProfileSettingsPath(s.ProfileName))
}

func LoadProfileSettings(name string) (Settings, error) {
	return ReadSettings(ProfileSettingsPath(name))
}

// AllProfileSettings is what the machine-wide files are generated from: the
// sudoers rule needs a line per interface, and the supervisor needs one
// reconcile loop per profile.
func AllProfileSettings() []Settings {
	var all []Settings
	for _, name := range ListProfiles() {
		s, err := LoadProfileSettings(name)
		if err != nil {
			// A profile whose settings cannot be read still has an app config,
			// which carries the interface — the one field the sudoers rule and
			// the supervisor actually need.
			config := installedTunnel(name)
			s = Settings{
				ProfileName:   name,
				InterfaceName: config.InterfaceName,
				ClientAddress: config.ClientAddress,
				Supervise:     config.Supervised,
			}
		}
		if s.ProfileName == "" {
			s.ProfileName = name
		}
		all = append(all, s)
	}
	return all
}

// installedTunnel reads one profile's app config, so an uninstall — and the
// diagnostics — use the interface and helper the install actually chose rather
// than guessing defaults.
//
// The whole decoded value is kept. An earlier version copied four fields out of
// it and left everything else zero, which made `diagnose` report a supervised
// deployment as unsupervised and a configured health check as absent.
func installedTunnel(profileName string) appConfig {
	defaults := Defaults()
	fallback := appConfig{
		ProfileName:   profileName,
		InterfaceName: defaults.InterfaceName,
		ClientAddress: defaults.ClientAddress,
		HelperPath:    HelperPath,
	}

	content, err := os.ReadFile(ProfileConfigPath(profileName))
	if err != nil {
		return fallback
	}

	var stored appConfig
	if err := json.Unmarshal(content, &stored); err != nil {
		return fallback
	}

	// Only the fields the file left empty fall back.
	if stored.ProfileName == "" {
		stored.ProfileName = profileName
	}
	if stored.InterfaceName == "" {
		stored.InterfaceName = fallback.InterfaceName
	}
	if stored.ClientAddress == "" {
		stored.ClientAddress = fallback.ClientAddress
	}
	if stored.HelperPath == "" {
		stored.HelperPath = fallback.HelperPath
	}
	return stored
}

// InstalledTunnelOf is the interface and address one profile actually uses,
// read from what the installer wrote rather than from the defaults.
func InstalledTunnelOf(profileName string) (interfaceName, clientAddress string) {
	config := installedTunnel(profileName)
	return config.InterfaceName, config.ClientAddress
}

// --- choosing what a new profile may use -----------------------------------

// NextFreeInterface picks a WireGuard interface no other profile on this
// machine is using. Two profiles on one interface would be two tunnels writing
// the same /etc/wireguard/wg0.conf, and the second install would take the first
// deployment down without saying so.
func NextFreeInterface(taken []string) string {
	used := map[string]bool{}
	for _, name := range taken {
		used[name] = true
	}
	for index := 0; index < 64; index++ {
		candidate := "wg" + strconv.Itoa(index)
		if !used[candidate] {
			return candidate
		}
	}
	return ""
}

// TakenInterfaces is what NextFreeInterface has to avoid: every interface the
// other profiles already claim. The profile being installed is excluded, so
// re-running its own install does not move it to a new interface.
func TakenInterfaces(except string) []string {
	var taken []string
	for _, s := range AllProfileSettings() {
		if s.ProfileName == except || s.InterfaceName == "" {
			continue
		}
		taken = append(taken, s.InterfaceName)
	}
	return taken
}

// ConflictsWithOtherProfiles refuses the collisions that produce a deployment
// which looks installed and does not work: two profiles on one interface, two
// profiles claiming one tunnel address, and — the one that is easiest to
// create and hardest to diagnose — two profiles whose tunnel subnets overlap,
// which makes the routing table send one deployment's traffic down the other's
// tunnel for as long as both are up.
func (s Settings) ConflictsWithOtherProfiles() error {
	var problems []string
	_, mine, cidrErr := net.ParseCIDR(s.VpnCidr)

	for _, other := range AllProfileSettings() {
		if other.ProfileName == s.ProfileName {
			continue
		}
		if other.InterfaceName == s.InterfaceName {
			problems = append(problems, fmt.Sprintf(
				"profile %q already uses the interface %s", other.ProfileName, other.InterfaceName))
		}
		if other.ClientAddress == s.ClientAddress {
			problems = append(problems, fmt.Sprintf(
				"profile %q already uses the tunnel address %s; give this one another --client-ip or another --vpn-cidr",
				other.ProfileName, other.ClientAddress))
		}
		if cidrErr == nil {
			if _, theirs, err := net.ParseCIDR(other.VpnCidr); err == nil && tunnel.Overlaps(mine, theirs) {
				problems = append(problems, fmt.Sprintf(
					"profile %q already tunnels %s, which overlaps %s; give this one a --vpn-cidr of its own",
					other.ProfileName, other.VpnCidr, s.VpnCidr))
			}
		}
		if other.StackName != "" && other.StackName == s.StackName && other.Region == s.Region {
			problems = append(problems, fmt.Sprintf(
				"profile %q is already this machine's name for the stack %s in %s",
				other.ProfileName, other.StackName, other.Region))
		}
	}
	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this profile collides with another one on this machine:\n  · %s",
		strings.Join(problems, "\n  · "))
}

// ConflictsWithLocalNetworks refuses a deployment whose tunnel subnet is a
// network this machine is already attached to.
//
// Checked at install time as well as at every raise, because the answer is
// different at the two moments. Here it is "choose another --vpn-cidr", and
// choosing it costs nothing because the stack has not been built yet; at raise
// time the range is already in every workstation's config and the honest
// answer is to move off the network instead.
func (s Settings) ConflictsWithLocalNetworks() error {
	local, err := tunnel.LocalNetworks([]string{s.InterfaceName, tunnel.Device(s.InterfaceName)})
	if err != nil {
		return nil
	}

	collisions := tunnel.Collisions([]string{s.VpnCidr}, local)
	if len(collisions) == 0 {
		return nil
	}
	return fmt.Errorf(
		"the tunnel subnet %s is a network this machine is already on:\n%s\n\n"+
			"  Deploying it would take that network away whenever the tunnel is up.\n"+
			"  Choose a --vpn-cidr that is not in use here or on any workstation this\n"+
			"  deployment will serve from — 10.100.0.0/24 and 10.110.0.0/24 are the\n"+
			"  defaults for a reason.",
		s.VpnCidr, tunnel.Explain(collisions))
}
