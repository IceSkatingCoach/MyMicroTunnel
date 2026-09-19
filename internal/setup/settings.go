// SPDX-License-Identifier: GPL-3.0-or-later
// Package setup is the install and uninstall flow: what to ask, what to write,
// and what to check before reporting success.
package setup

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"regexp"
	"strconv"
	"strings"
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
	SudoersPath      = "/etc/sudoers.d/mymicrotunnel"
	InstalledAppPath = "/Applications/MyMicroTunnel.app"

	// CommandPath is the copy on the path, for people.
	CommandPath = "/usr/local/bin/mymicrotunnel"

	// HelperPath is the copy the sudoers rule names, and it is deliberately not
	// the one above.
	//
	// A NOPASSWD rule is only as trustworthy as the file it points at. The
	// first version of this product pointed at /opt/homebrew/bin/wg-quick,
	// which Homebrew installs into a directory owned by the user and group
	// admin, mode 775 — so the very user the rule names could replace that file
	// and become root without a password. Homebrew on Intel does the same to
	// /usr/local. /Library/PrivilegedHelperTools is root:wheel, is the location
	// Apple documents for exactly this, and is not somewhere a package manager
	// takes ownership of.
	HelperPath = "/Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper"

	// EmbeddedEngineDir holds the copy of wireguard-go the package ships, so a
	// customer needs neither Homebrew nor a second installer.
	EmbeddedEngineDir = "/usr/local/lib/mymicrotunnel"

	// MTU leaves room for the WireGuard header inside a 1500-byte path, with
	// enough margin for a PPPoE link underneath it.
	TunnelMTU = 1380
)

// Settings is the whole deployment in one struct: what the user answered, what
// was discovered about their account, and what the stack reported back.
//
// One of these is one *VPN profile*. A workstation can hold several, each with
// its own stack, its own tunnel subnet, its own WireGuard interface and its own
// set of exposed ports, so that a laptop can be behind two deployments at once
// — a work one and a personal one, say — without either knowing about the
// other. Everything that used to be a fixed path is derived from the profile
// name or the interface name for exactly that reason.
type Settings struct {
	// ProfileName names the VPN profile on this machine. It is not the AWS
	// profile below; the two are separate because one AWS account routinely
	// holds several deployments.
	ProfileName string `json:"profileName"`

	Profile         string `json:"profile"`
	AccessKeyID     string `json:"-"`
	SecretAccessKey string `json:"-"`
	Region          string `json:"region"`
	AccountID       string `json:"accountId"`

	StackName       string `json:"stackName"`
	DomainName      string `json:"domainName"`
	ServicePort     string `json:"servicePort"`
	HealthCheckPath string `json:"healthCheckPath"`

	// TcpPorts are exposed through the load balancer as plain TCP, in addition
	// to the TLS-terminated ServicePort on 443. Ten at most, because the
	// template has ten slots.
	TcpPorts []string `json:"tcpPorts,omitempty"`

	// IdleTimeoutMinutes switches the gateway off after that many minutes with
	// no traffic through the load balancer. Zero leaves it running.
	IdleTimeoutMinutes int `json:"idleTimeoutMinutes"`

	// AppPrincipalArn is who may assume the wake role: the identity this
	// workstation's AWS profile authenticates as. Discovered rather than
	// asked for, and empty falls back to trusting the account.
	AppPrincipalArn string `json:"appPrincipalArn"`

	// Discovered from the account rather than asked for. Kept in the settings
	// file so the privileged stage and a later uninstall see the same
	// deployment the deploy stage saw.
	HostedZoneID     string   `json:"hostedZoneId"`
	VpcID            string   `json:"vpcId"`
	VpcCidr          string   `json:"vpcCidr"`
	SubnetIDs        []string `json:"subnetIds"`
	GatewaySubnetIDs []string `json:"gatewaySubnetIds"`
	RouteTableIDs    []string `json:"routeTableIds"`

	AlarmEmail        string `json:"alarmEmail"`
	AlarmWebhook      string `json:"alarmWebhook"`
	AlarmOnTunnelDown bool   `json:"alarmOnTunnelDown"`

	InterfaceName  string `json:"interfaceName"`
	VpnCidr        string `json:"vpnCidr"`
	ClientAddress  string `json:"clientAddress"`
	GatewayAddress string `json:"gatewayAddress"`

	// PeerLabel names this workstation in the peer registry, so somebody
	// reading the list later can tell whose laptop each key belongs to.
	PeerLabel string `json:"peerLabel"`

	// Supervise installs the LaunchDaemon that holds the tunnel at whatever
	// state the menu bar last asked for, across reboots and sleep.
	Supervise bool `json:"supervise"`

	// Username owns the sudoers rule. Captured in the unprivileged stage,
	// because the privileged one may not be able to work it out.
	Username string `json:"username"`

	// Filled in after the stack is deployed.
	Endpoint        string `json:"endpoint"`
	ServerPublicKey string `json:"serverPublicKey"`
	TargetGroupARN  string `json:"targetGroupArn"`

	// TcpTargetGroups maps an exposed port to the target group the stack built
	// for it. Read back from the outputs rather than derived, because a slot
	// that is not in use has no target group at all.
	TcpTargetGroups map[string]string `json:"tcpTargetGroups,omitempty"`

	// The alias record's target, kept so an uninstall can delete exactly the
	// record this deployment wrote and leave one pointing elsewhere alone.
	LoadBalancerDNSName string `json:"loadBalancerDnsName"`
	LoadBalancerZoneID  string `json:"loadBalancerZoneId"`

	// What the workstation needs to bring a switched-off gateway back.
	WakeRoleARN      string `json:"wakeRoleArn"`
	GatewayGroupName string `json:"gatewayGroupName"`
}

