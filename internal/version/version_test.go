// SPDX-License-Identifier: GPL-3.0-or-later
package version

import (
	"runtime/debug"
	"testing"
)

// stamp sets what the linker and the build info would have said.
func stamp(t *testing.T, version, commit string, info *debug.BuildInfo, ok bool) {
	t.Helper()
	previousVersion, previousCommit, previousRead := Version, Commit, readBuildInfo
	Version, Commit = version, commit
	readBuildInfo = func() (*debug.BuildInfo, bool) { return info, ok }
	t.Cleanup(func() { Version, Commit, readBuildInfo = previousVersion, previousCommit, previousRead })
}

func buildInfo(settings ...debug.BuildSetting) *debug.BuildInfo {
	return &debug.BuildInfo{Settings: settings}
}

func TestStringShortensTheStampedCommit(t *testing.T) {
	stamp(t, "1.2.0", "abc1234def5678", buildInfo(debug.BuildSetting{Key: "vcs.revision", Value: "fff"}), true)

	// The stamp wins over the build info: it is what the release was cut from.
	if got := String(); got != "1.2.0 (abc1234)" {
		t.Errorf("String() = %q, want %q", got, "1.2.0 (abc1234)")
	}
}

func TestStringKeepsAShortCommitWhole(t *testing.T) {
	stamp(t, "1.2.0", "abc12", nil, false)
	if got := String(); got != "1.2.0 (abc12)" {
		t.Errorf("String() = %q", got)
	}
}

func TestStringFallsBackToTheBuildInfoRevision(t *testing.T) {
	// A `go install` build: no ldflags, but the toolchain recorded the commit.
	stamp(t, "dev", "", buildInfo(
		debug.BuildSetting{Key: "vcs", Value: "git"},
		debug.BuildSetting{Key: "vcs.revision", Value: "0123456789abcdef"},
	), true)

	if got := String(); got != "dev (0123456)" {
		t.Errorf("String() = %q, want %q", got, "dev (0123456)")
	}
}

func TestStringIsJustTheVersionWhenNoCommitIsKnown(t *testing.T) {
	cases := map[string]struct {
		info *debug.BuildInfo
		ok   bool
	}{
		"no build info":          {nil, false},
		"build info, no vcs":     {buildInfo(debug.BuildSetting{Key: "GOOS", Value: "darwin"}), true},
		"build info, no setting": {buildInfo(), true},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			stamp(t, "dev", "", c.info, c.ok)
			if got := String(); got != "dev" {
				t.Errorf("String() = %q, want %q", got, "dev")
			}
		})
	}
}
