// SPDX-License-Identifier: GPL-3.0-or-later
// Command wiregard-mini-vpn deploys the AWS side of the tunnel, configures the
// local side, and installs the menu bar app that toggles it.
//
//	wiregard-mini-vpn install --domain updates.example.com
//	wiregard-mini-vpn uninstall [--delete-stack] [--delete-keys]
//	wiregard-mini-vpn status
//	wiregard-mini-vpn peers [--stack NAME]
//	wiregard-mini-vpn version
//
// It talks to AWS through the SDK, so the only things it expects to find are
// WireGuard and, when building from a source checkout, the Swift compiler.
//
// The --json flag swaps the human output for one event per line, and --stage
// splits the run into the part that needs AWS credentials, the part that needs
// root, and the rest. The Swift setup window uses both, so the two front ends
// share this one implementation.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/awsops"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/setup"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/sys"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/tunnel"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/ui"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/version"
)

func main() {
	// Must happen before anything looks for a tool: a GUI-launched process
	// inherits launchd's bare PATH, not a shell's.
	sys.ExtendPath()

	if len(os.Args) < 2 {
		runInstall(os.Args[1:])
		return
	}

	switch os.Args[1] {
	case "install":
		runInstall(os.Args[2:])
	case "uninstall":
		runUninstall(os.Args[2:])
	case "status":
		runStatus(os.Args[2:])
	case "peers":
		runPeers(os.Args[2:])
	case "supervise":
		runSupervise(os.Args[2:])
	case "tunnel":
		runTunnel(os.Args[2:])
	case "diagnose":
		runDiagnose(os.Args[2:])
	case "version", "--version":
		fmt.Println(version.String())
	case "profiles":
		// Exists so the setup window does not have to parse ~/.aws itself.
		fmt.Println(strings.Join(awsops.Profiles(), "\n"))
	case "-h", "--help", "help":
		usage()
	default:
		runInstall(os.Args[1:])
	}
}

func usage() {
	fmt.Println(`wiregard-mini-vpn ` + version.String() + `

  install    deploy the stack and configure this machine (default)
  uninstall  remove the local install; optionally delete the stack
  status     report whether the tunnel is up
  peers      list or remove the workstations a deployment serves from
  tunnel     up, down or status for the local WireGuard interface
  diagnose   collect everything a support conversation would ask for
  supervise  run the reconcile loop; normally started by launchd
  version    print the build

Flags for install:
  --json                 emit one NDJSON event per line instead of prose
  --non-interactive      never prompt; every value must be supplied
  --stage all|deploy|root|finish
  --settings PATH        carry settings between stages
  --profile NAME         existing AWS profile
  --access-key-id ID     use static credentials instead of a profile
  --secret-access-key K
  --region NAME          defaults to the region the profile names
  --stack NAME
  --domain HOST          the public hostname this deployment serves (required)
  --port NUMBER          port the service listens on, on this machine
  --health-path PATH     path the load balancer polls (default /hc)
  --vpc ID               VPC to deploy into; discovered when omitted
  --hosted-zone ID       Route53 zone for the hostname; discovered when omitted
  --vpn-cidr CIDR        tunnel subnet (default 10.100.0.0/24)
  --client-ip ADDRESS    tunnel address of this machine
  --gateway-ip ADDRESS   tunnel address of the gateway
  --peer-label NAME      how this machine appears in the peer list
  --alarm-email ADDRESS  notify this address when the gateway is down
  --alarm-webhook URL    POST alarms to an https endpoint (Slack, PagerDuty)
  --alarm-on-tunnel-down also notify when no workstation is connected
  --supervise            keep the tunnel at its last state across reboots
  --client-public-key KEY  reuse a known key instead of reading or generating one
  --login-item           register the app to open at login`)
}

