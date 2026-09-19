// SPDX-License-Identifier: GPL-3.0-or-later
// Command mymicrotunnel deploys the AWS side of the tunnel, configures the
// local side, and installs the menu bar app that toggles it.
//
//	mymicrotunnel install --domain updates.example.com
//	mymicrotunnel uninstall [--delete-stack] [--delete-keys]
//	mymicrotunnel status
//	mymicrotunnel peers [--stack NAME]
//	mymicrotunnel version
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
	"strconv"
	"strings"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/version"
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
	case "profile":
		runProfile(os.Args[2:])
	case "wake":
		runWake(os.Args[2:])
	case "supervise":
		runSupervise(os.Args[2:])
	case "tunnel":
		runTunnel(os.Args[2:])
	case "diagnose":
		runDiagnose(os.Args[2:])
	case "version", "--version":
		fmt.Println(version.String())
	case "regions":
		// Read by the setup window to fill its region picker.
		runRegions(os.Args[2:])
	case "default-stack":
		// Exists so the setup window can show the name a deploy would choose
		// instead of leaving the field blank or, worse, inheriting whatever
		// an older deployment was called.
		runDefaultStack(os.Args[2:])
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
	fmt.Println(`mymicrotunnel ` + version.String() + `

  install    deploy the stack and configure this machine (default)
  uninstall  remove the local install; optionally delete the stack
  status     report whether the tunnel is up
  peers      list or remove the workstations a deployment serves from
  profile    list this machine's VPN profiles, or show one
  wake       bring a gateway back after its idle timeout switched it off
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
  --vpn-profile NAME     which deployment on this machine (default "default")
  --stack NAME           defaults to mymicrotunnel-<account-id>-<region>
  --domain HOST          the public hostname this deployment serves (required)
  --port NUMBER          port the service listens on, on this machine
  --tcp-ports LIST       up to 10 more TCP ports to expose, e.g. 5432,6379
  --idle-timeout MINUTES switch the gateway off after this much silence (0 = never)
  --health-path PATH     path the load balancer polls (default /hc)
  --vpc ID               VPC to deploy into; discovered when omitted
  --hosted-zone ID       Route53 zone for the hostname; discovered when omitted
  --vpn-cidr CIDR        the tunnel subnet on this machine (default 10.100.0.0/24)
  --interface NAME       WireGuard interface; picked per profile when omitted
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
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	profile := flags.String("profile", "", "existing AWS profile")
	accessKeyID := flags.String("access-key-id", "", "static credentials")
	secretAccessKey := flags.String("secret-access-key", "", "static credentials")
	region := flags.String("region", "", "AWS region")
	stackName := flags.String("stack", "", "CloudFormation stack name")
	domainName := flags.String("domain", "", "public hostname")
	servicePort := flags.String("port", defaults.ServicePort, "local service port")
	tcpPorts := flags.String("tcp-ports", "", "up to 10 more TCP ports to expose through the load balancer")
	idleTimeout := flags.Int("idle-timeout", 0, "minutes of silence before the gateway is switched off")
	interfaceName := flags.String("interface", "", "WireGuard interface; picked for the profile when omitted")
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

	// A profile that is already installed is the starting point for a re-run,
	// so an install that only changes the ports does not have to repeat every
	// other answer.
	settings := defaults
	settings.ProfileName = *vpnProfile
	if stored, err := setup.LoadProfileSettings(*vpnProfile); err == nil {
		settings = stored
	}
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
	applyString(typed, "vpn-profile", vpnProfile, &settings.ProfileName)
	applyString(typed, "interface", interfaceName, &settings.InterfaceName)
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
	if typed["tcp-ports"] {
		settings.TcpPorts = setup.ParseTcpPorts(*tcpPorts)
	}
	if typed["idle-timeout"] {
		settings.IdleTimeoutMinutes = *idleTimeout
	}
	settings.Username = setup.CurrentUsername()

	// A second profile cannot share the first one's interface: both would
	// write /etc/wireguard/wg0.conf and the second install would take the
	// first deployment down without saying so.
	if settings.InterfaceName == "" {
		settings.InterfaceName = setup.NextFreeInterface(setup.TakenInterfaces(settings.ProfileName))
		if settings.InterfaceName == "" {
			ui.Fail("Every WireGuard interface name is taken; remove a profile first.")
		}
	}

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
		fmt.Println("MyMicroTunnel installer " + version.String())
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
		// come from the profile rather than from a flag, and the stack name
		// from the account.
		if err := settings.Validate(); err != nil {
			ui.Fail("%v", err)
		}
		// Checked before anything is deployed: two profiles sharing an
		// interface or a tunnel address both look installed and only one of
		// them works.
		if err := settings.ConflictsWithOtherProfiles(); err != nil {
			ui.Fail("%v", err)
		}
		// And against the networks this Mac is attached to right now. A tunnel
		// that claims the LAN it is sitting on comes up and takes the LAN away.
		if err := settings.ConflictsWithLocalNetworks(); err != nil {
			ui.Fail("%v", err)
		}

		setup.Discover(ctx, client, &settings)

		if interactive && !*asJSON {
			fmt.Println("\n  About to deploy:")
			fmt.Printf("    profile   %s on %s\n", settings.ProfileName, settings.InterfaceName)
			fmt.Printf("    stack     %s in %s\n", settings.StackName, settings.Region)
			fmt.Printf("    hostname  %s\n", settings.ServiceURL())
			fmt.Printf("    network   %s, subnets %s\n", settings.VpcID, strings.Join(settings.SubnetIDs, ", "))
			fmt.Printf("    tunnel    %s, this machine at %s\n", settings.VpnCidr, settings.ClientAddress)
			fmt.Printf("    target    %s:%s on this machine\n", settings.ClientAddress, settings.ServicePort)
			if len(settings.TcpPorts) > 0 {
				fmt.Printf("    also TCP  %s\n", strings.Join(settings.TcpPorts, ", "))
			}
			if settings.IdleTimeoutMinutes > 0 {
				fmt.Printf("    idle      off after %d minutes with no traffic\n", settings.IdleTimeoutMinutes)
			}
			if !ui.Confirm("Proceed?", true) {
				ui.Fail("Cancelled.")
			}
		}

		clientPublicKey := *clientPublicKeyFlag
		if clientPublicKey == "" {
			clientPublicKey = setup.EnsureClientKey(&settings, interactive)
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
		// Recorded under the profile as well, so `peers`, `wake`, `diagnose`
		// and `uninstall` can find this deployment by name instead of being
		// told the stack again on every command line.
		if err := setup.SaveProfileSettings(settings); err != nil {
			ui.Warn("Could not record the profile: %v", err)
		}
		ui.Result(map[string]string{
			"endpoint":        settings.Endpoint,
			"serverPublicKey": settings.ServerPublicKey,
			"stackName":       settings.StackName,
			"region":          settings.Region,
			"profile":         settings.Profile,
			"serviceUrl":      settings.ServiceURL(),
			"vpnProfile":      settings.ProfileName,
			"interface":       settings.InterfaceName,
			"wakeRoleArn":     settings.WakeRoleARN,
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
	if err := setup.SetDesiredState(settings.ProfileName, true); err != nil {
		ui.Warn("Could not record the desired tunnel state: %v", err)
	}
	if err := setup.SaveProfileSettings(settings); err != nil {
		ui.Warn("Could not record the profile: %v", err)
	}
	ui.Done("%s", setup.ProfileConfigPath(settings.ProfileName))

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

// firstNonEmpty is the precedence every command shares: what was typed, then
// what the profile recorded, then a last resort. Without it each command
// grew its own three-line version of the same fallback, and they disagreed.
func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
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
			settings.Profile = "mymicrotunnel"
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
			settings.Profile = ui.Ask("Name for the new profile", "mymicrotunnel")
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

	// Who the wake role will trust. An assumed-role ARN names a session that
	// will not exist tomorrow, so it is reduced to the role itself; anything
	// else — an IAM user, a role ARN already — is used as it stands.
	if settings.AppPrincipalArn == "" {
		settings.AppPrincipalArn = awsops.TrustablePrincipal(identity)
	}

	if account, err := client.AccountID(ctx); err == nil {
		settings.AccountID = account
		// Derived here rather than in Defaults(), which cannot know the
		// account: one fixed default makes the second deployment in an
		// account collide with the first.
		if settings.StackName == "" {
			settings.StackName = setup.DefaultStackName(account, settings.Region)
			ui.Info("Deploying as %s", settings.StackName)
		}
	} else if settings.StackName == "" {
		ui.Fail("Could not read the account id, so the default stack name cannot be built: %v.\n"+
			"  Pass --stack with a name of your own.", err)
	}

	return client
}

func runUninstall(args []string) {
	flags := flag.NewFlagSet("uninstall", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "emit NDJSON events")
	deleteStack := flags.Bool("delete-stack", false, "also delete the CloudFormation stack")
	deleteKeys := flags.Bool("delete-keys", false, "also delete /etc/wireguard")
	nonInteractive := flags.Bool("non-interactive", false, "never prompt")

	defaults := setup.Defaults()
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	stackName := flags.String("stack", "", "CloudFormation stack name")
	profile := flags.String("profile", "", "AWS profile")
	region := flags.String("region", "", "AWS region")

	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	ctx := context.Background()
	// What the install recorded, so an uninstall does not have to be told the
	// stack, the AWS profile and the region all over again.
	stored, _ := setup.LoadProfileSettings(*vpnProfile)
	resolvedStack := firstNonEmpty(*stackName, stored.StackName)
	resolvedProfile := firstNonEmpty(*profile, stored.Profile, "default")
	resolvedRegion := firstNonEmpty(*region, stored.Region)
	if resolvedRegion == "" {
		resolvedRegion = awsops.ProfileRegion(ctx, resolvedProfile)
	}

	options := setup.UninstallOptions{
		ProfileName:    *vpnProfile,
		DeleteStack:    *deleteStack,
		DeleteKeys:     *deleteKeys,
		StackName:      resolvedStack,
		Profile:        resolvedProfile,
		Region:         resolvedRegion,
		NonInteractive: *nonInteractive,
	}

	if !*nonInteractive && !*asJSON {
		fmt.Println("MyMicroTunnel uninstaller")
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

// runRegions prints the regions this account can deploy into, one per line,
// and nothing else. Silent on failure: the caller has a built-in list and a
// window that cannot open because AWS was unreachable is worse than one
// offering a region that turns out to need opting into.
func runRegions(args []string) {
	flags := flag.NewFlagSet("regions", flag.ExitOnError)
	profile := flags.String("profile", "default", "AWS profile")
	_ = flags.Parse(args)

	ctx := context.Background()
	region := awsops.ProfileRegion(ctx, *profile)
	if region == "" {
		region = "us-east-1"
	}
	client, err := awsops.LoadProfile(ctx, *profile, region)
	if err != nil {
		return
	}
	names, err := client.Regions(ctx)
	if err != nil {
		return
	}
	fmt.Println(strings.Join(names, "\n"))
}

// runDefaultStack prints the name a deploy would pick for a new deployment in
// this account and region, and nothing else. It is read by the setup window,
// so a stray line of prose here would become a stack name.
func runDefaultStack(args []string) {
	flags := flag.NewFlagSet("default-stack", flag.ExitOnError)
	profile := flags.String("profile", "default", "AWS profile")
	region := flags.String("region", "", "AWS region; taken from the profile when empty")
	_ = flags.Parse(args)

	ctx := context.Background()
	resolvedRegion := *region
	if resolvedRegion == "" {
		resolvedRegion = awsops.ProfileRegion(ctx, *profile)
	}
	if resolvedRegion == "" {
		return
	}

	client, err := awsops.LoadProfile(ctx, *profile, resolvedRegion)
	if err != nil {
		return
	}
	account, err := client.AccountID(ctx)
	if err != nil {
		return
	}
	fmt.Println(setup.DefaultStackName(account, resolvedRegion))
}

// runPeers is the operational view of a deployment: which workstations may
// serve it, and the means to retire one from a machine that is not it.
func runPeers(args []string) {
	flags := flag.NewFlagSet("peers", flag.ExitOnError)
	defaults := setup.Defaults()
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	stackName := flags.String("stack", "", "CloudFormation stack name")
	profile := flags.String("profile", "", "AWS profile")
	region := flags.String("region", "", "AWS region")
	remove := flags.String("remove", "", "tunnel address of a workstation to retire")

	_ = flags.Parse(args)

	ctx := context.Background()
	stored, _ := setup.LoadProfileSettings(*vpnProfile)
	resolvedStack := firstNonEmpty(*stackName, stored.StackName)
	resolvedProfile := firstNonEmpty(*profile, stored.Profile, "default")
	resolvedRegion := firstNonEmpty(*region, stored.Region)
	if resolvedRegion == "" {
		resolvedRegion = awsops.ProfileRegion(ctx, resolvedProfile)
	}
	if resolvedStack == "" {
		ui.Fail("No stack: profile %q is not installed here, so pass --stack.", *vpnProfile)
	}

	client, err := awsops.LoadProfile(ctx, resolvedProfile, resolvedRegion)
	if err != nil {
		ui.Fail("Could not load AWS credentials: %v", err)
	}
	settings := setup.Settings{StackName: resolvedStack}

	if *remove != "" {
		if err := client.RemovePeer(ctx, settings.PeersParameter(), *remove); err != nil {
			ui.Fail("Could not remove %s: %v", *remove, err)
		}
		outputs, err := client.StackOutputs(ctx, resolvedStack)
		if err == nil && outputs["TargetGroupArn"] != "" {
			// The port is whatever it was registered with; the API matches on
			// the pair, so a wrong port would leave the target in place.
			if parameter, found := client.StackParameter(ctx, resolvedStack, "ServicePort"); found {
				port := setup.Settings{ServicePort: parameter}.Port()
				if err := client.DeregisterTarget(ctx, outputs["TargetGroupArn"], *remove, port); err != nil {
					ui.Warn("Removed from the peer list, but not from the load balancer: %v", err)
				}
			}
			// And from every exposed port's own target group, each of which
			// would otherwise keep routing to a workstation that has gone.
			for port, arn := range setup.TcpTargetGroups(outputs) {
				number, convErr := strconv.Atoi(port)
				if convErr != nil {
					continue
				}
				if err := client.DeregisterTarget(ctx, arn, *remove, int32(number)); err != nil {
					ui.Warn("Still registered on TCP %s: %v", port, err)
				}
			}
		}
		fmt.Printf("Removed %s from %s.\n", *remove, resolvedStack)
		return
	}

	peers, err := client.Peers(ctx, settings.PeersParameter())
	if err != nil {
		ui.Fail("Could not read %s: %v", settings.PeersParameter(), err)
	}
	if len(peers) == 0 {
		fmt.Printf("No workstation is registered for %s.\n", resolvedStack)
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
	profilesDir := flags.String("profiles", "", "profile store to reconcile; every supervised profile in it")
	statePath := flags.String("state", "", "single profile: file holding the desired tunnel state")
	interfaceName := flags.String("interface", defaults.InterfaceName, "single profile: WireGuard interface")
	configPath := flags.String("config", "", "single profile: tunnel configuration")
	wakeUser := flags.String("wake-user", "", "user whose AWS profile is used to wake a switched-off gateway")
	once := flags.Bool("once", false, "reconcile once and exit")

	_ = flags.Parse(args)

	// Neither given: watch the whole store. That is what the installed daemon
	// asks for, and what somebody debugging by hand almost always means.
	if *profilesDir == "" && *statePath == "" {
		*profilesDir = setup.ProfilesDir()
	}

	setup.Supervise(setup.SuperviseOptions{
		ProfilesDir:   *profilesDir,
		StatePath:     *statePath,
		InterfaceName: *interfaceName,
		ConfigPath:    *configPath,
		WakeUser:      *wakeUser,
		Once:          *once,
	})
}

// runProfile is how somebody finds out what this machine is actually holding.
// A laptop behind three deployments has three interfaces, three tunnel
// addresses and three stacks, and none of that is guessable from the menu bar.
func runProfile(args []string) {
	flags := flag.NewFlagSet("profile", flag.ExitOnError)
	_ = flags.Parse(args)

	action := "list"
	if flags.NArg() > 0 {
		action = flags.Arg(0)
	}

	switch action {
	case "list":
		names := setup.ListProfiles()
		if len(names) == 0 {
			fmt.Println("No VPN profile is installed.")
			return
		}
		fmt.Printf("%-16s %-8s %-16s %-14s %s\n", "PROFILE", "IFACE", "ADDRESS", "STATE", "STACK")
		// AllProfileSettings rather than LoadProfileSettings per name: a
		// profile migrated from a single-deployment install has an app config
		// and no settings file yet, and listing it as blank rows is how it
		// looks like a broken install rather than an upgraded one.
		byName := map[string]setup.Settings{}
		for _, s := range setup.AllProfileSettings() {
			byName[s.ProfileName] = s
		}
		for _, name := range names {
			s := byName[name]
			state := "down"
			if tunnel.IsUp(s.InterfaceName) || tunnel.AddressPresent(s.ClientAddress) {
				state = "up"
			}
			fmt.Printf("%-16s %-8s %-16s %-14s %s\n",
				name, s.InterfaceName, s.ClientAddress, state, s.StackName)
		}

	case "show":
		if flags.NArg() < 2 {
			ui.Fail("Usage: mymicrotunnel profile show NAME")
		}
		name := flags.Arg(1)
		s, err := setup.LoadProfileSettings(name)
		if err != nil {
			ui.Fail("No profile called %q: %v", name, err)
		}
		fmt.Printf("profile        %s\n", s.ProfileName)
		fmt.Printf("stack          %s in %s\n", s.StackName, s.Region)
		fmt.Printf("hostname       %s\n", s.ServiceURL())
		fmt.Printf("interface      %s\n", s.InterfaceName)
		fmt.Printf("tunnel subnet  %s (this machine %s, gateway %s)\n", s.VpnCidr, s.ClientAddress, s.GatewayAddress)
		fmt.Printf("service port   %s\n", s.ServicePort)
		if len(s.TcpPorts) > 0 {
			fmt.Printf("exposed TCP    %s\n", strings.Join(s.TcpPorts, ", "))
		}
		if s.IdleTimeoutMinutes > 0 {
			fmt.Printf("idle timeout   %d minutes\n", s.IdleTimeoutMinutes)
		}
		fmt.Printf("supervised     %t\n", s.Supervise)

	default:
		ui.Fail("Unknown profile command %q. Use list or show.", action)
	}
}

// runWake brings a gateway back that switched itself off on its idle timeout.
//
// It is a subcommand rather than something buried in the menu bar because it
// is also the answer when the tunnel is down and nobody knows why: waking a
// gateway that is already running costs one API call and says so.
func runWake(args []string) {
	flags := flag.NewFlagSet("wake", flag.ExitOnError)
	defaults := setup.Defaults()
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	wait := flags.Duration("wait", 4*time.Minute, "how long to wait for the gateway to come back; 0 does not wait")
	quiet := flags.Bool("quiet", false, "say nothing unless something goes wrong")

	_ = flags.Parse(args)

	settings, err := setup.LoadProfileSettings(*vpnProfile)
	if err != nil {
		ui.Fail("No profile called %q: %v", *vpnProfile, err)
	}

	ctx := context.Background()
	client, err := awsops.LoadProfile(ctx, settings.Profile, settings.Region)
	if err != nil {
		ui.Fail("Could not load AWS credentials: %v", err)
	}

	report := func(message string) {
		if !*quiet {
			fmt.Println(message)
		}
	}
	if err := client.WakeGateway(ctx, awsops.WakeOptions{
		RoleARN:    settings.WakeRoleARN,
		GroupName:  settings.GatewayGroupName,
		Wait:       *wait,
		OnProgress: report,
	}); err != nil {
		ui.Fail("Could not wake the gateway for %s: %v", *vpnProfile, err)
	}
}

// runTunnel is what the sudoers rule allows and what the menu bar calls. The
// argument list is fixed — `tunnel up wg0` — because that exact line is what
// /etc/sudoers.d/mymicrotunnel grants, and anything this accepts beyond it would be
// something the rule did not mean to allow.
func runTunnel(args []string) {
	if len(args) < 1 {
		ui.Fail("Usage: mymicrotunnel tunnel up|down|status [interface] [--config PATH]")
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
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	stackName := flags.String("stack", "", "CloudFormation stack name")
	profile := flags.String("profile", "", "AWS profile")
	region := flags.String("region", "", "AWS region; taken from the profile when empty")
	skipAWS := flags.Bool("no-aws", false, "skip the parts that need AWS credentials")
	output := flags.String("o", "", "write to this file instead of the terminal")

	_ = flags.Parse(args)

	stored, _ := setup.LoadProfileSettings(*vpnProfile)
	report := setup.Diagnose(context.Background(), setup.DiagnoseOptions{
		ProfileName: *vpnProfile,
		StackName:   firstNonEmpty(*stackName, stored.StackName),
		Profile:     firstNonEmpty(*profile, stored.Profile, "default"),
		Region:      firstNonEmpty(*region, stored.Region),
		SkipAWS:     *skipAWS,
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
	vpnProfile := flags.String("vpn-profile", setup.DefaultProfileName, "which deployment on this machine")
	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	// The installed profile rather than the defaults: a second deployment is
	// on another interface at another address, and reporting wg0 for it would
	// say "down" about a tunnel that is up.
	interfaceName, address := setup.InstalledTunnelOf(*vpnProfile)

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
			"profile":   *vpnProfile,
			"device":    tunnel.Device(interfaceName),
			"version":   version.String(),
		})
		return
	}
	reportTunnel(interfaceName, address)
}