func Defaults() Settings {
	return Settings{
		ProfileName: DefaultProfileName,
		// Region, DomainName and StackName have no useful default here. A
		// region guess deploys into the wrong continent and a hostname guess
		// claims a name in somebody else's zone; the stack name is derived
		// from the account and the region once the credentials are known, by
		// DefaultStackName.
		ServicePort:     "3000",
		HealthCheckPath: "/hc",
		InterfaceName:   "wg0",
		VpnCidr:         "10.100.0.0/24",
		ClientAddress:   "10.100.0.2",
		GatewayAddress:  "10.100.0.1",
		PeerLabel:       Hostname(),
	}
}

// Hostname is what this workstation is called in the peer registry. Falls back
// to something that is at least unique-ish rather than to an empty label.
func Hostname() string {
	name, err := os.Hostname()
	if err != nil || name == "" {
		return "workstation"
	}
	return strings.TrimSuffix(name, ".local")
}

// --- derived names ---------------------------------------------------------
//
// Every parameter this deployment owns lives under one prefix named for the
// stack. One account can hold several deployments, and the fixed
// /microtunnel/vpn/... path the first version used meant the second stack to boot
// would overwrite the first one's server key and silently break its tunnel.

func (s Settings) ParameterPrefix() string {
	return "/" + s.StackName + "/wireguard"
}

func (s Settings) PeersParameter() string {
	return s.ParameterPrefix() + "/peers"
}

func (s Settings) ServerKeyParameter() string {
	return s.ParameterPrefix() + "/server-public-key"
}

func (s Settings) TunnelConfigPath() string {
	return "/etc/wireguard/" + s.InterfaceName + ".conf"
}

// ClientKeyPath is one private key per interface, because one machine can hold
// several tunnels and a shared key would mean two deployments trusting the
// same identity — and either of them revoking it for both.
func (s Settings) ClientKeyPath() string {
	name := s.InterfaceName
	if name == "" {
		name = Defaults().InterfaceName
	}
	return "/etc/wireguard/" + name + ".key"
}

// DefaultStackName is what a deployment is called when nobody says otherwise.
//
// The account and the region are in the name because the alternative — one
// fixed name — makes the second deployment in an account collide with the
// first, and because a stack name is the only thing a person sees in the
// CloudFormation console when they are looking at three of them.
func DefaultStackName(accountID, region string) string {
	if accountID == "" || region == "" {
		return ""
	}
	return "microtunnel-" + accountID + "-" + region
}

func (s Settings) ServiceURL() string {
	return "https://" + s.DomainName
}

func (s Settings) HealthCheckURL() string {
	return s.ServiceURL() + s.HealthCheckPath
}

