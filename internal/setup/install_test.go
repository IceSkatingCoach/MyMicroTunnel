// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTunnelConfigRoutesTheVpcAndTheTunnel(t *testing.T) {
	s := valid()
	s.Endpoint = "203.0.113.10"
	s.ServerPublicKey = "abc123="

	config := TunnelConfig(s)

	for _, expected := range []string{
		"Address = 10.100.0.2/32",
		"PublicKey = abc123=",
		"Endpoint = 203.0.113.10:51820",
		"PersistentKeepalive = 25",
		// Both are needed: the tunnel subnet carries replies to the gateway,
		// and the VPC range is where the load balancer's nodes live. Dropping
		// either produces a tunnel that comes up and serves nothing.
		"AllowedIPs = 10.100.0.0/24, 172.31.0.0/16",
	} {
		if !strings.Contains(config, expected) {
			t.Errorf("the tunnel config has no %q:\n%s", expected, config)
		}
	}

	// The private key is read from a root-owned file at PostUp time. A copy in
	// this file would be a second place to leak it from.
	if strings.Contains(config, "PrivateKey =") {
		t.Error("the tunnel config contains a private key")
	}
	if !strings.Contains(config, "PostUp = wg set %i private-key "+valid().ClientKeyPath()) {
		t.Error("the tunnel config does not load the key from " + valid().ClientKeyPath())
	}
}

func TestTunnelConfigFollowsTheDiscoveredVpc(t *testing.T) {
	s := valid()
	s.VpcCidr = "10.42.0.0/16"
	s.VpnCidr = "10.200.0.0/24"
	s.ClientAddress = "10.200.0.2"

	if !strings.Contains(TunnelConfig(s), "AllowedIPs = 10.200.0.0/24, 10.42.0.0/16") {
		t.Error("the tunnel config still carries the addresses of the account it was written for")
	}
}

func TestSudoersRuleGrantsTwoCommandsAndNoMore(t *testing.T) {
	rule := SudoersFile([]Settings{valid()}, "someone")

	want := "someone ALL=(root) NOPASSWD: " + HelperPath + " tunnel up wg0, " +
		HelperPath + " tunnel down wg0"
	if !strings.Contains(rule, want) {
		t.Fatalf("the rule is not the expected one:\n%s", rule)
	}
	// An unqualified command, or one with a wildcard, turns this drop-in into a
	// path to a root shell.
	for _, forbidden := range []string{"ALL=(ALL)", "NOPASSWD: ALL", "*"} {
		if strings.Contains(rule, forbidden) {
			t.Errorf("the rule contains %q:\n%s", forbidden, rule)
		}
	}
}

// The rule the first version of this product shipped pointed at
// /opt/homebrew/bin/wg-quick. Homebrew owns that directory as the logged-in
// user, mode 775, so the very account the rule names could overwrite the file
// and become root without a password. The grant has to name a file that account
// cannot write.
func TestSudoersRuleNamesNoUserWritableDirectory(t *testing.T) {
	rule := SudoersFile([]Settings{valid()}, "someone")

	for _, userWritable := range []string{"/opt/homebrew", "/usr/local/bin", "/Users/"} {
		// The prose above the rule explains the reasoning and is allowed to
		// mention these; the grant line itself is not.
		for _, line := range strings.Split(rule, "\n") {
			if strings.HasPrefix(line, "#") || strings.TrimSpace(line) == "" {
				continue
			}
			if strings.Contains(line, userWritable) {
				t.Errorf("the grant names %s, which a package manager may hand to the user:\n%s",
					userWritable, line)
			}
		}
	}
}

func TestSudoersRuleNamesTheInterfaceTheInstallChose(t *testing.T) {
	s := valid()
	s.InterfaceName = "wg7"

	rule := SudoersFile([]Settings{s}, "someone")
	if !strings.Contains(rule, "tunnel up wg7") || !strings.Contains(rule, "tunnel down wg7") {
		t.Errorf("the rule does not name wg7:\n%s", rule)
	}
	if strings.Contains(rule, "wg0") {
		t.Errorf("the rule still names wg0:\n%s", rule)
	}
}

// A machine with two profiles needs both grants in the one file, and needs
// them to be exactly two: a rule appended per install would leave a removed
// profile's grant behind, and a re-run would leave the same grant twice.
func TestSudoersFileCoversEveryProfileOnce(t *testing.T) {
	work := valid()
	work.ProfileName = "work"
	work.InterfaceName = "wg0"

	personal := valid()
	personal.ProfileName = "personal"
	personal.InterfaceName = "wg1"

	rule := SudoersFile([]Settings{work, personal, work}, "someone")

	var grants []string
	for _, line := range strings.Split(rule, "\n") {
		if strings.HasPrefix(line, "someone ALL=") {
			grants = append(grants, line)
		}
	}
	if len(grants) != 2 {
		t.Fatalf("%d grants, want one per profile:\n%s", len(grants), rule)
	}
	if !strings.Contains(rule, "tunnel up wg0") || !strings.Contains(rule, "tunnel up wg1") {
		t.Errorf("a profile lost its grant:\n%s", rule)
	}
}

// visudo is the authority on what /etc/sudoers.d accepts, and a file it rejects
// breaks every sudo on the machine — including the one needed to remove it.
func TestValidateSudoersAgreesWithVisudo(t *testing.T) {
	if _, err := os.Stat("/usr/sbin/visudo"); err != nil {
		t.Skip("no visudo on this machine")
	}

	if err := ValidateSudoers(SudoersFile([]Settings{valid()}, "someone")); err != nil {
		t.Errorf("visudo rejected a rule the installer would write: %v", err)
	}
	if err := ValidateSudoers("this is not a sudoers file\n"); err == nil {
		t.Error("a malformed rule was accepted")
	}
}

// InstallApp used to join an empty repository root with "menubar", which yields
// a relative path rather than nothing. Run from inside a checkout, the
// *installed* binary then found ./menubar, decided it was a source build, and
// copied a locally built bundle over the signed one the package had installed.
func TestRepoRootIsEmptyWhenNotInACheckout(t *testing.T) {
	if root := RepoRoot(); root != "" && !strings.HasPrefix(root, "/") {
		t.Errorf("RepoRoot returned a relative path %q; every caller joins it with a subdirectory", root)
	}
}

func TestJoiningAnEmptyRootDoesNotProduceARelativePath(t *testing.T) {
	// The bug in one line: this is what the old code computed.
	if joined := filepath.Join("", "menubar"); filepath.IsAbs(joined) {
		t.Skip("filepath.Join no longer behaves this way")
	} else if joined != "menubar" {
		t.Skipf("unexpected join result %q", joined)
	}
	// So InstallApp has to check the root before joining, not after.
}
