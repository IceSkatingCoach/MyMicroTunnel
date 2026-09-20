// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"os"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

type UninstallOptions struct {
	// ProfileName is the VPN profile to remove. A machine with several of them
	// loses only this one: the app, the helper and the sudoers file stay, and
	// the file is rewritten from whatever is left.
	ProfileName string

	DeleteStack bool
	DeleteKeys  bool

	// KeepApp removes one profile and leaves the application in place.
	//
	// Deleting a profile and deleting this product are different requests,
	// and the menu offers both. Without this, removing the last profile took
	// the app, the login item and the stored AWS credentials with it —
	// which is right for an uninstall and startling for somebody tidying up
	// one deployment.
	KeepApp        bool
	StackName      string
	Profile        string
	Region         string
	NonInteractive bool
}

// Uninstall removes the re-creatable things without asking. The two that are
// not re-creatable — the private key and the AWS stack — are only touched on an
// explicit request.
func Uninstall(ctx context.Context, options UninstallOptions) {
	if options.ProfileName == "" {
		options.ProfileName = DefaultProfileName
	}
	config := installedTunnel(options.ProfileName)
	settings, _ := LoadProfileSettings(options.ProfileName)
	if options.StackName == "" {
		options.StackName = settings.StackName
	}

	// Everything this profile leaves behind for the others to live with.
	remaining := remainingProfiles(options.ProfileName)

	// The supervisor goes first. It exists to put the tunnel back up, and
	// removing anything else while it is still running means racing it.
	//
	// It is reinstalled at the end when another profile still wants it; taking
	// it down for the duration is the only way to be sure it does not raise
	// the tunnel being removed halfway through.
	RemoveSupervisor(false)

	// Down next: removing the sudoers rule takes away the means to do it.
	helper := config.HelperPath
	if helper == "" {
		helper = HelperPath
	}
	sys.RunInteractive("/usr/bin/sudo", helper, "tunnel", "down", config.InterfaceName)
	ui.Done("Tunnel %s down", config.InterfaceName)

	// Withdrawn from AWS before the local state that says how to reach AWS is
	// deleted. Leaving this machine in the peer list and in the target groups
	// would give the load balancer a target that never answers again, and every
	// request routed to it a timeout.
	withdrawFromDeployment(ctx, options, config)

	if len(remaining) == 0 && !options.KeepApp {
		sys.Run("/usr/bin/pkill", "-f", "MyMicroTunnel.app/Contents/MacOS/MyMicroTunnel")
		sys.Run("osascript", "-e",
			`tell application "System Events" to delete (every login item whose name is "MyMicroTunnel")`)
		os.RemoveAll(InstalledAppPath)
		ui.Done("App and login item removed")
	} else if len(remaining) > 0 {
		ui.Info("Keeping the app: %d other profile(s) still use it", len(remaining))
	}

	// Rewritten rather than deleted whenever something is left, so the other
	// profiles keep the grant they need and this one loses it.
	rewriteSudoers(remaining)

	os.RemoveAll(ProfileDir(options.ProfileName))
	ui.Done("Profile %s removed", options.ProfileName)
	if len(remaining) == 0 && !options.KeepApp {
		os.RemoveAll(AppConfigDir())
		ui.Done("App configuration removed")

		// The last profile takes the AWS credentials with it. A machine that
		// no longer runs this should not keep a working key for an account it
		// can no longer be asked about — and a key left in a Keychain is
		// exactly the kind nobody rotates.
		if settings.AccountID != "" {
			if stored, err := awsops.LoadAppCredentials(settings.AccountID); err == nil && stored != nil {
				if client, err := awsops.LoadProfile(ctx, options.Profile, options.Region); err == nil {
					if err := client.DeleteAppAccessKey(ctx, stored.AccessKeyID); err != nil {
						ui.Warn("The application's AWS key is still live: %v", err)
					}
				}
				if err := awsops.ForgetAppCredentials(settings.AccountID); err != nil {
					ui.Warn("Could not remove the stored credentials: %v", err)
				} else {
					ui.Done("Application AWS credentials removed from the Keychain")
				}
			}
		}
	}

	if options.DeleteKeys {
		// Only this profile's own files. /etc/wireguard belongs to the machine
		// and may hold another profile's key and config.
		sys.RunInteractive("/usr/bin/sudo", "rm", "-f",
			settingsKeyPath(settings, config), tunnelConfigPathOf(settings, config))
		ui.Done("Tunnel key and configuration removed")
	}

	if len(remaining) > 0 && AnySupervised(remaining) {
		if err := InstallSupervisor(ProfilesDir()); err != nil {
			ui.Warn("Could not restart the supervisor for the remaining profiles: %v", err)
		} else {
			ui.Done("Supervisor restarted for the remaining profiles")
		}
	}

	if options.DeleteStack {
		ui.Step("Deleting %s, which takes down the hostname it serves", options.StackName)
		client, err := awsops.LoadProfile(ctx, options.Profile, options.Region)
		if err != nil {
			ui.Fail("Could not load AWS credentials: %v", err)
		}

		// The alias record is not the stack's, so deleting the stack leaves it
		// behind pointing at a load balancer that no longer exists. Removed
		// first, and only when it still points where this deployment put it:
		// a record somebody has since repointed belongs to them now.
		if settings.HostedZoneID != "" && settings.LoadBalancerDNSName != "" {
			if err := client.DeleteAlias(ctx, settings.HostedZoneID, settings.DomainName,
				settings.LoadBalancerDNSName, settings.LoadBalancerZoneID); err != nil {
				ui.Warn("Could not remove the DNS record for %s: %v", settings.DomainName, err)
			} else {
				ui.Done("DNS record for %s removed", settings.DomainName)
			}
		}

		if err := client.DeleteStack(ctx, options.StackName); err != nil {
			ui.Fail("Could not delete the stack: %v", err)
		}
		ui.Done("Stack deleted")

		// The gateway creates these for itself — that is how a replaced
		// instance keeps its WireGuard identity — so CloudFormation does not
		// know about them and deleting the stack leaves them behind.
		stack := Settings{StackName: options.StackName}
		if err := client.DeleteParametersByPath(ctx, stack.ParameterPrefix()); err != nil {
			ui.Warn("Could not remove %s/*: %v", stack.ParameterPrefix(), err)
		} else {
			ui.Done("Parameters under %s removed", stack.ParameterPrefix())
		}
	}
}

