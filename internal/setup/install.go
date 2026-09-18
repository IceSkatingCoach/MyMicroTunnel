// SPDX-License-Identifier: GPL-3.0-or-later
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
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/tunnel"
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

// Prerequisites checks that the one native thing this product needs is present.
//
// It used to install wireguard-tools through Homebrew, which meant a customer
// without Homebrew had to run an installer before running the installer, and a
// customer with it ended up trusting a root-capable binary in a directory their
// own account could write to. The package now carries wireguard-go itself, so
// there is nothing to fetch and nothing to trust that did not arrive signed.
func Prerequisites(interactive bool) string {
	ui.Step("Checking prerequisites")

	engine, err := tunnel.Engine("")
	if err != nil {
		ui.Fail("%v", err)
	}
	ui.Done("wireguard-go at %s", engine)
	return engine
}

// PublicKeyPath records the public half where the user can read it. Public keys
// are not secret, and keeping a copy outside /etc/wireguard is what lets a
// re-run recognise an existing tunnel without asking for root first. Without
// it, a non-interactive run would mint a new keypair and register a second peer
// whose private half this machine does not have.
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

	for _, sudo := range [][]string{{"-n", "cat", ClientKeyPath}, {"cat", ClientKeyPath}} {
		if !interactive && sudo[0] != "-n" {
			continue
		}
		existing := sys.Run("/usr/bin/sudo", sudo...)
		if !existing.OK() || existing.Output == "" {
			continue
		}
		public, err := tunnel.PublicKey(strings.TrimSpace(existing.Output))
		if err != nil {
			ui.Fail("%s does not hold a WireGuard key: %v", ClientKeyPath, err)
		}
		ui.Done("Reusing the existing key")
		return recordPublicKey(public)
	}

	private, err := tunnel.GenerateKey()
	if err != nil {
		ui.Fail("Could not generate a WireGuard key: %v", err)
	}

	// Staged in the user's own directory here; the root-owned copy is written
	// by the privileged stage, which may run in a separate process.
	if err := os.MkdirAll(stagingDir(), 0o700); err != nil {
		ui.Fail("Could not create %s: %v", stagingDir(), err)
	}
	if err := os.WriteFile(filepath.Join(stagingDir(), "client.key"), []byte(private+"\n"), 0o600); err != nil {
		ui.Fail("Could not stage the client key: %v", err)
	}
	ui.Done("Key generated")

	public, err := tunnel.PublicKey(private)
	if err != nil {
		ui.Fail("Could not derive the public key: %v", err)
	}
	return recordPublicKey(public)
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

// Discover fills in everything about the customer's account that the template
// needs and nobody should have to look up by hand.
//
// The first version of this stack carried one particular account's vpc-,
// subnet- and rtb- ids as parameter defaults. That is workable for a single
// deployment and impossible for a product: the account belongs to the customer,
// and an installer that opens by asking for a route table id is an installer
// nobody finishes.
func Discover(ctx context.Context, client *awsops.Client, s *Settings) {
	ui.Step("Looking at the account")

	network, err := client.DiscoverNetwork(ctx, s.VpcID)
	if err != nil {
		ui.Fail("%v", err)
	}
	s.VpcID = network.VpcID
	s.VpcCidr = network.VpcCidr
	s.SubnetIDs = network.PublicSubnets
	s.GatewaySubnetIDs = network.GatewaySubnets
	s.RouteTableIDs = network.RouteTableIDs
	ui.Done("VPC %s (%s), %d public subnet(s)", s.VpcID, s.VpcCidr, len(s.SubnetIDs))

	if s.HostedZoneID == "" {
		zone, err := client.FindHostedZone(ctx, s.DomainName)
		if err != nil {
			ui.Fail("%v", err)
		}
		s.HostedZoneID = zone
	}
	ui.Done("Hosted zone %s is authoritative for %s", s.HostedZoneID, s.DomainName)

	if err := s.ValidateNetwork(); err != nil {
		ui.Fail("%v", err)
	}
}