func runInstall(args []string) {
	flags := flag.NewFlagSet("install", flag.ExitOnError)

	asJSON := flags.Bool("json", false, "emit NDJSON events")
	nonInteractive := flags.Bool("non-interactive", false, "never prompt")
	stage := flags.String("stage", string(setup.StageAll), "all, deploy, root or finish")
	settingsPath := flags.String("settings", "", "path used to carry settings between stages")
	loginItem := flags.Bool("login-item", false, "register the app to open at login")

	defaults := setup.Defaults()
	profile := flags.String("profile", "", "existing AWS profile")
	accessKeyID := flags.String("access-key-id", "", "static credentials")
	secretAccessKey := flags.String("secret-access-key", "", "static credentials")
	region := flags.String("region", "", "AWS region")
	stackName := flags.String("stack", defaults.StackName, "CloudFormation stack name")
	domainName := flags.String("domain", "", "public hostname")
	servicePort := flags.String("port", defaults.ServicePort, "local service port")
	healthPath := flags.String("health-path", defaults.HealthCheckPath, "load balancer health check path")
	vpcID := flags.String("vpc", "", "VPC to deploy into")
	hostedZoneID := flags.String("hosted-zone", "", "Route53 hosted zone id")
	vpnCidr := flags.String("vpn-cidr", defaults.VpnCidr, "tunnel subnet")
	clientAddress := flags.String("client-ip", defaults.ClientAddress, "tunnel address of this machine")
	gatewayAddress := flags.String("gateway-ip", defaults.GatewayAddress, "tunnel address of the gateway")
	peerLabel := flags.String("peer-label", defaults.PeerLabel, "name for this machine in the peer list")
	alarmEmail := flags.String("alarm-email", "", "address notified when the gateway is down")
	alarmWebhook := flags.String("alarm-webhook", "", "https endpoint notified when the gateway is down")
	alarmOnTunnelDown := flags.Bool("alarm-on-tunnel-down", false, "also alarm when no workstation is connected")
	supervise := flags.Bool("supervise", false, "keep the tunnel at its last state across reboots")
	clientPublicKeyFlag := flags.String("client-public-key", "", "reuse a known WireGuard public key instead of reading or generating one")

	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)
	interactive := !*nonInteractive

	settings := defaults
	if *settingsPath != "" {
		if loaded, err := setup.ReadSettings(*settingsPath); err == nil {
			settings = loaded
		}
	}

	// A flag that was actually typed wins over whatever the settings file
	// carried; one left at its default does not, or resuming a staged install
	// would undo every answer the earlier stage recorded.
	typed := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { typed[f.Name] = true })
	applyString(typed, "region", region, &settings.Region)
	applyString(typed, "stack", stackName, &settings.StackName)
	applyString(typed, "domain", domainName, &settings.DomainName)
	applyString(typed, "port", servicePort, &settings.ServicePort)
	applyString(typed, "health-path", healthPath, &settings.HealthCheckPath)
	applyString(typed, "vpc", vpcID, &settings.VpcID)
	applyString(typed, "hosted-zone", hostedZoneID, &settings.HostedZoneID)
	applyString(typed, "vpn-cidr", vpnCidr, &settings.VpnCidr)
	applyString(typed, "client-ip", clientAddress, &settings.ClientAddress)
	applyString(typed, "gateway-ip", gatewayAddress, &settings.GatewayAddress)
	applyString(typed, "peer-label", peerLabel, &settings.PeerLabel)
	applyString(typed, "alarm-email", alarmEmail, &settings.AlarmEmail)
	applyString(typed, "alarm-webhook", alarmWebhook, &settings.AlarmWebhook)
	applyString(typed, "profile", profile, &settings.Profile)
	if typed["alarm-on-tunnel-down"] {
		settings.AlarmOnTunnelDown = *alarmOnTunnelDown
	}
	if typed["supervise"] {
		settings.Supervise = *supervise
	}
	settings.Username = setup.CurrentUsername()

	ctx := context.Background()

	// The privileged stage does nothing else: it writes the root-owned files
	// and exits, so the authorisation dialog covers as little as possible.
	if setup.Stage(*stage) == setup.StageRoot {
		if os.Geteuid() != 0 {
			ui.Fail("The root stage must run as root.")
		}
		// Prefer the name captured while still unprivileged.
		owner := settings.Username
		if owner == "" || owner == "root" {
			owner = setup.CurrentUsername()
		}
		if owner == "root" {
			ui.Fail("Refusing to write a sudoers rule owned by root: it would grant the actual user nothing.")
		}
		if err := setup.WriteRootFiles(settings, owner, true); err != nil {
			ui.Fail("%v", err)
		}
		ui.Done("Tunnel configuration and sudoers rule written")
		return
	}

	if interactive && !*asJSON {
		fmt.Println("wiregard_mini_vpn installer " + version.String())
		fmt.Println("\nThis deploys AWS resources into your account that cost roughly")
		fmt.Println("USD 26/month, and asks for your password to write root-owned files.")
	}

	setup.Prerequisites(interactive)

	runDeploy := setup.Stage(*stage) == setup.StageAll || setup.Stage(*stage) == setup.StageDeploy
	runFinish := setup.Stage(*stage) == setup.StageAll || setup.Stage(*stage) == setup.StageFinish

	if runDeploy {
		if settings.DomainName == "" && interactive && !*asJSON {
			settings.DomainName = ui.Ask("Public hostname this deployment should serve", "")
		}

		client := resolveClient(ctx, &settings, interactive, *accessKeyID, *secretAccessKey)

		// Checked once credentials are resolved, because the region may have
		// come from the profile rather than from a flag.
		if err := settings.Validate(); err != nil {
			ui.Fail("%v", err)
		}

		setup.Discover(ctx, client, &settings)

		if interactive && !*asJSON {
			fmt.Println("\n  About to deploy:")
			fmt.Printf("    stack     %s in %s\n", settings.StackName, settings.Region)
			fmt.Printf("    hostname  %s\n", settings.ServiceURL())
			fmt.Printf("    network   %s, subnets %s\n", settings.VpcID, strings.Join(settings.SubnetIDs, ", "))
			fmt.Printf("    target    %s:%s on this machine\n", settings.ClientAddress, settings.ServicePort)
			if !ui.Confirm("Proceed?", true) {
				ui.Fail("Cancelled.")
			}
		}

		clientPublicKey := *clientPublicKeyFlag
		if clientPublicKey == "" {
			clientPublicKey = setup.EnsureClientKey(interactive)
		} else {
			ui.Step("WireGuard client key")
			ui.Done("Using the key supplied on the command line")
		}

		setup.Deploy(ctx, client, &settings)
		setup.RegisterWorkstation(ctx, client, settings, clientPublicKey)

		if *settingsPath != "" {
			if err := settings.Write(*settingsPath); err != nil {
				ui.Fail("Could not save settings to %s: %v", *settingsPath, err)
			}
		}
		ui.Result(map[string]string{
			"endpoint":        settings.Endpoint,
			"serverPublicKey": settings.ServerPublicKey,
			"stackName":       settings.StackName,
			"region":          settings.Region,
			"profile":         settings.Profile,
			"serviceUrl":      settings.ServiceURL(),
		})

		// In the split flow the caller runs the root stage next.
		if setup.Stage(*stage) == setup.StageDeploy {
			return
		}

		ui.Step("Writing the tunnel configuration and sudoers rule")
		if err := setup.WriteRootFiles(settings, setup.CurrentUsername(), false); err != nil {
			ui.Fail("%v", err)
		}
		ui.Done("Written")
	}

	if !runFinish {
		return
	}

	ui.Step("Writing the app configuration")
	if err := setup.WriteAppConfig(settings); err != nil {
		ui.Fail("Could not write the app configuration: %v", err)
	}
	// An install ends with the tunnel up, so that is what the supervisor should
	// restore after a reboot until the user says otherwise.
	if err := setup.SetDesiredState(true); err != nil {
		ui.Warn("Could not record the desired tunnel state: %v", err)
	}
	ui.Done("%s", setup.AppConfigPath())

	setup.InstallApp(setup.RepoRoot())

	if *loginItem || (interactive && !*asJSON && ui.Confirm("Open the app automatically at login?", true)) {
		ui.Step("Login item")
		setup.RegisterLoginItem()
	}

	client := resolveClient(ctx, &settings, false, *accessKeyID, *secretAccessKey)
	setup.Verify(ctx, client, settings)

	sys.Run("/usr/bin/open", setup.InstalledAppPath)

	if !*asJSON {
		fmt.Println("\n✓ Installed.")
		fmt.Printf("  The padlock shield in the menu bar toggles %s.\n", settings.DomainName)
	}
	ui.Done("Installed")
}