// remainingProfiles is every profile except the one being removed. It is read
// before anything is deleted, because afterwards there is no way to tell a
// profile that was never there from one this run took away.
func remainingProfiles(removed string) []Settings {
	var kept []Settings
	for _, profile := range AllProfileSettings() {
		if profile.ProfileName == removed {
			continue
		}
		kept = append(kept, profile)
	}
	return kept
}

func rewriteSudoers(remaining []Settings) {
	if len(remaining) == 0 {
		sys.RunInteractive("/usr/bin/sudo", "rm", "-f", SudoersPath)
		ui.Done("Sudoers rule removed")
		return
	}

	rule := SudoersFile(remaining, CurrentUsername())
	if err := ValidateSudoers(rule); err != nil {
		ui.Warn("Leaving %s alone: the rewritten rule did not validate: %v", SudoersPath, err)
		return
	}
	if err := sys.WriteAsRoot(rule, SudoersPath, "0440"); err != nil {
		ui.Warn("Could not rewrite %s: %v", SudoersPath, err)
		return
	}
	ui.Done("Sudoers rule rewritten for the remaining profiles")
}

// settingsKeyPath and tunnelConfigPathOf prefer the recorded settings and fall
// back to the app config, because an install that failed before writing its
// settings still wrote the config the menu bar reads.
func settingsKeyPath(settings Settings, config appConfig) string {
	if settings.InterfaceName != "" {
		return settings.ClientKeyPath()
	}
	return Settings{InterfaceName: config.InterfaceName}.ClientKeyPath()
}

func tunnelConfigPathOf(settings Settings, config appConfig) string {
	if settings.InterfaceName != "" {
		return settings.TunnelConfigPath()
	}
	return Settings{InterfaceName: config.InterfaceName}.TunnelConfigPath()
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

	stack := Settings{StackName: options.StackName}
	if err := client.RemovePeer(ctx, stack.PeersParameter(), config.ClientAddress); err != nil {
		ui.Warn("Could not remove this machine from the peer list: %v", err)
	} else {
		ui.Done("Removed from the gateway's peer list")
	}

	outputs, err := client.StackOutputs(ctx, options.StackName)
	if err != nil {
		ui.Warn("Could not read the stack outputs: %v", err)
		return
	}
	if targetGroup := outputs["TargetGroupArn"]; targetGroup != "" {
		if err := client.DeregisterTarget(ctx, targetGroup, config.ClientAddress, config.Port()); err != nil {
			ui.Warn("Could not deregister %s from the load balancer: %v", config.ClientAddress, err)
		} else {
			ui.Done("Deregistered from the load balancer")
		}
	}

	// Every exposed port is its own target group, and a port left registered
	// is a listener that keeps routing to a workstation which has gone.
	for mapping, arn := range TcpTargetGroups(outputs) {
		// The key is local:published; the registration was made with the
		// local half, and the API matches on the pair, so deregistering with
		// the published one would quietly leave the target in place.
		parsed, err := ParsePortMapping(mapping)
		if err != nil {
			continue
		}
		if err := client.DeregisterTarget(ctx, arn, config.ClientAddress, parsed.Local); err != nil {
			ui.Warn("Could not deregister %s:%d: %v", config.ClientAddress, parsed.Local, err)
			continue
		}
		ui.Done("Deregistered from the load balancer on TCP %s", mapping)
	}
}
