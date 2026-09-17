package setup

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"time"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/infra"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/awsops"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/sys"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/ui"
)

type Options struct {
	Stage          Stage
	NonInteractive bool
	Settings       Settings
	// SettingsPath carries the deployment between stages when the graphical
	// front end runs them as separate processes.
	SettingsPath string
}

// Prerequisites returns the discovered wg-quick path. The AWS CLI is not among
// them: this binary talks to AWS itself.
func Prerequisites(interactive bool) string {
	ui.Step("Checking prerequisites")

	wgQuick := sys.Which("wg-quick")
	if wgQuick == "" {
		if !interactive {
			ui.Fail("wg-quick is not installed. Install it with `brew install wireguard-tools`.")
		}
		ui.Info("WireGuard tools are missing; installing with Homebrew.")
		if sys.RunInteractive("brew", "install", "wireguard-tools") != 0 {
			ui.Fail("`brew install wireguard-tools` failed.")
		}
		if wgQuick = sys.Which("wg-quick"); wgQuick == "" {
			ui.Fail("wg-quick is still not on PATH after installing.")
		}
	}
	ui.Done("wg-quick at %s", wgQuick)
	return wgQuick
}

// PublicKeyPath records the public half where the user can read it. Public keys
// are not secret, and keeping a copy outside /etc/wireguard is what lets a
// re-run recognise an existing tunnel without asking for root first. Without
// it, a non-interactive run would mint a new keypair, change ClientPublicKey,
// replace the gateway, and then read a stale server key out of SSM.
func PublicKeyPath() string {
	return filepath.Join(AppConfigDir(), "client.pub")
}

// EnsureClientKey generates the private key on this machine and returns only
// the public half. The private key never leaves the machine and is never a
// CloudFormation parameter.
func EnsureClientKey(interactive bool) string {
	ui.Step("WireGuard client key")

	if recorded, err := os.ReadFile(PublicKeyPath()); err == nil && len(recorded) > 0 {
		ui.Done("Reusing the recorded public key")
		return strings.TrimSpace(string(recorded))
	}

	if existing := sys.Run("/usr/bin/sudo", "-n", "cat", ClientKeyPath); existing.OK() && existing.Output != "" {
		ui.Done("Reusing the existing key")
		return recordPublicKey(sys.RunWithInput(existing.Output+"\n", "wg", "pubkey").Output)
	}
	if interactive {
		if existing := sys.Run("/usr/bin/sudo", "cat", ClientKeyPath); existing.OK() && existing.Output != "" {
			ui.Done("Reusing the existing key")
			return recordPublicKey(sys.RunWithInput(existing.Output+"\n", "wg", "pubkey").Output)
		}
	}

	private := sys.Run("wg", "genkey")
	if !private.OK() || private.Output == "" {
		ui.Fail("`wg genkey` produced nothing.")
	}

	// Staged in the user's own directory here; the root-owned copy is written
	// by the privileged stage, which may run in a separate process.
	if err := os.MkdirAll(stagingDir(), 0o700); err != nil {
		ui.Fail("Could not create %s: %v", stagingDir(), err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir(), "client.key"), []byte(private.Output+"\n"), 0o600); err != nil {
		ui.Fail("Could not stage the client key: %v", err)
	}
	ui.Done("Key generated")

	return recordPublicKey(sys.RunWithInput(private.Output+"\n", "wg", "pubkey").Output)
}

func recordPublicKey(publicKey string) string {
	if publicKey == "" {
		ui.Fail("`wg pubkey` produced nothing.")
	}
	if err := os.MkdirAll(AppConfigDir(), 0o755); err == nil {
		_ = os.WriteFile(PublicKeyPath(), []byte(publicKey+"\n"), 0o644)
	}
	return publicKey
}

func stagingDir() string {
	return filepath.Join(os.TempDir(), "wiregard-mini-vpn-staging")
}

// Deploy is everything that needs AWS credentials and nothing that needs root.
func Deploy(ctx context.Context, client *awsops.Client, s *Settings, clientPublicKey string) {
	ui.Step("Deploying %s (this takes a few minutes)", s.StackName)

	parameters := map[string]string{
		"ClientPublicKey":   clientPublicKey,
		"ServiceDomainName": s.DomainName,
		"DnsStackName":      s.DNSStackName,
		"ServicePort":       s.ServicePort,
		"ClientVpnAddress":  s.ClientAddress,
		"GatewayVpnAddress": s.GatewayAddress,
	}

	if err := client.DeployStack(ctx, s.StackName, infra.Template, parameters, func(resource string) {
		ui.Info("%s", resource)
	}); err != nil {
		ui.Fail("The deployment failed: %v", err)
	}
	ui.Done("Stack deployed")

	endpoint, err := client.StackOutput(ctx, s.StackName, "GatewayPublicIp")
	if err != nil {
		ui.Fail("Could not read the gateway address: %v", err)
	}
	s.Endpoint = endpoint

	ui.Step("Waiting for the gateway to publish its public key")
	serverKey, err := client.WaitForParameter(ctx, ServerKeyParam, 5*time.Minute)
	if err != nil {
		ui.Fail("%v. Check the instance's /var/log/cloud-init-output.log over SSM Session Manager.", err)
	}
	s.ServerPublicKey = serverKey
	ui.Done("Gateway key retrieved")
}