// applyString exists because a flag package cannot tell "left at its default"
// from "typed with the default value", and a staged install has to be able to
// tell those apart: the second stage runs with fewer flags than the first.
func applyString(typed map[string]bool, name string, value *string, target *string) {
	if typed[name] {
		*target = *value
		return
	}
	if *target == "" {
		*target = *value
	}
}

// resolveClient turns whatever credentials are available into a client: an
// explicit key pair, a named profile, or a profile chosen interactively.
func resolveClient(ctx context.Context, settings *setup.Settings, interactive bool, accessKeyID, secretAccessKey string) *awsops.Client {
	ui.Step("AWS credentials")

	if accessKeyID != "" && secretAccessKey != "" {
		if settings.Profile == "" {
			settings.Profile = "xprem-vpn"
		}
		if settings.Region == "" && interactive {
			settings.Region = ui.Ask("Region", "us-east-1")
		}
		if err := awsops.WriteProfile(settings.Profile, accessKeyID, secretAccessKey, settings.Region); err != nil {
			ui.Fail("Could not save the credentials: %v", err)
		}
		ui.Done("Profile %s written to ~/.aws/credentials", settings.Profile)
	}

	if settings.Profile == "" {
		profiles := awsops.Profiles()
		if !interactive {
			ui.Fail("No AWS profile given. Pass --profile, or --access-key-id with --secret-access-key.")
		}
		if len(profiles) == 0 {
			settings.Profile = ui.Ask("Name for the new profile", "xprem-vpn")
			id := ui.Ask("AWS access key id", "")
			if id == "" {
				ui.Fail("An access key id is required.")
			}
			secret := ui.AskSecret("AWS secret access key (hidden)")
			if secret == "" {
				ui.Fail("A secret access key is required.")
			}
			settings.Region = ui.Ask("Region", settings.Region)
			if err := awsops.WriteProfile(settings.Profile, id, secret, settings.Region); err != nil {
				ui.Fail("Could not save the credentials: %v", err)
			}
			ui.Done("Profile %s written to ~/.aws/credentials", settings.Profile)
		} else {
			ui.Info("Existing profiles: %s", strings.Join(profiles, ", "))
			settings.Profile = ui.Ask("Profile", profiles[0])
		}
	}

	// A profile almost always names a region already. Asking for it again is
	// asking the user to repeat themselves, and getting a different answer is
	// how a deployment ends up split across two regions.
	if settings.Region == "" {
		settings.Region = awsops.ProfileRegion(ctx, settings.Profile)
		if settings.Region != "" {
			ui.Info("Using the region %s from profile %s", settings.Region, settings.Profile)
		} else if interactive {
			settings.Region = ui.Ask("Region", "us-east-1")
		}
	}

	client, err := awsops.LoadProfile(ctx, settings.Profile, settings.Region)
	if err != nil {
		ui.Fail("Could not load AWS credentials: %v", err)
	}

	identity, err := client.Identity(ctx)
	if err != nil {
		ui.Fail("%s", awsops.ExplainCredentialFailure(settings.Profile, err))
	}
	if awsops.IsSSOProfile(settings.Profile) {
		ui.Info("%s signs in through IAM Identity Center; its session will expire.", settings.Profile)
	}
	ui.Done("Authenticated as %s", identity)

	return client
}

