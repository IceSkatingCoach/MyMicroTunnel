// SPDX-License-Identifier: GPL-3.0-or-later
// Package version carries the build's identity, so a support conversation can
// start from a fact rather than from "the latest one, I think".
//
// Set at link time by the Makefile:
//
//	go build -ldflags "-X .../internal/version.Version=1.2.0 -X .../internal/version.Commit=abc1234"
package version

import "runtime/debug"

var (
	// Version is the released version. "dev" means a build from a checkout
	// that nobody stamped.
	Version = "dev"

	// Commit is the revision the binary was built from.
	Commit = ""
)

// String is what every front end shows: enough to reproduce the build, short
// enough to read out loud.
func String() string {
	commit := Commit
	if commit == "" {
		commit = revisionFromBuildInfo()
	}
	if commit == "" {
		return Version
	}
	if len(commit) > 7 {
		commit = commit[:7]
	}
	return Version + " (" + commit + ")"
}

// revisionFromBuildInfo recovers the commit for `go run` and `go install`
// builds, which never pass through the Makefile's ldflags.
func revisionFromBuildInfo() string {
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return ""
	}
	for _, setting := range info.Settings {
		if setting.Key == "vcs.revision" {
			return setting.Value
		}
	}
	return ""
}
