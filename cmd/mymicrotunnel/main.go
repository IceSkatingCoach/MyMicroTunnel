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
	case "reload":
		// Run by the package's postinstall, as root, after an update has
		// replaced the binaries underneath everything that is running.
		runReload(os.Args[2:])
	case "pubkey":
		// Diagnosis, not ceremony: when a tunnel sends and never hears back,
		// the question is whether the key on this machine is the one the
		// gateway was told about, and nothing else could answer it without
		// printing the private half somewhere it should not be.
		runPubkey(os.Args[2:])
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
  pubkey     print the public half of a private key, to compare identities
  reload     re-raise every running tunnel on the new engine; used after an update
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
  --port LOCAL[:PUBLIC]  where the service listens, and where it is published
                         (default 3000:443 — HTTPS on 443, service on 3000)
  --tcp-ports LIST       up to 10 more ports, each LOCAL[:PUBLIC]:
                         5432 publishes 5432; 3000:8080 publishes 3000 on 8080
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
	servicePort := flags.String("port", defaults.ServicePort, "local[:published] port for the TLS-terminated service")
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

	// Which flags were actually typed, as opposed to left at their default.
	// What a stage starts from is read against this: a profile nobody named is
	// a profile the carrier file itself decides.
	typed := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { typed[f.Name] = true })

	settings, resolveErr := settingsForRun(*vpnProfile, *settingsPath, setup.Stage(*stage), typed)
	if resolveErr != nil {
		ui.Fail("%v", resolveErr)
	}

	// A flag that was actually typed wins over whatever the settings file
	// carried; one left at its default does not, or resuming a staged install
	// would undo every answer the earlier stage recorded.
	applyString(typed, "vpn-profile", vpnProfile, &settings.ProfileName)
	applyString(typed, "interface", interfaceName, &settings.InterfaceName)
	applyString(typed, "region", region, &settings.Region)
	applyString(typed, "stack", stackName, &settings.StackName)
	applyString(typed, "domain", domainName, &settings.DomainName)
	if typed["port"] {
		// One flag, two fields: everything downstream reads them separately.
		mapping, err := setup.ParsePortMapping(*servicePort)
		if err != nil {
			ui.Fail("%v", err)
		}
		settings.ServicePort = strconv.Itoa(int(mapping.Local))
		settings.PublishedPort = strconv.Itoa(int(mapping.Published))
	} else if settings.ServicePort == "" {
		settings.ServicePort = *servicePort
	}
	if settings.PublishedPort == "" {
		settings.PublishedPort = "443"
	}
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
		mappings, err := setup.ParsePortMappings(*tcpPorts)
		if err != nil {
			ui.Fail("%v", err)
		}
		settings.TcpPorts = nil
		for _, mapping := range mappings {
			settings.TcpPorts = append(settings.TcpPorts, mapping.String())
		}
	}
	if typed["idle-timeout"] {
		settings.IdleTimeoutMinutes = *idleTimeout
	}
	settings.Username = setup.CurrentUsername()

	// An interface claimed by another profile is reassigned, but only for a
	// deployment that does not exist yet: a profile with an endpoint has a
	// tunnel, a config and a sudoers line under that name, and moving it
	// would orphan all three.
	if settings.Endpoint == "" {
		for _, other := range setup.AllProfileSettings() {
			if other.ProfileName != settings.ProfileName && other.InterfaceName == settings.InterfaceName {
				settings.InterfaceName = setup.NextFreeInterface(setup.TakenInterfaces(settings.ProfileName))
				break
			}
		}
	}

	// The tunnel addresses follow the tunnel subnet. Done before validation,
	// because otherwise choosing a subnet for a second profile fails on two
	// addresses the user never typed.
	settings.AlignAddressesToVpnCidr()

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
			fmt.Printf("    service   %s:%s here, published on %s\n",
				settings.ClientAddress, settings.ServicePort, settings.ServiceURL())
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

	if setup.HasApp && (*loginItem || (interactive && !*asJSON && ui.Confirm("Open the app automatically at login?", true))) {
		ui.Step("Login item")
		setup.RegisterLoginItem()
	}

	client := resolveClient(ctx, &settings, false, *accessKeyID, *secretAccessKey)
	setup.Verify(ctx, client, settings)

	setup.OpenApp()

	if !*asJSON {
		fmt.Println("\n✓ Installed.")
		if setup.HasApp {
			fmt.Printf("  The padlock shield in the menu bar toggles %s.\n", settings.DomainName)
		} else {
			fmt.Printf("  sudo %s tunnel up|down %s toggles %s.\n",
				setup.HelperPath, settings.InterfaceName, settings.DomainName)
		}
	}
	ui.Done("Installed")
}

