// Package setup is the install and uninstall flow: what to ask, what to write,
// and what to check before reporting success.
package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// Stage exists because the graphical front end cannot prompt for a password on
// a terminal. It runs the deploy as the user, then the root-owned writes once
// through a single macOS authorisation dialog, then the rest. On a terminal all
// three run together and sudo prompts as usual.
type Stage string

const (
	StageAll    Stage = "all"
	StageDeploy Stage = "deploy"
	StageRoot   Stage = "root"
	StageFinish Stage = "finish"
)

const (
	ClientKeyPath    = "/etc/wireguard/client.key"
	SudoersPath      = "/etc/sudoers.d/xprem-vpn"
	InstalledAppPath = "/Applications/XpremVpn.app"
	ServerKeyParam   = "/xprem/vpn/server-public-key"
)

// Settings is the whole deployment in one struct: what the user answered, plus
// what the stack reported back.
type Settings struct {
	Profile         string `json:"profile"`
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
	Region          string `json:"region"`

	StackName    string `json:"stackName"`
	DomainName   string `json:"domainName"`
	DNSStackName string `json:"dnsStackName"`
	ServicePort  string `json:"servicePort"`

	InterfaceName  string `json:"interfaceName"`
	ClientAddress  string `json:"clientAddress"`
	GatewayAddress string `json:"gatewayAddress"`
	WgQuickPath    string `json:"wgQuickPath"`

	// Filled in after the stack is deployed.
	Endpoint        string `json:"endpoint"`
	ServerPublicKey string `json:"serverPublicKey"`
}

func Defaults() Settings {
	return Settings{
		Region:         "us-east-2",
		StackName:      "xprem-onprem-vpn",
		DomainName:     "update.mobile.maragato.ca",
		DNSStackName:   "xprem-dns",
		ServicePort:    "3000",
		InterfaceName:  "wg0",
		ClientAddress:  "10.100.0.2",
		GatewayAddress: "10.100.0.1",
		WgQuickPath:    "/opt/homebrew/bin/wg-quick",
	}
}

func (s Settings) TunnelConfigPath() string {
	return "/etc/wireguard/" + s.InterfaceName + ".conf"
}

func AppConfigDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, "Library", "Application Support", "XpremVpn")
}

func AppConfigPath() string {
	return filepath.Join(AppConfigDir(), "config.json")
}

// appConfig is the subset the menu bar app reads. Written separately from
// Settings so the app never sees deployment details it has no use for.
type appConfig struct {
	InterfaceName  string `json:"interfaceName"`
	ClientAddress  string `json:"clientAddress"`
	GatewayAddress string `json:"gatewayAddress"`
	WgQuickPath    string `json:"wgQuickPath"`
	HealthCheckURL string `json:"healthCheckUrl"`
}

func WriteAppConfig(s Settings) error {
	directory := AppConfigDir()
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(appConfig{
		InterfaceName:  s.InterfaceName,
		ClientAddress:  s.ClientAddress,
		GatewayAddress: s.GatewayAddress,
		WgQuickPath:    s.WgQuickPath,
		HealthCheckURL: "https://" + s.DomainName + "/hc",
	}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(AppConfigPath(), append(encoded, '\n'), 0o644)
}

func ReadSettings(path string) (Settings, error) {
	var s Settings
	content, err := os.ReadFile(path)
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(content, &s)
	return s, err
}

func (s Settings) Write(path string) error {
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}