// TunnelConfig is the wg-quick config. Separate from writing it, because the
// privileged stage may run in another process and only needs the bytes.
func TunnelConfig(s Settings) string {
	return strings.Join([]string{
		"[Interface]",
		"Address = " + s.ClientAddress + "/32",
		"MTU = 1380",
		"# Reads the key from the file the installer created, so the private key is",
		"# not duplicated into this config.",
		"PostUp = wg set %i private-key " + ClientKeyPath,
		"",
		"[Peer]",
		"PublicKey = " + s.ServerPublicKey,
		"AllowedIPs = 10.100.0.0/24, 172.31.0.0/16",
		"Endpoint = " + s.Endpoint + ":51820",
		"# Keeps the NAT mapping open, and re-pins the tunnel when this machine's",
		"# public address changes.",
		"PersistentKeepalive = 25",
		"",
	}, "\n")
}

func SudoersRule(s Settings, username string) string {
	return strings.Join([]string{
		"# Installed by wiregard_mini_vpn. Lets the menu bar app raise and drop the",
		"# tunnel without a password prompt on every toggle.",
		"#",
		"# Scope: these two exact command lines only. This is not a general root",
		"# shell. Its safety depends on " + s.TunnelConfigPath() + " staying",
		"# root-owned and mode 0600, because wg-quick runs that file's PostUp as root.",
		fmt.Sprintf("%s ALL=(root) NOPASSWD: %s up %s, %s down %s",
			username, s.WgQuickPath, s.InterfaceName, s.WgQuickPath, s.InterfaceName),
		"",
	}, "\n")
}