// settingsForRun decides what an install stage starts from: the VPN profile
// as recorded on this machine, or the settings file the deployment stage
// leaves behind for the stages that follow it.
//
// Returns the reason when the two cannot be reconciled, rather than guessing:
// an applying stage handed the wrong file has no other source of truth, and
// guessing there is how a deployment for a profile nobody created appeared.
func settingsForRun(profileName, settingsPath string, stage setup.Stage, typed map[string]bool) (setup.Settings, error) {
	firstStage := stage == setup.StageAll || stage == setup.StageDeploy

	// A profile that is already installed is the starting point for a re-run,
	// so an install that only changes the ports does not have to repeat every
	// other answer.
	settings := setup.Defaults()
	settings.ProfileName = profileName
	storedProfile := false
	if stored, err := setup.LoadProfileSettings(profileName); err == nil {
		settings = stored
		storedProfile = true
	}

	if settingsPath == "" {
		return settings, nil
	}

	loaded, err := setup.ReadSettings(settingsPath)
	switch {
	case err != nil && !firstStage:
		// Refused rather than guessed. The privileged and finishing stages
		// exist to apply a deployment the first stage worked out; with the
		// file missing they used to fall back to the built-in defaults and
		// write a tunnel for a VPN profile called "default" on whatever
		// interface was free — a deployment nobody asked for, reported as an
		// error about a profile nobody created.
		return settings, fmt.Errorf("Cannot read %s: %v.\n\n"+
			"  This stage applies what the deployment stage recorded there. Running it\n"+
			"  without that file would invent a deployment; re-run the whole install.",
			settingsPath, err)
	case err != nil:
		// The first stage is allowed to start from nothing: that is what a
		// new profile is.
	case firstStage && storedProfile && typed["vpn-profile"]:
		// The file is the first stage's output, not its input. It is left
		// behind by the previous run, so reading it back deploys the ports
		// that run used rather than the ones just saved to the named profile
		// — a change set that runs and changes nothing that was asked for.
		// The profile store is the source of truth whenever the caller named
		// the profile; a typed flag still wins over both.
	case loaded.ProfileName == "" || loaded.ProfileName == settings.ProfileName:
		settings = loaded
	case !typed["vpn-profile"]:
		// Nobody named a profile, so the file names it. The privileged stage
		// is invoked with the path and nothing else — that is the point of
		// the path — and comparing its contents against the built-in default
		// would refuse every staged install.
		settings = loaded
	case !firstStage:
		// Refused, not ignored. An applying stage has no other source of
		// truth, so ignoring the file leaves it with the built-in defaults —
		// which is how a deployment for a VPN profile called "default"
		// appeared on a machine that had no such profile. An older front end
		// passing the path of a different profile is exactly the case: wrong
		// is not the same as absent, and both have to stop here.
		return settings, fmt.Errorf("%s describes the VPN profile %q, not %q.\n\n"+
			"  This stage applies what the deployment stage recorded. Applying another\n"+
			"  profile's deployment would build a tunnel nobody asked for; re-run the\n"+
			"  whole install for %q.",
			settingsPath, loaded.ProfileName, settings.ProfileName, settings.ProfileName)
	default:
		// The first stage may start from anything: it is about to work the
		// deployment out for itself.
	}
	return settings, nil
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

	// The app's own key, when this account has one. Keyed by account id,
	// which is why it can only be looked up once a deployment has recorded
	// which account it is in — a first run has no such record and falls
	// through to whatever the user supplies.
	//
	// Typed credentials still win: that is how a revoked or wrong key is
	// replaced without first working out where the old one is kept.
	if accessKeyID == "" && settings.AccountID != "" {
		if stored, err := awsops.LoadAppCredentials(settings.AccountID); err == nil && stored != nil {
			client, err := awsops.LoadStatic(ctx, stored.AccessKeyID, stored.SecretAccessKey, settings.Region)
			if err == nil {
				if identity, err := client.Identity(ctx); err == nil {
					ui.Done("Authenticated as %s, from the login Keychain", identity)
					return client
				}
			}
			ui.Warn("The stored application credentials did not work; falling back to %s.", settings.Profile)
		}
	}

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

	// From here on the app authenticates as itself.
	//
	// The credential just used belongs to a person. This mints the app's own
	// — narrower, unattended, and kept in the login Keychain — and the human
	// one is not read again. An account whose one-click stack predates the
	// app user simply carries on with what it has.
	if account, err := client.AccountID(ctx); err == nil {
		settings.AccountID = account
		if stored, err := awsops.LoadAppCredentials(account); err == nil && stored != nil {
			ui.Done("Using the application's own AWS credentials")
		} else if minted, err := awsops.EnsureAppCredentials(ctx, client, account); err != nil {
			ui.Warn("Could not create the application's own AWS credentials: %v", err)
			ui.Info("Carrying on with %s. Re-run setup to try again.", settings.Profile)
		} else if minted != nil {
			ui.Done("Created %s and stored its key in the login Keychain", awsops.AppUserName)
		}
	}

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
			settings.StackName = setup.DefaultStackName(account, settings.Region, settings.ProfileName)
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
	deleteKeys := flags.Bool("delete-keys", false, "also delete this profile's key and tunnel configuration")
	keepApp := flags.Bool("keep-app", false, "remove the profile but leave the app installed")
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
		KeepApp:        *keepApp,
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

// runReload puts every running tunnel back on the newly installed engine.
//
// An update replaces wireguard-go and this binary on disk and changes nothing
// that is already running: the kernel keeps the old image mapped for as long
// as the process lives. A tunnel raised before the update therefore goes on
// carrying packets through the old engine indefinitely, and the fix people
// reach for — quit the app — does not touch it either, because the engine is
// not the app's child.
//
// Bouncing the tunnel is the whole trick, and it is why this is allowed to be
// blunt: a second of downtime on a tunnel whose owner has just installed an
// update is a fair price, and the alternative is software that reports a
// version it is not running.
func runReload(args []string) {
	flags := flag.NewFlagSet("reload", flag.ExitOnError)
	_ = flags.Parse(args)

	if os.Geteuid() != 0 {
		ui.Fail("Reloading the tunnels needs root; the package's postinstall runs this.")
	}

	for _, profile := range setup.AllProfileSettings() {
		if profile.InterfaceName == "" || !tunnel.IsUp(profile.InterfaceName) {
			continue
		}
		fmt.Printf("re-raising %s (%s)\n", profile.InterfaceName, profile.ProfileName)
		if err := tunnel.Down(profile.InterfaceName); err != nil {
			fmt.Printf("  could not drop %s: %v\n", profile.InterfaceName, err)
			continue
		}
		if err := setup.RaiseTunnel(profile.InterfaceName, profile.TunnelConfigPath()); err != nil {
			// Reported, not fatal: the supervisor puts a supervised tunnel
			// back within its next pass, and a profile nobody supervises is
			// one the menu bar can raise.
			fmt.Printf("  could not raise %s: %v\n", profile.InterfaceName, err)
		}
	}
}

// runPubkey prints the public half of a private key file. Never the private
// half: the whole point is to compare identities in a place — a terminal, a
// support thread — where the secret must not appear.
func runPubkey(args []string) {
	path := setup.Settings{}.ClientKeyPath()
	if len(args) > 0 && args[0] != "" {
		path = args[0]
	}

	content, err := os.ReadFile(path)
	if err != nil {
		ui.Fail("Could not read %s: %v", path, err)
	}
	public, err := tunnel.PublicKey(strings.TrimSpace(string(content)))
	if err != nil {
		ui.Fail("%s does not hold a WireGuard key: %v", path, err)
	}
	fmt.Println(public)
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
	vpnProfile := flags.String("vpn-profile", setup.DefaultProfileName, "which deployment on this machine")
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
	fmt.Println(setup.DefaultStackName(account, resolvedRegion, *vpnProfile))
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

	case "save":
		runProfileSave(flags.Args()[1:])

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
		ui.Fail("Unknown profile command %q. Use list, show or save.", action)
	}
}

