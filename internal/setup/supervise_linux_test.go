// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"strings"
	"testing"
)

// The unit, like the plist, names the whole profile store rather than one
// tunnel, and restarts the daemon when it dies.
func TestSupervisorUnitWatchesTheWholeProfileStore(t *testing.T) {
	unit := SupervisorDefinition(HelperPath, "/home/someone/.config/mymicrotunnel/profiles")

	for _, expected := range []string{
		`ExecStart="` + HelperPath + `" supervise --profiles "/home/someone/.config/mymicrotunnel/profiles"`,
		"Restart=always",
		"WantedBy=multi-user.target",
	} {
		if !strings.Contains(unit, expected) {
			t.Errorf("the unit has no %q:\n%s", expected, unit)
		}
	}
}