// ValidateSudoers refuses to install a rule visudo rejects. A malformed file in
// /etc/sudoers.d breaks every sudo on the machine, including the one needed to
// remove it.
func ValidateSudoers(rule string) error {
	scratch, err := os.MkdirTemp("", "wiregard-sudoers-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	staged := filepath.Join(scratch, "xprem-vpn")
	if err := os.WriteFile(staged, []byte(rule), 0o600); err != nil {
		return err
	}
	if result := sys.Run("/usr/sbin/visudo", "-c", "-f", staged); !result.OK() {
		return fmt.Errorf("visudo rejected the generated rule:\n%s", result.Output)
	}
	return nil
}

// WriteRootFiles is the only part that needs root: the private key, the tunnel
// config and the sudoers rule.
func WriteRootFiles(s Settings, username string, asRoot bool) error {
	rule := SudoersRule(s, username)
	if err := ValidateSudoers(rule); err != nil {
		return err
	}

	staged := filepath.Join(stagingDir(), "client.key")
	writes := []struct {
		content string
		path    string
		mode    string
	}{
		{TunnelConfig(s), s.TunnelConfigPath(), "0600"},
		{rule, SudoersPath, "0440"},
	}

	if content, err := os.ReadFile(staged); err == nil {
		writes = append(writes, struct {
			content string
			path    string
			mode    string
		}{string(content), ClientKeyPath, "0600"})
	}

	for _, write := range writes {
		if asRoot {
			mode := os.FileMode(0o600)
			if write.mode == "0440" {
				mode = 0o440
			}
			if err := sys.WriteAsRootNonInteractive(write.content, write.path, mode); err != nil {
				return err
			}
			continue
		}
		if err := sys.WriteAsRoot(write.content, write.path, write.mode); err != nil {
			return err
		}
	}

	os.Remove(staged)
	return nil
}

// InstallApp is a no-op when the package already placed the app; it only builds
// when running from a source checkout.
func InstallApp(repoRoot string) {
	menubarDir := filepath.Join(repoRoot, "menubar")
	if !sys.Exists(menubarDir) {
		ui.Step("Menu bar app")
		if sys.Exists(InstalledAppPath) {
			ui.Done("Already installed at %s", InstalledAppPath)
			return
		}
		// Running from inside a bundle that is not in /Applications yet: the
		// app can install itself rather than declaring the situation hopeless.
		if bundle := enclosingBundle(); bundle != "" {
			if result := sys.Run("cp", "-R", bundle, "/Applications/"); result.OK() {
				ui.Done("Copied %s to %s", bundle, InstalledAppPath)
				return
			}
		}
		ui.Fail("%s is missing and there are no sources to build it from.", InstalledAppPath)
	}

	ui.Step("Building the menu bar app")
	if sys.RunInteractive("make", "-C", menubarDir, "app") != 0 {
		ui.Fail("The app did not build.")
	}

	sys.Run("/usr/bin/pkill", "-f", "XpremVpn.app/Contents/MacOS/XpremVpn")
	os.RemoveAll(InstalledAppPath)
	if result := sys.Run("cp", "-R", filepath.Join(menubarDir, "build", "XpremVpn.app"), "/Applications/"); !result.OK() {
		ui.Fail("Could not copy the app into /Applications: %s", result.Output)
	}
	ui.Done("%s", InstalledAppPath)
}

func RegisterLoginItem() {
	// Opening at login only puts the switch in the menu bar; it does not raise
	// the tunnel, which stays a deliberate act.
	sys.Run("osascript", "-e",
		`tell application "System Events" to delete (every login item whose name is "XpremVpn")`)
	result := sys.Run("osascript", "-e",
		`tell application "System Events" to make login item at end with properties {path:"`+InstalledAppPath+`", hidden:true}`)
	if !result.OK() {
		ui.Warn("Could not register the login item: %s", result.Output)
		ui.Info("Add it under System Settings › General › Login Items.")
		return
	}
	ui.Done("Registered")
}

// Verify proves the path rather than assuming it. Every hop is checked in the
// order the traffic takes.
func Verify(ctx context.Context, client *awsops.Client, s Settings) {
	ui.Step("Verifying the whole path")

	up := sys.Run("/usr/bin/sudo", "-n", s.WgQuickPath, "up", s.InterfaceName)
	if !up.OK() && !strings.Contains(up.Output, "already exists") {
		ui.Fail("The tunnel would not come up:\n%s", up.Output)
	}
	ui.Done("Tunnel is up")

	if ping := sys.Run("/sbin/ping", "-c", "2", "-t", "5", s.GatewayAddress); !ping.OK() {
		ui.Fail("The gateway at %s did not answer.", s.GatewayAddress)
	}
	ui.Done("Gateway %s answers", s.GatewayAddress)

	targetGroup, err := client.TargetGroupARN(ctx, "xprem")
	if err != nil {
		ui.Fail("%v", err)
	}

	// Two consecutive checks 30s apart have to pass, so this takes about a minute.
	state := "unknown"
	for attempt := 0; attempt < 10; attempt++ {
		if state, err = client.TargetHealth(ctx, targetGroup); err != nil {
			ui.Fail("%v", err)
		}
		if state == "healthy" {
			break
		}
		ui.Info("target %s", state)
		time.Sleep(20 * time.Second)
	}
	if state != "healthy" {
		ui.Fail("The load balancer still reports the target as %q. Check that the service is listening on 0.0.0.0:%s.",
			state, s.ServicePort)
	}
	ui.Done("Load balancer target is healthy")

	probe := &http.Client{Timeout: 15 * time.Second}
	response, err := probe.Get("https://" + s.DomainName + "/hc")
	if err != nil {
		ui.Fail("https://%s/hc did not answer: %v", s.DomainName, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		ui.Fail("https://%s/hc returned %d.", s.DomainName, response.StatusCode)
	}
	ui.Done("https://%s/hc returns 200", s.DomainName)
}

func CurrentUsername() string {
	// SUDO_USER is set when this process was itself started through sudo, which
	// is how the privileged stage runs. The rule belongs to the human, not root.
	if name := os.Getenv("SUDO_USER"); name != "" {
		return name
	}
	if current, err := user.Current(); err == nil {
		return current.Username
	}
	return os.Getenv("USER")
}

// RepoRoot locates the checkout when running from source, and returns "" when
// running from the installed package.
func RepoRoot() string {
	// os.Executable rather than os.Args[0]: under sudo, and when invoked
	// through a symlink or from inside the app bundle, argv[0] is not a path
	// that can be walked back to the checkout.
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	resolved, err := filepath.EvalSymlinks(executable)
	if err == nil {
		executable = resolved
	}
	directory := filepath.Dir(executable)
	for attempt := 0; attempt < 4; attempt++ {
		if sys.Exists(filepath.Join(directory, "menubar")) && sys.Exists(filepath.Join(directory, "infra")) {
			return directory
		}
		directory = filepath.Dir(directory)
	}
	return ""
}

// enclosingBundle returns the .app this binary is running inside, or "" when it
// is a plain command-line build.
func enclosingBundle() string {
	executable, err := os.Executable()
	if err != nil {
		return ""
	}
	directory := filepath.Dir(executable)
	for attempt := 0; attempt < 4; attempt++ {
		if strings.HasSuffix(directory, ".app") {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			break
		}
		directory = parent
	}
	return ""
}
