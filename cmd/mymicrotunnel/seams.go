// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"os"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// Everything a command does that a test in this package must not do for real
// — end the process, read the terminal, touch the Keychain, launchd, root-owned
// files or a WireGuard interface, or run a CloudFormation deployment — is
// reached through one of these. Nothing outside the tests reassigns them.
var (
	fail      = ui.Fail
	ask       = ui.Ask
	askSecret = ui.AskSecret
	confirm   = ui.Confirm
	geteuid   = os.Geteuid

	loadAppCredentials   = awsops.LoadAppCredentials
	ensureAppCredentials = awsops.EnsureAppCredentials

	prerequisites       = setup.Prerequisites
	discover            = setup.Discover
	ensureClientKey     = setup.EnsureClientKey
	deploy              = setup.Deploy
	registerWorkstation = setup.RegisterWorkstation
	writeRootFiles      = setup.WriteRootFiles
	installApp          = setup.InstallApp
	registerLoginItem   = setup.RegisterLoginItem
	verify              = setup.Verify
	openApp             = setup.OpenApp
	uninstall           = setup.Uninstall
	superviseProfiles   = setup.Supervise
	diagnose            = setup.Diagnose
	raiseTunnel         = setup.RaiseTunnel

	tunnelDown           = tunnel.Down
	tunnelIsUp           = tunnel.IsUp
	tunnelDevice         = tunnel.Device
	tunnelReport         = tunnel.Report
	tunnelAddressPresent = tunnel.AddressPresent
)
