// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"strings"
	"testing"
)

// One daemon holds every supervised profile, so the job names the store
// rather than a single tunnel. A plist naming one interface was why installing
// a second profile left the first one unsupervised.
func TestSupervisorPlistWatchesTheWholeProfileStore(t *testing.T) {
	plist := SupervisorPlist(HelperPath, "/tmp/profiles")

	for _, expected := range []string{
		"<string>" + SupervisorLabel + "</string>",
		"<string>" + HelperPath + "</string>",
		"<string>supervise</string>",
		"<string>--profiles</string>",
		"<string>/tmp/profiles</string>",
		"<key>RunAtLoad</key>",
		"<key>KeepAlive</key>",
	} {
		if !strings.Contains(plist, expected) {
			t.Errorf("the plist has no %q:\n%s", expected, plist)
		}
	}
}
