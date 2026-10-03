// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/ui"
)

// Everything this package does to the machine itself goes through these, so
// the tests can stand in for root, launchd, systemd and the tunnel instead of
// needing them.
var (
	run                       = sys.Run
	runInteractive            = sys.RunInteractive
	writeAsRoot               = sys.WriteAsRoot
	writeAsRootNonInteractive = sys.WriteAsRootNonInteractive
	geteuid                   = os.Geteuid
	sleep                     = time.Sleep
	fail                      = ui.Fail

	tunnelEngine         = tunnel.Engine
	tunnelDevice         = tunnel.Device
	tunnelIsUp           = tunnel.IsUp
	tunnelUp             = tunnel.Up
	tunnelDown           = tunnel.Down
	tunnelReport         = tunnel.Report
	tunnelAddressPresent = tunnel.AddressPresent
	localNetworks        = tunnel.LocalNetworks

	loadAppCredentials   = awsops.LoadAppCredentials
	forgetAppCredentials = awsops.ForgetAppCredentials
)

// probeTransport carries the HTTP checks of the published service. Nil is the
// default transport.
var probeTransport http.RoundTripper

// rootDir is where the machine-wide paths are looked for, so a test can hold
// its own /etc and /Library. Empty is the real filesystem.
var rootDir = ""

func onDisk(path string) string {
	if rootDir == "" {
		return path
	}
	return filepath.Join(rootDir, path)
}