// Deploy is everything that needs AWS credentials and nothing that needs root.
func Deploy(ctx context.Context, client *awsops.Client, s *Settings) {
	ui.Step("Deploying %s (this takes a few minutes)", s.StackName)

	if err := client.DeployStack(ctx, s.StackName, infra.Template, StackParameters(*s), func(resource string) {
		ui.Info("%s", resource)
	}); err != nil {
		ui.Fail("The deployment failed: %v", err)
	}
	ui.Done("Stack deployed")

	outputs, err := client.StackOutputs(ctx, s.StackName)
	if err != nil {
		ui.Fail("Could not read the stack outputs: %v", err)
	}
	s.Endpoint = outputs["GatewayPublicIp"]
	s.TargetGroupARN = outputs["TargetGroupArn"]
	if s.Endpoint == "" || s.TargetGroupARN == "" {
		ui.Fail("The stack did not report a gateway address and a target group.")
	}

	ui.Step("Waiting for the gateway to publish its public key")
	serverKey, err := client.WaitForParameter(ctx, s.ServerKeyParameter(), 5*time.Minute)
	if err != nil {
		ui.Fail("%v. Check the instance's /var/log/cloud-init-output.log over SSM Session Manager.", err)
	}
	s.ServerPublicKey = serverKey
	ui.Done("Gateway key retrieved")
}

// StackParameters is what the template is told about this deployment. Kept
// separate from the deploy so a test can hold it against the template's own
// parameter list: a name that drifts on one side is silently dropped by
// CloudFormation on the other, and the symptom is a stack that deploys
// successfully and serves nothing.
func StackParameters(s Settings) map[string]string {
	return map[string]string{
		"ServiceDomainName": s.DomainName,
		"HostedZoneId":      s.HostedZoneID,
		"VpcId":             s.VpcID,
		"VpcCidr":           s.VpcCidr,
		"SubnetIds":         strings.Join(s.SubnetIDs, ","),
		"GatewaySubnetIds":  strings.Join(s.GatewaySubnetIDs, ","),
		"RouteTableIds":     strings.Join(s.RouteTableIDs, ","),
		"VpnCidr":           s.VpnCidr,
		"GatewayVpnAddress": s.GatewayAddress,
		"ServicePort":       s.ServicePort,
		"HealthCheckPath":   s.HealthCheckPath,
		"AlarmEmail":        s.AlarmEmail,
		"AlarmWebhook":      s.AlarmWebhook,
		"AlarmOnTunnelDown": fmt.Sprintf("%t", s.AlarmOnTunnelDown),
	}
}

// RegisterWorkstation puts this machine into the two lists that decide whether
// traffic reaches it: the gateway's peers, and the load balancer's targets.
//
// Neither is a CloudFormation property any more. A peer used to be a stack
// parameter, which meant adding a second workstation rewrote the gateway's boot
// script and replaced the instance — taking the first workstation offline to
// add the second one.
func RegisterWorkstation(ctx context.Context, client *awsops.Client, s Settings, clientPublicKey string) {
	ui.Step("Registering %s", s.PeerLabel)

	peers, err := client.UpsertPeer(ctx, s.PeersParameter(), awsops.Peer{
		PublicKey: clientPublicKey,
		Address:   s.ClientAddress,
		Label:     s.PeerLabel,
	})
	if err != nil {
		ui.Fail("Could not register this machine as a peer: %v", err)
	}
	ui.Done("%d workstation(s) in the peer list", len(peers))

	if err := client.RegisterTarget(ctx, s.TargetGroupARN, s.ClientAddress, s.Port()); err != nil {
		ui.Fail("Could not register %s behind the load balancer: %v", s.ClientAddress, err)
	}
	ui.Done("%s:%s is a load balancer target", s.ClientAddress, s.ServicePort)
}