func runUninstall(args []string) {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "emit NDJSON events")
	deleteStack := flags.Bool("delete-stack", false, "also delete the CloudFormation stack")
	deleteKeys := flags.Bool("delete-keys", false, "also delete /etc/wireguard")
	nonInteractive := flags.Bool("non-interactive", false, "never prompt")

	defaults := setup.Defaults()
	stackName := flags.String("stack", defaults.StackName, "CloudFormation stack name")
	profile := flags.String("profile", "default", "AWS profile")
	region := flags.String("region", "", "AWS region")

	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	ctx := context.Background()
	resolvedRegion := *region
	if resolvedRegion == "" {
		resolvedRegion = awsops.ProfileRegion(ctx, *profile)
	}

	options := setup.UninstallOptions{
		DeleteStack:    *deleteStack,
		DeleteKeys:     *deleteKeys,
		StackName:      *stackName,
		Profile:        *profile,
		Region:         resolvedRegion,
		NonInteractive: *nonInteractive,
	}

	if !*nonInteractive && !*asJSON {
		fmt.Println("wiregard_mini_vpn uninstaller")
		if !options.DeleteKeys {
			options.DeleteKeys = ui.Confirm("Also delete /etc/wireguard (tunnel config and private key)?", false)
		}
		if !options.DeleteStack {
			options.DeleteStack = ui.Confirm("Also delete the AWS CloudFormation stack?", false)
		}
		if options.DeleteStack {
			options.StackName = ui.Ask("Stack name", options.StackName)
			options.Profile = ui.Ask("AWS profile", options.Profile)
			options.Region = ui.Ask("Region", options.Region)
			if !ui.Confirm(fmt.Sprintf("Delete %s in %s? This takes the hostname down for every workstation.",
				options.StackName, options.Region), false) {
				options.DeleteStack = false
			}
		}
	}

	setup.Uninstall(ctx, options)

	if !*asJSON {
		fmt.Println("\n✓ Uninstalled.")
	}
}

