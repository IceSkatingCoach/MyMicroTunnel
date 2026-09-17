// Command wiregard-mini-vpn deploys the AWS side of the tunnel, configures the
// local side, and installs the menu bar app that toggles it.
//
//	wiregard-mini-vpn install
//	wiregard-mini-vpn uninstall [--delete-stack] [--delete-keys]
//	wiregard-mini-vpn status
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

	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/awsops"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/setup"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/sys"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/ui"
)

func main() {
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
	fmt.Println(`wiregard-mini-vpn

  install    deploy the stack and configure this machine (default)
  uninstall  remove the local install; optionally delete the stack
  status     report whether the tunnel is up

Flags for install:
  --json                 emit one NDJSON event per line instead of prose
  --non-interactive      never prompt; every value must be supplied
  --stage all|deploy|root|finish
  --settings PATH        carry settings between stages
  --profile NAME         existing AWS profile
  --access-key-id ID     use static credentials instead of a profile
  --secret-access-key K
  --region NAME
  --stack NAME
  --domain HOST
  --dns-stack NAME
  --port NUMBER
  --client-ip ADDRESS
  --gateway-ip ADDRESS
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
	region := flags.String("region", defaults.Region, "AWS region")
	stackName := flags.String("stack", defaults.StackName, "CloudFormation stack name")
	domainName := flags.String("domain", defaults.DomainName, "public hostname")
	dnsStackName := flags.String("dns-stack", defaults.DNSStackName, "stack owning the hosted zone")
	servicePort := flags.String("port", defaults.ServicePort, "local service port")
	clientAddress := flags.String("client-ip", defaults.ClientAddress, "tunnel address of this machine")
	gatewayAddress := flags.String("gateway-ip", defaults.GatewayAddress, "tunnel address of the gateway")

	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)
	interactive := !*nonInteractive

	settings := defaults
	if *settingsPath != "" {
		if loaded, err := setup.ReadSettings(*settingsPath); err == nil {
			settings = loaded
		}
	}

	settings.Region = *region
	settings.StackName = *stackName
	settings.DomainName = *domainName
	settings.DNSStackName = *dnsStackName
	settings.ServicePort = *servicePort
	settings.ClientAddress = *clientAddress
	settings.GatewayAddress = *gatewayAddress
	if *profile != "" {
		settings.Profile = *profile
	}

	ctx := context.Background()

	// The privileged stage does nothing else: it writes the three root-owned
	// files and exits, so the authorisation dialog covers as little as possible.
	if setup.Stage(*stage) == setup.StageRoot {
		if os.Geteuid() != 0 {
			ui.Fail("The root stage must run as root.")
		}
		if err := setup.WriteRootFiles(settings, setup.CurrentUsername(), true); err != nil {
			ui.Fail("%v", err)
		}
		ui.Done("Tunnel configuration and sudoers rule written")
		return
	}

	if interactive && !*asJSON {
		fmt.Println("wiregard_mini_vpn installer")
		fmt.Println("\nThis deploys AWS resources that cost roughly USD 26/month, and")
		fmt.Println("asks for your password to write root-owned files.")
	}

	settings.WgQuickPath = setup.Prerequisites(interactive)

	runDeploy := setup.Stage(*stage) == setup.StageAll || setup.Stage(*stage) == setup.StageDeploy
	runFinish := setup.Stage(*stage) == setup.StageAll || setup.Stage(*stage) == setup.StageFinish

	if runDeploy {
		client := resolveClient(ctx, &settings, interactive, *accessKeyID, *secretAccessKey)

		if interactive && !*asJSON {
			fmt.Println("\n  About to deploy:")
			fmt.Printf("    stack     %s in %s\n", settings.StackName, settings.Region)
			fmt.Printf("    hostname  https://%s\n", settings.DomainName)
			fmt.Printf("    target    %s:%s on this machine\n", settings.ClientAddress, settings.ServicePort)
			if !ui.Confirm("Proceed?", true) {
				ui.Fail("Cancelled.")
			}
		}

		clientPublicKey := setup.EnsureClientKey(interactive)
		setup.Deploy(ctx, client, &settings, clientPublicKey)

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

// resolveClient turns whatever credentials are available into a client: an
// explicit key pair, a named profile, or a profile chosen interactively.
func resolveClient(ctx context.Context, settings *setup.Settings, interactive bool, accessKeyID, secretAccessKey string) *awsops.Client {
	ui.Step("AWS credentials")

	if accessKeyID != "" && secretAccessKey != "" {
		if settings.Profile == "" {
			settings.Profile = "xprem-vpn"
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

	client, err := awsops.LoadProfile(ctx, settings.Profile, settings.Region)
	if err != nil {
		ui.Fail("Could not load AWS credentials: %v", err)
	}

	identity, err := client.Identity(ctx)
	if err != nil {
		ui.Fail("Those credentials do not work: %v", err)
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
	region := flags.String("region", defaults.Region, "AWS region")

	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	options := setup.UninstallOptions{
		DeleteStack:    *deleteStack,
		DeleteKeys:     *deleteKeys,
		StackName:      *stackName,
		Profile:        *profile,
		Region:         *region,
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
			if !ui.Confirm(fmt.Sprintf("Delete %s in %s?", options.StackName, options.Region), false) {
				options.DeleteStack = false
			}
		}
	}

	setup.Uninstall(context.Background(), options)

	if !*asJSON {
		fmt.Println("\n✓ Uninstalled.")
	}
}

func runStatus(args []string) {
	flags := flag.NewFlagSet("status", flag.ExitOnError)
	asJSON := flags.Bool("json", false, "emit NDJSON events")
	_ = flags.Parse(args)
	ui.SetJSON(*asJSON)

	settings := setup.Defaults()
	connected := strings.Contains(sys.Run("/sbin/ifconfig").Output, settings.ClientAddress)

	if *asJSON {
		ui.Result(map[string]string{"connected": fmt.Sprintf("%t", connected)})
		return
	}
	if connected {
		fmt.Printf("connected (%s)\n", settings.ClientAddress)
		return
	}
	fmt.Println("disconnected")
}
