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
	interfaceName, wgQuickPath := installedTunnel()

	// Down first: removing the sudoers rule takes away the means to do it.
	sys.RunInteractive("/usr/bin/sudo", wgQuickPath, "down", interfaceName)
	ui.Done("Tunnel down")

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
	}
}

// installedTunnel reads the app's own config, so an uninstall uses the same
// interface and wg-quick the install chose rather than guessing defaults.
func installedTunnel() (string, string) {
	defaults := Defaults()

	content, err := os.ReadFile(AppConfigPath())
	if err != nil {
		return defaults.InterfaceName, defaults.WgQuickPath
	}

	var config appConfig
	if err := json.Unmarshal(content, &config); err != nil {
		return defaults.InterfaceName, defaults.WgQuickPath
	}
	if config.InterfaceName == "" {
		config.InterfaceName = defaults.InterfaceName
	}
	if config.WgQuickPath == "" {
		config.WgQuickPath = defaults.WgQuickPath
	}
	return config.InterfaceName, config.WgQuickPath
}
