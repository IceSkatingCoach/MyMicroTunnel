// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"encoding/json"
	"os"

	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/awsops"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/sys"
	"github.com/IceSkatingCoach/wiregard_mini_vpn/internal/ui"
)

type UninstallOptions struct {
	DeleteStack    bool
	DeleteKeys     bool
	StackName      string
	Profile        string
	Region         string
	NonInteractive bool
}

// Uninstall removes the re-creatable things without asking. The two that are
// not re-creatable — the private key and the AWS stack — are only touched on an
// explicit request.
func Uninstall(ctx context.Context, options UninstallOptions) {
	config := installedTunnel()

	// The supervisor goes first. It exists to put the tunnel back up, and
	// removing anything else while it is still running means racing it.
	RemoveSupervisor(false)

	// Down next: removing the sudoers rule takes away the means to do it.
	helper := config.HelperPath
	if helper == "" {
		helper = HelperPath
	}
	sys.RunInteractive("/usr/bin/sudo", helper, "tunnel", "down", config.InterfaceName)
	ui.Done("Tunnel down")

	// Withdrawn from AWS before the local state that says how to reach AWS is
	// deleted. Leaving this machine in the peer list and in the target group
	// would give the load balancer a target that never answers again, and every
	// request routed to it a timeout.
	withdrawFromDeployment(ctx, options, config)

	sys.Run("/usr/bin/pkill", "-f", "XpremVpn.app/Contents/MacOS/XpremVpn")
	sys.Run("osascript", "-e",
		`tell application "System Events" to delete (every login item whose name is "XpremVpn")`)
	os.RemoveAll(InstalledAppPath)
	ui.Done("App and login item removed")

	sys.RunInteractive("/usr/bin/sudo", "rm", "-f", SudoersPath)
	ui.Done("Sudoers rule removed")

	os.RemoveAll(AppConfigDir())
	ui.Done("App configuration removed")

	if options.DeleteKeys {
		sys.RunInteractive("/usr/bin/sudo", "rm", "-rf", "/etc/wireguard")
		ui.Done("/etc/wireguard removed")
	}

	if options.DeleteStack {
		ui.Step("Deleting %s, which takes down the hostname it serves", options.StackName)
		client, err := awsops.LoadProfile(ctx, options.Profile, options.Region)
		if err != nil {
			ui.Fail("Could not load AWS credentials: %v", err)
		}
		if err := client.DeleteStack(ctx, options.StackName); err != nil {
			ui.Fail("Could not delete the stack: %v", err)
		}
		ui.Done("Stack deleted")

		// The gateway creates these for itself — that is how a replaced
		// instance keeps its WireGuard identity — so CloudFormation does not
		// know about them and deleting the stack leaves them behind.
		settings := Settings{StackName: options.StackName}
		if err := client.DeleteParametersByPath(ctx, settings.ParameterPrefix()); err != nil {
			ui.Warn("Could not remove %s/*: %v", settings.ParameterPrefix(), err)
		} else {
			ui.Done("Parameters under %s removed", settings.ParameterPrefix())
		}
	}
}

// withdrawFromDeployment takes this machine out of a deployment that goes on
// serving from other workstations. Best effort on purpose: an uninstall must
// not stall because a credential expired months ago.
func withdrawFromDeployment(ctx context.Context, options UninstallOptions, config appConfig) {
	if config.ClientAddress == "" || options.StackName == "" {
		return
	}
	// Deleting the stack removes both lists anyway.
	if options.DeleteStack {
		return
	}

	client, err := awsops.LoadProfile(ctx, options.Profile, options.Region)
	if err != nil {
		ui.Warn("Not removing this machine from the AWS deployment: %v", err)
		return
	}

	settings := Settings{StackName: options.StackName}
	if err := client.RemovePeer(ctx, settings.PeersParameter(), config.ClientAddress); err != nil {
		ui.Warn("Could not remove this machine from the peer list: %v", err)
	} else {
		ui.Done("Removed from the gateway's peer list")
	}

	outputs, err := client.StackOutputs(ctx, options.StackName)
	if err != nil {
		ui.Warn("Could not read the stack outputs: %v", err)
		return
	}
	targetGroup := outputs["TargetGroupArn"]
	if targetGroup == "" {
		return
	}
	if err := client.DeregisterTarget(ctx, targetGroup, config.ClientAddress, config.Port()); err != nil {
		ui.Warn("Could not deregister %s from the load balancer: %v", config.ClientAddress, err)
		return
	}
	ui.Done("Deregistered from the load balancer")
}

// installedTunnel reads the app's own config, so an uninstall uses the same
// interface and wg-quick the install chose rather than guessing defaults.
func installedTunnel() appConfig {
	defaults := Defaults()
	config := appConfig{
		InterfaceName: defaults.InterfaceName,
		ClientAddress: defaults.ClientAddress,
		HelperPath:    HelperPath,
	}

	content, err := os.ReadFile(AppConfigPath())
	if err != nil {
		return config
	}

	var stored appConfig
	if err := json.Unmarshal(content, &stored); err != nil {
		return config
	}
	if stored.InterfaceName != "" {
		config.InterfaceName = stored.InterfaceName
	}
	if stored.HelperPath != "" {
		config.HelperPath = stored.HelperPath
	}
	if stored.ClientAddress != "" {
		config.ClientAddress = stored.ClientAddress
	}
	config.ServicePort = stored.ServicePort
	return config
}