// runPeers is the operational view of a deployment: which workstations may
// serve it, and the means to retire one from a machine that is not it.
func runPeers(args []string) {
	flags := flag.NewFlagSet("peers", flag.ExitOnError)
	defaults := setup.Defaults()
	stackName := flags.String("stack", defaults.StackName, "CloudFormation stack name")
	profile := flags.String("profile", "default", "AWS profile")
	region := flags.String("region", "", "AWS region")
	remove := flags.String("remove", "", "tunnel address of a workstation to retire")

	_ = flags.Parse(args)

	ctx := context.Background()
	resolvedRegion := *region
	if resolvedRegion == "" {
		resolvedRegion = awsops.ProfileRegion(ctx, *profile)
	}

	client, err := awsops.LoadProfile(ctx, *profile, resolvedRegion)
	if err != nil {
		ui.Fail("Could not load AWS credentials: %v", err)
	}
	settings := setup.Settings{StackName: *stackName}

	if *remove != "" {
		if err := client.RemovePeer(ctx, settings.PeersParameter(), *remove); err != nil {
			ui.Fail("Could not remove %s: %v", *remove, err)
		}
		outputs, err := client.StackOutputs(ctx, *stackName)
		if err == nil && outputs["TargetGroupArn"] != "" {
			// The port is whatever it was registered with; the API matches on
			// the pair, so a wrong port would leave the target in place.
			if parameter, found := client.StackParameter(ctx, *stackName, "ServicePort"); found {
				port := setup.Settings{ServicePort: parameter}.Port()
				if err := client.DeregisterTarget(ctx, outputs["TargetGroupArn"], *remove, port); err != nil {
					ui.Warn("Removed from the peer list, but not from the load balancer: %v", err)
				}
			}
		}
		fmt.Printf("Removed %s from %s.\n", *remove, *stackName)
		return
	}

	peers, err := client.Peers(ctx, settings.PeersParameter())
	if err != nil {
		ui.Fail("Could not read %s: %v", settings.PeersParameter(), err)
	}
	if len(peers) == 0 {
		fmt.Printf("No workstation is registered for %s.\n", *stackName)
		return
	}
	for _, peer := range peers {
		label := peer.Label
		if label == "" {
			label = "(unnamed)"
		}
		fmt.Printf("%-16s %-24s %s\n", peer.Address, label, peer.PublicKey)
	}
}

// runSupervise is the LaunchDaemon's entry point rather than something a person
// runs, but it stays a plain subcommand so it can be reproduced by hand when it
// misbehaves.
func runSupervise(args []string) {
	flags := flag.NewFlagSet("supervise", flag.ExitOnError)
	defaults := setup.Defaults()
	statePath := flags.String("state", setup.DesiredStatePath(), "file holding the desired tunnel state")
	interfaceName := flags.String("interface", defaults.InterfaceName, "WireGuard interface")
	configPath := flags.String("config", "", "tunnel configuration; defaults to the interface's own")
	once := flags.Bool("once", false, "reconcile once and exit")

	_ = flags.Parse(args)

	setup.Supervise(setup.SuperviseOptions{
		StatePath:     *statePath,
		InterfaceName: *interfaceName,
		ConfigPath:    *configPath,
		Once:          *once,
	})
}

// runTunnel is what the sudoers rule allows and what the menu bar calls. The
// argument list is fixed — `tunnel up wg0` — because that exact line is what
// /etc/sudoers.d/xprem-vpn grants, and anything this accepts beyond it would be
// something the rule did not mean to allow.
func runTunnel(args []string) {
	if len(args) < 1 {
		ui.Fail("Usage: wiregard-mini-vpn tunnel up|down|status [interface] [--config PATH]")
	}

	interfaceName := setup.Defaults().InterfaceName
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		interfaceName = rest[0]
		rest = rest[1:]
	}

	// --config is for trying a tunnel out without disturbing the installed one.
	// It does not widen the sudoers grant: that rule matches the whole command
	// line, and the two lines it allows do not carry this flag.
	flags := flag.NewFlagSet("tunnel", flag.ExitOnError)
	configPath := flags.String("config", "", "configuration to raise; defaults to the interface's own")
	_ = flags.Parse(rest)

	switch args[0] {
	case "up":
		if os.Geteuid() != 0 {
			ui.Fail("Raising the tunnel needs root.")
		}
		if err := setup.RaiseTunnel(interfaceName, *configPath); err != nil {
			ui.Fail("%v", err)
		}
		fmt.Printf("%s is up on %s\n", interfaceName, tunnel.Device(interfaceName))

	case "down":
		if os.Geteuid() != 0 {
			ui.Fail("Dropping the tunnel needs root.")
		}
		if err := tunnel.Down(interfaceName); err != nil {
			ui.Fail("%v", err)
		}
		fmt.Printf("%s is down\n", interfaceName)

	case "status":
		reportTunnel(interfaceName, setup.InstalledClientAddress())

	default:
		ui.Fail("Unknown tunnel command %q. Use up, down or status.", args[0])
	}
}

