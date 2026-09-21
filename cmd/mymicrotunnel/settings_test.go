// SPDX-License-Identifier: GPL-3.0-or-later
package main

import (
	"path/filepath"
	"testing"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/setup"
)

// What a stage starts from, tested directly: the deploy stage reads the
// profile store, and the applying stages read the file the deploy stage left
// them. The end-to-end tests cover the refusals; these cover the precedence,
// which needs no AWS account.

// The bug: saving a new port in the setup window ran a deployment that
// deployed the old one. The window hands the deploy stage the carrier file
// from the previous run, and that file still held the ports that run used —
// so CloudFormation got a change set for settings nobody had just chosen.
func TestTheDeployStagePrefersTheProfileOverTheCarrierFile(t *testing.T) {
	store := t.TempDir()
	t.Setenv("HOME", store)

	stale := setup.Defaults()
	stale.ProfileName = "lab"
	stale.ServicePort = "3000"
	stale.TcpPorts = []string{"5432"}
	carrier := filepath.Join(store, "stale.json")
	if err := stale.Write(carrier); err != nil {
		t.Fatalf("writing the carrier file: %v", err)
	}

	current := stale
	current.ServicePort = "3010"
	current.TcpPorts = []string{"6379"}
	if err := setup.SaveProfileSettings(current); err != nil {
		t.Fatalf("saving the profile: %v", err)
	}

	typed := map[string]bool{"vpn-profile": true}
	settings, err := settingsForRun("lab", carrier, setup.StageDeploy, typed)
	if err != nil {
		t.Fatalf("resolving the settings: %v", err)
	}
	if settings.ServicePort != "3010" {
		t.Errorf("the deploy stage would deploy port %s, not the saved 3010", settings.ServicePort)
	}
	if len(settings.TcpPorts) != 1 || settings.TcpPorts[0] != "6379" {
		t.Errorf("the deploy stage would deploy TCP ports %v, not the saved [6379]", settings.TcpPorts)
	}
}

// The carrier file is still what the applying stages apply: they run after
// the deploy stage worked the deployment out, and the profile store is not
// updated until the end.
func TestAnApplyingStageStillReadsTheCarrierFile(t *testing.T) {
	store := t.TempDir()
	t.Setenv("HOME", store)

	current := setup.Defaults()
	current.ProfileName = "lab"
	current.ServicePort = "3010"
	if err := setup.SaveProfileSettings(current); err != nil {
		t.Fatalf("saving the profile: %v", err)
	}

	staged := current
	staged.ServicePort = "3000"
	carrier := filepath.Join(store, "staged.json")
	if err := staged.Write(carrier); err != nil {
		t.Fatalf("writing the carrier file: %v", err)
	}

	typed := map[string]bool{"vpn-profile": true}
	settings, err := settingsForRun("lab", carrier, setup.StageRoot, typed)
	if err != nil {
		t.Fatalf("resolving the settings: %v", err)
	}
	if settings.ServicePort != "3000" {
		t.Errorf("an applying stage read port %s instead of the staged 3000", settings.ServicePort)
	}
}

// Nobody named a profile, so the file names it — including for the first
// stage, which is how a front end that passes only the path still works.
func TestTheCarrierFileStillNamesTheProfileWhenNobodyElseDoes(t *testing.T) {
	store := t.TempDir()
	t.Setenv("HOME", store)

	staged := setup.Defaults()
	staged.ProfileName = "lab"
	staged.ServicePort = "3000"
	carrier := filepath.Join(store, "staged.json")
	if err := staged.Write(carrier); err != nil {
		t.Fatalf("writing the carrier file: %v", err)
	}

	settings, err := settingsForRun("default", carrier, setup.StageDeploy, map[string]bool{})
	if err != nil {
		t.Fatalf("resolving the settings: %v", err)
	}
	if settings.ProfileName != "lab" || settings.ServicePort != "3000" {
		t.Errorf("the file did not name the deployment: %s on port %s",
			settings.ProfileName, settings.ServicePort)
	}
}