// TunnelConfig is the contents of /etc/wireguard/<interface>.conf. Separate
// from writing it, because the privileged stage may run in another process and
// only needs the bytes.
func TunnelConfig(s Settings) string {
	return tunnel.Marshal(tunnel.File{
		Address:        s.ClientAddress,
		MTU:            TunnelMTU,
		PrivateKeyPath: ClientKeyPath,
		Config: tunnel.Config{
			Peers: []tunnel.Peer{{
				PublicKey: s.ServerPublicKey,
				Endpoint:  s.Endpoint + ":51820",
				// The tunnel subnet carries replies to the gateway; the VPC
				// range is where the load balancer's nodes live. Dropping
				// either produces a tunnel that comes up and serves nothing.
				AllowedIPs:          []string{s.VpnCidr, s.VpcCidr},
				PersistentKeepalive: 25,
			}},
		},
	})
}

// SudoersRule lets the menu bar move the tunnel without a password prompt on
// every toggle.
//
// It names the helper in /Library/PrivilegedHelperTools rather than the copy on
// the path. See HelperPath for why that distinction is the difference between a
// narrow grant and a root shell.
func SudoersRule(s Settings, username string) string {
	return strings.Join([]string{
		"# Installed by wiregard_mini_vpn. Lets the menu bar app raise and drop the",
		"# tunnel without a password prompt on every toggle.",
		"#",
		"# Scope: these two exact command lines only. This is not a general root",
		"# shell. Its safety rests on two things:",
		"#",
		"#   · " + HelperPath + " is root-owned,",
		"#     in a directory no package manager takes ownership of. A NOPASSWD rule",
		"#     pointing into a user-writable directory — /opt/homebrew/bin, say — is",
		"#     a password-free root shell for the user it names.",
		"#   · " + s.TunnelConfigPath() + " stays root-owned and mode 0600,",
		"#     because it names the key the helper loads and the peer it trusts.",
		fmt.Sprintf("%s ALL=(root) NOPASSWD: %s tunnel up %s, %s tunnel down %s",
			username, HelperPath, s.InterfaceName, HelperPath, s.InterfaceName),
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
// config, the sudoers rule, and — when asked for — the supervisor daemon.
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

	// An existing private key is never replaced. Overwriting one leaves the
	// gateway trusting a public key whose private half no longer exists: the
	// tunnel comes up, sends, and is silently dropped at the far end as an
	// unknown peer.
	if asRoot && sys.Exists(ClientKeyPath) {
		if _, err := os.Stat(staged); err == nil {
			os.Remove(staged)
			ui.Warn("Keeping the existing %s; the newly generated key was discarded.", ClientKeyPath)
		}
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

	if !s.Supervise {
		// Removed rather than left running: turning supervision off has to
		// actually turn it off, or the daemon goes on reconciling a deployment
		// the user has changed their mind about.
		RemoveSupervisor(asRoot)
		return nil
	}
	if !asRoot {
		// The interactive path has no way to write into /Library/LaunchDaemons
		// without another sudo, and doing it here keeps the number of password
		// prompts at the one the user has already answered.
		if err := sys.WriteAsRoot(
			SupervisorPlist(s, SupervisorExecutable, DesiredStatePath()),
			SupervisorPlistPath, "0644"); err != nil {
			return err
		}
		sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootout", "system/"+SupervisorLabel)
		if sys.RunInteractive("/usr/bin/sudo", "/bin/launchctl", "bootstrap", "system", SupervisorPlistPath) != 0 {
			return fmt.Errorf("launchctl refused the supervisor")
		}
		return nil
	}
	return InstallSupervisor(s, DesiredStatePath())
}

// InstallApp is a no-op when the package already placed the app; it only builds
// when running from a source checkout.
func InstallApp(repoRoot string) {
	// An empty root means this is the installed copy rather than one running
	// from a checkout. Joining "" with "menubar" produces a *relative* path,
	// which resolves against whatever directory the command happened to be run
	// from — so running /usr/local/bin/wiregard-mini-vpn while sitting in a
	// checkout made it try to rebuild the app and copy it over the signed
	// bundle the package had just installed.
	menubarDir := ""
	if repoRoot != "" {
		menubarDir = filepath.Join(repoRoot, "menubar")
	}
	if menubarDir == "" || !sys.Exists(menubarDir) {
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
	// the tunnel, which stays a deliberate act. When the supervisor is
	// installed, the tunnel's state at boot comes from the desired-state file
	// instead, which is the user's own last decision rather than a default.
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

	// Through sudo rather than in this process: the install may be running
	// unprivileged, and this is the same command the menu bar is allowed to run.
	up := sys.Run("/usr/bin/sudo", "-n", HelperPath, "tunnel", "up", s.InterfaceName)
	if !up.OK() {
		// A source checkout has no installed helper yet, so fall back to doing
		// it here, which works when the install itself was started with sudo.
		if err := RaiseTunnel(s.InterfaceName, s.TunnelConfigPath()); err != nil {
			ui.Fail("The tunnel would not come up: %v\n%s", err, up.Output)
		}
	}
	ui.Done("Tunnel is up")

	// The gateway learns about this machine from a parameter its timer reads
	// once a minute, so the first ping after a fresh registration can be up to
	// that late. Retried rather than failed, because "not yet" and "never" look
	// identical in a single attempt.
	var reachable bool
	for attempt := 0; attempt < 6; attempt++ {
		if sys.Run("/sbin/ping", "-c", "2", "-t", "5", s.GatewayAddress).OK() {
			reachable = true
			break
		}
		ui.Info("waiting for the gateway to pick up this peer")
		time.Sleep(20 * time.Second)
	}
	if !reachable {
		ui.Fail("The gateway at %s did not answer. Its peer list is %s.",
			s.GatewayAddress, s.PeersParameter())
	}
	ui.Done("Gateway %s answers", s.GatewayAddress)

	// Two consecutive checks 30s apart have to pass, so this takes about a minute.
	state := "unknown"
	var err error
	for attempt := 0; attempt < 10; attempt++ {
		if state, err = client.TargetHealthOf(ctx, s.TargetGroupARN, s.ClientAddress); err != nil {
			ui.Fail("%v", err)
		}
		if state == "healthy" {
			break
		}
		ui.Info("target %s", state)
		time.Sleep(20 * time.Second)
	}
	if state != "healthy" {
		ui.Fail("The load balancer still reports this machine as %q. Check that the service is listening on 0.0.0.0:%s.",
			state, s.ServicePort)
	}
	ui.Done("Load balancer target is healthy")

	probe := &http.Client{Timeout: 15 * time.Second}
	response, err := probe.Get(s.HealthCheckURL())
	if err != nil {
		ui.Fail("%s did not answer: %v", s.HealthCheckURL(), err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		ui.Fail("%s returned %d.", s.HealthCheckURL(), response.StatusCode)
	}
	ui.Done("%s returns 200", s.HealthCheckURL())
}

// CurrentUsername is who the sudoers rule should name: the human, never root.
//
// Getting this wrong is silent and total. The privileged stage runs as root
// two different ways — through sudo from a terminal, and through
// `osascript … with administrator privileges` from the setup window — and only
// the first sets SUDO_USER. The second once produced a rule reading
// "root ALL=(root) NOPASSWD: …", which is valid, installs cleanly, and grants
// the actual user nothing.
func CurrentUsername() string {
	if name := os.Getenv("SUDO_USER"); name != "" && name != "root" {
		return name
	}

	// Running as root with no SUDO_USER: ask who owns the login session.
	if os.Geteuid() == 0 {
		if console := sys.Run("/usr/bin/stat", "-f%Su", "/dev/console"); console.OK() {
			if name := strings.TrimSpace(console.Output); name != "" && name != "root" {
				return name
			}
		}
	}

	if current, err := user.Current(); err == nil && current.Username != "root" {
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