func (s Settings) Port() int32 {
	port, err := strconv.Atoi(s.ServicePort)
	if err != nil {
		return 0
	}
	return int32(port)
}

// MaxTcpPorts is the number of slots the template declares. It is a hard limit
// rather than a soft one: an eleventh port has nowhere to go, and finding that
// out from a rejected change set is five minutes later than finding it out
// here.
const MaxTcpPorts = 10

// TcpPortNumbers is the exposed ports as numbers, in the order given, skipping
// anything that is not one.
func (s Settings) TcpPortNumbers() []int32 {
	ports := make([]int32, 0, len(s.TcpPorts))
	for _, raw := range s.TcpPorts {
		port, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || port < 1 || port > 65535 {
			continue
		}
		ports = append(ports, int32(port))
	}
	return ports
}

// ParseTcpPorts turns "5432, 6379" into the list the settings hold. Empty
// entries are dropped rather than rejected, so a trailing comma is not an
// error worth stopping an install for.
func ParseTcpPorts(list string) []string {
	var ports []string
	for _, field := range strings.Split(list, ",") {
		field = strings.TrimSpace(field)
		if field != "" {
			ports = append(ports, field)
		}
	}
	return ports
}

// --- validation ------------------------------------------------------------

var (
	hostnamePattern  = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?(\.[a-zA-Z0-9]([a-zA-Z0-9-]*[a-zA-Z0-9])?)+$`)
	stackNamePattern = regexp.MustCompile(`^[a-zA-Z][-a-zA-Z0-9]{0,127}$`)
	profilePattern   = regexp.MustCompile(`^[a-zA-Z0-9][-a-zA-Z0-9_]{0,31}$`)
	interfacePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,14}$`)
	regionPattern    = regexp.MustCompile(`^[a-z]{2}(-gov)?-[a-z]+-[0-9]$`)
	emailPattern     = regexp.MustCompile(`^[^@\s]+@[^@\s]+\.[^@\s]+$`)
)

