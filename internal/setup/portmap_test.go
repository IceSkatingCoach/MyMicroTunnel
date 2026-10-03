// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"os"
	"testing"
)

// A trailing comma is not a reason to stop an install.
func TestPortListsAreReadAsTyped(t *testing.T) {
	mappings, err := ParsePortMappings("5432, 3000:8080,")
	if err != nil {
		t.Fatal(err)
	}
	if len(mappings) != 2 || mappings[0].String() != "5432" || mappings[1].String() != "3000:8080" {
		t.Errorf("read %v", mappings)
	}
	if _, err := ParsePortMappings("5432, 3000:http"); err == nil {
		t.Error("a mapping to a name was accepted")
	}
}

func TestAnAppConfigWithoutAPortDeregistersNothing(t *testing.T) {
	if port := (appConfig{ServicePort: "3000"}).Port(); port != 3000 {
		t.Errorf("port %d", port)
	}
	if port := (appConfig{}).Port(); port != 0 {
		t.Errorf("a config predating the field has port %d", port)
	}
}

// The interface and address are what the installer wrote, not the defaults.
func TestTheInstalledTunnelIsReadFromTheAppConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := valid()
	s.ProfileName = DefaultProfileName
	s.InterfaceName = "wg4"
	s.ClientAddress = "10.100.0.7"
	if err := WriteAppConfig(s); err != nil {
		t.Fatal(err)
	}

	if name, address := InstalledTunnelOf(DefaultProfileName); name != "wg4" || address != "10.100.0.7" {
		t.Errorf("installed tunnel is %s %s", name, address)
	}
	if address := InstalledClientAddress(); address != "10.100.0.7" {
		t.Errorf("installed address is %s", address)
	}
}

func TestAnUnreadableAppConfigFallsBackToTheDefaults(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := os.MkdirAll(ProfileDir("broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProfileConfigPath("broken"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if name, address := InstalledTunnelOf("broken"); name != "wg0" || address != "10.100.0.2" {
		t.Errorf("fell back to %s %s", name, address)
	}

	// The fields the file left empty fall back one by one.
	if err := os.WriteFile(ProfileConfigPath("broken"), []byte(`{"supervised":true}`), 0o644); err != nil {
		t.Fatal(err)
	}
	config := installedTunnel("broken")
	if config.ProfileName != "broken" || config.InterfaceName != "wg0" || config.HelperPath != HelperPath || !config.Supervised {
		t.Errorf("read %+v", config)
	}
}

// A profile whose settings cannot be read still has the interface the
// machine-wide files need.
func TestAProfileWithUnreadableSettingsStillCountsByItsAppConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	s := valid()
	s.ProfileName = "work"
	s.InterfaceName = "wg6"
	s.Supervise = true
	if err := WriteAppConfig(s); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ProfileSettingsPath("work"), []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}

	all := AllProfileSettings()
	if len(all) != 1 || all[0].ProfileName != "work" || all[0].InterfaceName != "wg6" || !all[0].Supervise {
		t.Errorf("read %+v", all)
	}
}