// reportTunnel answers the only question that matters about a running tunnel:
// not whether an interface exists, but whether anything is crossing it.
//
// There are three answers, not two. wireguard-go writes its name file mode 400
// and root-owned, so an unprivileged caller cannot tell a tunnel that is down
// from one it is not allowed to look at — and reporting the second as the first
// is how somebody ends up debugging a tunnel that was working all along.
func reportTunnel(interfaceName, address string) {
	device := tunnel.Device(interfaceName)
	if device == "" {
		if tunnel.AddressPresent(address) {
			fmt.Printf("%s is up (%s), but reading its handshake needs root:\n", interfaceName, address)
			fmt.Printf("  sudo %s tunnel status %s\n", setup.CommandPath, interfaceName)
			return
		}
		fmt.Printf("%s is down\n", interfaceName)
		return
	}

	status, err := tunnel.Report(interfaceName)
	if err != nil {
		ui.Fail("%v", err)
	}
	fmt.Printf("%s is up on %s\n", interfaceName, device)
	for _, peer := range status.Peers {
		when := "never"
		if !peer.LastHandshake.IsZero() {
			when = time.Since(peer.LastHandshake).Round(time.Second).String() + " ago"
		}
		fmt.Printf("  peer %s via %s, last handshake %s, rx %d tx %d\n",
			peer.PublicKey, peer.Endpoint, when, peer.ReceivedBytes, peer.SentBytes)
	}
	if !status.Connected(time.Now()) {
		fmt.Println("  no peer has handshaken recently: the interface exists but carries nothing")
	}
}

// runDiagnose is the first thing to ask somebody to run, and ideally the only
// thing: it gathers in one pass what would otherwise take three rounds of
// email. The output is meant to be pasted whole.
func runDiagnose(args []string) {
	flags := flag.NewFlagSet("diagnose", flag.ExitOnError)
	defaults := setup.Defaults()
	stackName := flags.String("stack", defaults.StackName, "CloudFormation stack name")
	profile := flags.String("profile", "default", "AWS profile")
	region := flags.String("region", "", "AWS region; taken from the profile when empty")
	skipAWS := flags.Bool("no-aws", false, "skip the parts that need AWS credentials")
	output := flags.String("o", "", "write to this file instead of the terminal")

	_ = flags.Parse(args)

	report := setup.Diagnose(context.Background(), setup.DiagnoseOptions{
		StackName: *stackName,
		Profile:   *profile,
		Region:    *region,
		SkipAWS:   *skipAWS,
	})

	if *output == "" {
		fmt.Print(report)
		return
	}
	if err := os.WriteFile(*output, []byte(report), 0o644); err != nil {
		ui.Fail("Could not write %s: %v", *output, err)
	}
	fmt.Printf("Written to %s\n", *output)
}

func runStatus(args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "emit NDJSON events")
	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	interfaceName := setup.Defaults().InterfaceName
	address := setup.InstalledClientAddress()

	// Asking the interface needs root, because /var/run/wireguard is 0700.
	// Without it, the address on an interface is the most that can be known.
	status, err := tunnel.Report(interfaceName)
	up := tunnel.IsUp(interfaceName) || tunnel.AddressPresent(address)
	connected := err == nil && status.Connected(time.Now())

	if *asJSON {
		ui.Result(map[string]string{
			"up":        fmt.Sprintf("%t", up),
			"connected": fmt.Sprintf("%t", connected),
			"interface": interfaceName,
			"address":   address,
			"device":    tunnel.Device(interfaceName),
			"version":   version.String(),
		})
		return
	}
	reportTunnel(interfaceName, address)
}