// Validate refuses a deployment that cannot work before it costs anything.
//
// Every one of these used to be found by CloudFormation, five minutes into a
// deploy, as a rollback with a message about a parameter constraint. Some were
// not found at all: an interface name with a slash in it goes straight into a
// sudoers rule and a file path.
func (s Settings) Validate() error {
	var problems []string

	if !stackNamePattern.MatchString(s.StackName) {
		problems = append(problems, "the stack name must start with a letter and hold only letters, digits and hyphens")
	}
	if s.Region == "" {
		problems = append(problems, "no region: pass --region, or set one on the AWS profile")
	} else if !regionPattern.MatchString(s.Region) {
		problems = append(problems, fmt.Sprintf("%q is not a region name", s.Region))
	}
	if s.DomainName == "" {
		problems = append(problems, "no hostname: pass --domain with the name this deployment should serve")
	} else if !hostnamePattern.MatchString(s.DomainName) {
		problems = append(problems, fmt.Sprintf("%q is not a fully qualified hostname", s.DomainName))
	} else if strings.Count(s.DomainName, ".") < 2 {
		// A name has to sit *inside* a zone this account already holds: a host
		// label, the domain, and the top-level domain. Given "example.com" the
		// installer would look for a zone authoritative for it and then write
		// the zone apex — taking over the customer's bare domain, which is
		// usually where their website is.
		problems = append(problems, fmt.Sprintf(
			"%q has no host part: the hostname needs three labels, like updates.example.com, "+
				"so the record goes inside the zone rather than over it", s.DomainName))
	}

	port, err := strconv.Atoi(s.ServicePort)
	if err != nil || port < 1 || port > 65535 {
		problems = append(problems, fmt.Sprintf("%q is not a port number", s.ServicePort))
	}
	if !strings.HasPrefix(s.HealthCheckPath, "/") {
		problems = append(problems, "the health check path must start with /")
	}

	// The VPN profile's name becomes a directory under Application Support,
	// so it cannot be a path of its own.
	if !profilePattern.MatchString(s.ProfileName) {
		problems = append(problems, fmt.Sprintf(
			"%q is not a usable profile name: letters, digits, - and _, up to 32 characters", s.ProfileName))
	}

	problems = append(problems, s.portProblems()...)

	if s.IdleTimeoutMinutes < 0 || s.IdleTimeoutMinutes > 1440 {
		problems = append(problems, fmt.Sprintf(
			"the idle timeout is %d minutes; it has to be between 0 (never) and 1440", s.IdleTimeoutMinutes))
	}

	// A name that is not a plain interface name would be interpolated into
	// /etc/wireguard/<name>.conf and into the sudoers rule.
	if !interfacePattern.MatchString(s.InterfaceName) {
		problems = append(problems, fmt.Sprintf("%q is not a usable interface name", s.InterfaceName))
	}

	for label, address := range map[string]string{
		"the tunnel address of this machine": s.ClientAddress,
		"the tunnel address of the gateway":  s.GatewayAddress,
	} {
		parsed := net.ParseIP(address)
		if parsed == nil || parsed.To4() == nil {
			problems = append(problems, fmt.Sprintf("%s, %q, is not an IPv4 address", label, address))
		}
	}
	if s.ClientAddress != "" && s.ClientAddress == s.GatewayAddress {
		problems = append(problems, "this machine and the gateway cannot share a tunnel address")
	}

	// Both tunnel addresses are routed by the VPN subnet's route. An address
	// outside it is one the load balancer's packets would never be sent to.
	if _, tunnel, err := net.ParseCIDR(s.VpnCidr); err != nil {
		problems = append(problems, fmt.Sprintf("%q is not a subnet in CIDR notation", s.VpnCidr))
	} else {
		for label, address := range map[string]string{
			"this machine": s.ClientAddress,
			"the gateway":  s.GatewayAddress,
		} {
			if parsed := net.ParseIP(address); parsed != nil && !tunnel.Contains(parsed) {
				problems = append(problems, fmt.Sprintf(
					"the tunnel address of %s, %s, is outside the tunnel subnet %s", label, address, s.VpnCidr))
			}
		}
	}

	if s.AlarmEmail != "" && !emailPattern.MatchString(s.AlarmEmail) {
		problems = append(problems, fmt.Sprintf("%q is not an email address", s.AlarmEmail))
	}
	// SNS refuses a plain-HTTP subscription, and finding that out is a rolled
	// back deploy rather than a message.
	if s.AlarmWebhook != "" && !strings.HasPrefix(s.AlarmWebhook, "https://") {
		problems = append(problems, "the alarm webhook must be an https:// URL")
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("this deployment cannot work as described:\n  · %s", strings.Join(problems, "\n  · "))
}

// portProblems checks the exposed TCP ports on their own, because there are
// four separate ways to get them wrong and each one fails differently:
// a duplicate builds two target groups for one port and the second listener is
// rejected; 443 collides with the TLS listener this template already owns; an
// eleventh port has no slot; and a non-number reaches CloudFormation as a
// parameter constraint violation five minutes into a deploy.
func (s Settings) portProblems() []string {
	var problems []string

	if len(s.TcpPorts) > MaxTcpPorts {
		problems = append(problems, fmt.Sprintf(
			"%d TCP ports were asked for and the deployment has room for %d", len(s.TcpPorts), MaxTcpPorts))
	}

	seen := map[int]bool{}
	for _, raw := range s.TcpPorts {
		trimmed := strings.TrimSpace(raw)
		port, err := strconv.Atoi(trimmed)
		if err != nil || port < 1 || port > 65535 {
			problems = append(problems, fmt.Sprintf("%q is not a port number", trimmed))
			continue
		}
		if seen[port] {
			problems = append(problems, fmt.Sprintf("port %d is listed twice", port))
			continue
		}
		seen[port] = true
		if port == 443 {
			problems = append(problems, "port 443 already carries the TLS listener for "+s.DomainName)
		}
		if port == 51820 {
			problems = append(problems, "port 51820 is the WireGuard endpoint itself")
		}
	}
	return problems
}

// ValidateNetwork checks what discovery produced, separately from what the user
// typed, because the two fail for different reasons and at different times.
func (s Settings) ValidateNetwork() error {
	var problems []string

	if s.VpcID == "" || s.VpcCidr == "" {
		problems = append(problems, "no VPC was discovered")
	}
	if len(s.SubnetIDs) == 0 {
		problems = append(problems, "no public subnet was discovered")
	}
	if len(s.RouteTableIDs) == 0 {
		problems = append(problems, "no route table was discovered, so no traffic would reach the tunnel")
	}
	if s.HostedZoneID == "" {
		problems = append(problems, "no Route53 hosted zone is authoritative for "+s.DomainName)
	}

	// The tunnel subnet is routed inside the VPC. If the VPC already contains
	// it, that route hijacks addresses the VPC is using.
	if s.VpcCidr != "" && s.ClientAddress != "" {
		if _, network, err := net.ParseCIDR(s.VpcCidr); err == nil {
			if network.Contains(net.ParseIP(s.ClientAddress)) {
				problems = append(problems, fmt.Sprintf(
					"the tunnel address %s falls inside the VPC range %s; choose a tunnel subnet outside it",
					s.ClientAddress, s.VpcCidr))
			}
		}
	}

	if len(problems) == 0 {
		return nil
	}
	return fmt.Errorf("the account is not ready for this deployment:\n  · %s", strings.Join(problems, "\n  · "))
}

// --- files -----------------------------------------------------------------

// appConfig is the subset the menu bar app reads. Written separately from
// Settings so the app never sees deployment details it has no use for, and in
// particular never sees the AWS account.
type appConfig struct {
	ProfileName    string `json:"profileName"`
	InterfaceName  string `json:"interfaceName"`
	ClientAddress  string `json:"clientAddress"`
	GatewayAddress string `json:"gatewayAddress"`
	ServicePort    string `json:"servicePort"`
	// HelperPath is what the app runs through sudo to move the tunnel. Named in
	// the config rather than compiled into the app so the two cannot disagree
	// about which file the sudoers rule allows.
	HelperPath       string `json:"helperPath"`
	HealthCheckURL   string `json:"healthCheckUrl"`
	ServiceURL       string `json:"serviceUrl"`
	Supervised       bool   `json:"supervised"`
	DesiredStatePath string `json:"desiredStatePath"`

	// TcpPorts is shown in the menu, so somebody can see what a profile
	// publishes without opening the AWS console.
	TcpPorts []string `json:"tcpPorts,omitempty"`

	// What waking a switched-off gateway needs. The app runs the wake through
	// this tool rather than talking to AWS itself, so these are here to be
	// displayed and to say whether waking is possible at all.
	AwsProfile         string `json:"awsProfile"`
	Region             string `json:"region"`
	StackName          string `json:"stackName"`
	IdleTimeoutMinutes int    `json:"idleTimeoutMinutes"`
}

func WriteAppConfig(s Settings) error {
	directory := ProfileDir(s.ProfileName)
	if err := os.MkdirAll(directory, 0o755); err != nil {
		return err
	}

	encoded, err := json.MarshalIndent(appConfig{
		ProfileName:        s.ProfileName,
		InterfaceName:      s.InterfaceName,
		ClientAddress:      s.ClientAddress,
		GatewayAddress:     s.GatewayAddress,
		ServicePort:        s.ServicePort,
		HelperPath:         HelperPath,
		HealthCheckURL:     s.HealthCheckURL(),
		ServiceURL:         s.ServiceURL(),
		Supervised:         s.Supervise,
		DesiredStatePath:   s.DesiredStatePath(),
		TcpPorts:           s.TcpPorts,
		AwsProfile:         s.Profile,
		Region:             s.Region,
		StackName:          s.StackName,
		IdleTimeoutMinutes: s.IdleTimeoutMinutes,
	}, "", "  ")
	if err != nil {
		return err
	}

	return os.WriteFile(ProfileConfigPath(s.ProfileName), append(encoded, '\n'), 0o644)
}

// Port is the load balancer target's port. Zero when the config predates the
// field, which the caller reads as "do not try to deregister a target".
func (c appConfig) Port() int32 {
	port, err := strconv.Atoi(c.ServicePort)
	if err != nil {
		return 0
	}
	return int32(port)
}

// InstalledClientAddress is the tunnel address this machine actually uses,
// read from what the installer wrote rather than from the defaults.
func InstalledClientAddress() string {
	config := installedTunnel(DefaultProfileName)
	return config.ClientAddress
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

// Write saves the settings for the next stage. 0600 because the file names the
// account, the region and the stack: not secrets, but not everyone's business
// on a shared machine either.
func (s Settings) Write(path string) error {
	encoded, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o600)
}