// runProfileSave records what the setup window is holding, without deploying
// anything.
//
// Filling a form and having no way to keep it is the complaint this answers:
// the only button deployed a CloudFormation stack, which is minutes and money
// and not what somebody adjusting a port wants at that moment. What is saved
// is the description of a deployment, not a deployment — the stack is
// unchanged until it is deployed, and the saved profile says so by having no
// endpoint yet.
func runProfileSave(args []string) {
	flags := flag.NewFlagSet("profile save", flag.ExitOnError)
	defaults := setup.Defaults()
	vpnProfile := flags.String("vpn-profile", defaults.ProfileName, "which deployment on this machine")
	awsProfile := flags.String("profile", "", "AWS profile")
	region := flags.String("region", "", "AWS region")
	stackName := flags.String("stack", "", "CloudFormation stack name")
	domainName := flags.String("domain", "", "public hostname")
	servicePort := flags.String("port", "", "local[:published] port for the service")
	tcpPorts := flags.String("tcp-ports", "", "further ports, each local[:published]")
	healthPath := flags.String("health-path", "", "load balancer health check path")
	vpnCidr := flags.String("vpn-cidr", "", "tunnel subnet")
	idleTimeout := flags.Int("idle-timeout", -1, "minutes of silence before the gateway sleeps")
	supervise := flags.Bool("supervise", false, "reconnect this profile at login")

	_ = flags.Parse(args)

	typed := map[string]bool{}
	flags.Visit(func(f *flag.Flag) { typed[f.Name] = true })

	// Started from what is already recorded, so saving one field does not
	// erase the rest.
	settings, err := setup.LoadProfileSettings(*vpnProfile)
	if err != nil {
		settings = defaults
		settings.ProfileName = *vpnProfile
		// Not the default interface: a saved draft that claims wg0 is one the
		// install later refuses for colliding with the profile that already
		// has it, against a value the user never typed.
		settings.InterfaceName = setup.NextFreeInterface(setup.TakenInterfaces(*vpnProfile))
	}

	applyString(typed, "profile", awsProfile, &settings.Profile)
	applyString(typed, "region", region, &settings.Region)
	applyString(typed, "stack", stackName, &settings.StackName)
	applyString(typed, "domain", domainName, &settings.DomainName)
	applyString(typed, "health-path", healthPath, &settings.HealthCheckPath)
	applyString(typed, "vpn-cidr", vpnCidr, &settings.VpnCidr)

	if typed["port"] {
		mapping, err := setup.ParsePortMapping(*servicePort)
		if err != nil {
			ui.Fail("%v", err)
		}
		settings.ServicePort = strconv.Itoa(int(mapping.Local))
		settings.PublishedPort = strconv.Itoa(int(mapping.Published))
	}
	if typed["tcp-ports"] {
		mappings, err := setup.ParsePortMappings(*tcpPorts)
		if err != nil {
			ui.Fail("%v", err)
		}
		settings.TcpPorts = nil
		for _, mapping := range mappings {
			settings.TcpPorts = append(settings.TcpPorts, mapping.String())
		}
	}
	if typed["idle-timeout"] && *idleTimeout >= 0 {
		settings.IdleTimeoutMinutes = *idleTimeout
	}
	if typed["supervise"] {
		settings.Supervise = *supervise
	}
	if settings.ProfileName == "" {
		settings.ProfileName = *vpnProfile
	}
	settings.AlignAddressesToVpnCidr()

	if err := setup.SaveProfileSettings(settings); err != nil {
		ui.Fail("Could not save the profile: %v", err)
	}
	fmt.Println(setup.ProfileSettingsPath(settings.ProfileName))
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

	// On Linux the interface is visible to everyone, but asking it anything
	// needs root; on macOS the device itself is hidden. Either way an
	// unprivileged caller can know the tunnel is up and no more.
	status, err := tunnel.Report(interfaceName)
	if device != "" && err != nil && os.Geteuid() != 0 {
		device = ""
	}
	if device == "" {
		if tunnel.AddressPresent(address) {
			fmt.Printf("%s is up (%s), but reading its handshake needs root:\n", interfaceName, address)
			fmt.Printf("  sudo %s tunnel status %s\n", setup.CommandPath, interfaceName)
			return
		}
		fmt.Printf("%s is down\n", interfaceName)
		return
	}
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
