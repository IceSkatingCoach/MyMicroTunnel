// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// valid returns a deployment that should pass every check, so each test below
// can change exactly one thing and be sure that thing is what failed.
func valid() Settings {
	s := Defaults()
	s.Region = "us-east-2"
	s.DomainName = "updates.example.com"
	s.VpcCidr = "172.31.0.0/16"
	s.VpcID = "vpc-01234567890abcdef"
	s.SubnetIDs = []string{"subnet-aaa", "subnet-bbb"}
	s.GatewaySubnetIDs = s.SubnetIDs
	s.RouteTableIDs = []string{"rtb-aaa"}
	s.HostedZoneID = "Z123456789"
	s.StackName = DefaultStackName("123456789012", s.Region)
	return s
}

func TestValidateAcceptsAWorkingDeployment(t *testing.T) {
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid deployment was rejected: %v", err)
	}
	if err := valid().ValidateNetwork(); err != nil {
		t.Fatalf("a valid network was rejected: %v", err)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]struct {
		change func(*Settings)
		expect string
	}{
		"no hostname":            {func(s *Settings) { s.DomainName = "" }, "no hostname"},
		"a bare hostname":        {func(s *Settings) { s.DomainName = "updates" }, "not a fully qualified"},
		"a hostname with a path": {func(s *Settings) { s.DomainName = "example.com/hc" }, "not a fully qualified"},
		"no region":              {func(s *Settings) { s.Region = "" }, "no region"},
		"a region that is not":   {func(s *Settings) { s.Region = "us-east" }, "is not a region name"},
		"a stack name with a slash": {
			func(s *Settings) { s.StackName = "microtunnel/vpn" },
			"must start with a letter",
		},
		"a port that is not a number": {func(s *Settings) { s.ServicePort = "http" }, "is not a port number"},
		"a port out of range":         {func(s *Settings) { s.ServicePort = "70000" }, "is not a port number"},
		"a relative health path":      {func(s *Settings) { s.HealthCheckPath = "hc" }, "must start with /"},

		// An interface name reaches both a file path and a sudoers rule, so a
		// name with a separator in it is a privilege problem, not a typo.
		"an interface name with a slash": {
			func(s *Settings) { s.InterfaceName = "../../etc/passwd" },
			"not a usable interface name",
		},
		"an interface name with a space": {
			func(s *Settings) { s.InterfaceName = "wg0 x" },
			"not a usable interface name",
		},

		"a tunnel address that is not an address": {
			func(s *Settings) { s.ClientAddress = "ten dot one" },
			"is not an IPv4 address",
		},
		"one address for both ends": {
			func(s *Settings) { s.GatewayAddress = s.ClientAddress },
			"cannot share a tunnel address",
		},
		"a client address outside the tunnel subnet": {
			func(s *Settings) { s.ClientAddress = "192.168.9.9" },
			"outside the tunnel subnet",
		},
		"a tunnel subnet that is not a subnet": {
			func(s *Settings) { s.VpnCidr = "10.100.0.2" },
			"not a subnet in CIDR notation",
		},
		"an alarm address that is not one": {
			func(s *Settings) { s.AlarmEmail = "tell-ops" },
			"is not an email address",
		},
	}

	for name, test := range cases {
		t.Run(name, func(t *testing.T) {
			s := valid()
			test.change(&s)

			err := s.Validate()
			if err == nil {
				t.Fatalf("%s was accepted", name)
			}
			if !strings.Contains(err.Error(), test.expect) {
				t.Fatalf("the message for %s does not mention %q:\n%v", name, test.expect, err)
			}
		})
	}
}

func TestValidateReportsEveryProblemAtOnce(t *testing.T) {
	s := valid()
	s.DomainName = ""
	s.Region = ""
	s.ServicePort = "http"

	err := s.Validate()
	if err == nil {
		t.Fatal("three problems were accepted")
	}
	// Three round trips through a five-minute deploy to find three typos is
	// the failure mode this replaces.
	for _, expected := range []string{"no hostname", "no region", "is not a port number"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the message stops before mentioning %q:\n%v", expected, err)
		}
	}
}

func TestValidateNetworkRejectsATunnelInsideTheVpc(t *testing.T) {
	s := valid()
	s.VpcCidr = "10.0.0.0/8"

	err := s.ValidateNetwork()
	if err == nil {
		t.Fatal("a tunnel subnet inside the VPC was accepted")
	}
	if !strings.Contains(err.Error(), "falls inside the VPC range") {
		t.Fatalf("unexpected message: %v", err)
	}
}

func TestValidateNetworkRejectsAnEmptyDiscovery(t *testing.T) {
	err := (&Settings{DomainName: "updates.example.com"}).ValidateNetwork()
	if err == nil {
		t.Fatal("an account with nothing discovered was accepted")
	}
	for _, expected := range []string{"no VPC", "no public subnet", "no route table", "hosted zone"} {
		if !strings.Contains(err.Error(), expected) {
			t.Errorf("the message does not mention %q:\n%v", expected, err)
		}
	}
}

// Every parameter this deployment owns has to be namespaced by the stack.
// A fixed path meant a second deployment in one account overwrote the first
// one's server key and silently broke its tunnel.
func TestParameterNamesAreScopedToTheStack(t *testing.T) {
	first := Settings{StackName: "one"}
	second := Settings{StackName: "two"}

	if first.PeersParameter() == second.PeersParameter() {
		t.Error("two stacks share a peer list")
	}
	if first.ServerKeyParameter() == second.ServerKeyParameter() {
		t.Error("two stacks share a server key")
	}
	if got, want := first.PeersParameter(), "/one/wireguard/peers"; got != want {
		t.Errorf("peers parameter is %q, want %q", got, want)
	}
	if got, want := first.ServerKeyParameter(), "/one/wireguard/server-public-key"; got != want {
		t.Errorf("server key parameter is %q, want %q", got, want)
	}
}

func TestDerivedPaths(t *testing.T) {
	s := valid()
	s.InterfaceName = "wg1"
	s.HealthCheckPath = "/healthz"

	if got, want := s.TunnelConfigPath(), "/etc/wireguard/wg1.conf"; got != want {
		t.Errorf("tunnel config path is %q, want %q", got, want)
	}
	if got, want := s.HealthCheckURL(), "https://updates.example.com/healthz"; got != want {
		t.Errorf("health check URL is %q, want %q", got, want)
	}
	if got, want := s.Port(), int32(3000); got != want {
		t.Errorf("port is %d, want %d", got, want)
	}
}

func TestAppConfigCarriesWhatTheAppNeedsAndNothingElse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	s := valid()
	s.Profile = "a-profile"
	s.Supervise = true
	if err := WriteAppConfig(s); err != nil {
		t.Fatalf("writing the app config: %v", err)
	}

	content, err := os.ReadFile(ProfileConfigPath(s.ProfileName))
	if err != nil {
		t.Fatalf("reading it back: %v", err)
	}

	var written map[string]any
	if err := json.Unmarshal(content, &written); err != nil {
		t.Fatalf("it is not JSON: %v", err)
	}

	for _, key := range []string{"interfaceName", "clientAddress", "helperPath", "healthCheckUrl", "supervised"} {
		if _, present := written[key]; !present {
			t.Errorf("the app config has no %s", key)
		}
	}
	// The menu bar app has no business knowing about the AWS account, and the
	// file is world-readable.
	// The account itself is still none of the app's business. The region, the
	// AWS profile name and the stack are: waking a switched-off gateway runs
	// through this tool and has to know which deployment to wake.
	for _, key := range []string{"hostedZoneId", "vpcId", "subnetIds", "serverPublicKey", "accountId"} {
		if _, present := written[key]; present {
			t.Errorf("the app config leaks %s", key)
		}
	}
}

func TestSettingsRoundTripThroughTheStagingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "settings.json")

	s := valid()
	s.SecretAccessKey = "not-to-be-written"
	s.AccessKeyID = "also-not"
	if err := s.Write(path); err != nil {
		t.Fatalf("writing: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the staged settings are mode %o, want 600", mode)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading: %v", err)
	}
	// The file is handed to a second process and outlives the run; a secret
	// access key in it would outlive the run too.
	if strings.Contains(string(content), "not-to-be-written") {
		t.Error("the staged settings contain the secret access key")
	}

	read, err := ReadSettings(path)
	if err != nil {
		t.Fatalf("reading back: %v", err)
	}
	if read.DomainName != s.DomainName || read.VpcID != s.VpcID || len(read.SubnetIDs) != len(s.SubnetIDs) {
		t.Errorf("the deployment did not survive the round trip: %+v", read)
	}
	if read.SecretAccessKey != "" {
		t.Error("the secret access key survived the round trip")
	}
}

func TestHostnameIsNeverEmpty(t *testing.T) {
	if Hostname() == "" {
		t.Error("a workstation with no name would be an unlabelled peer")
	}
}

// The hostname has to sit inside a zone, not over it. Given a bare
// "example.com" the installer would find the zone authoritative for it and
// then write the apex — taking over the customer's own website, which is
// usually what a bare domain points at.
func TestValidateRequiresAHostPartInTheHostname(t *testing.T) {
	s := valid()
	s.DomainName = "example.com"

	err := s.Validate()
	if err == nil {
		t.Fatal("a bare domain was accepted as the hostname to serve")
	}
	if !strings.Contains(err.Error(), "three labels") {
		t.Errorf("the refusal does not explain what is needed: %v", err)
	}

	// Three labels, and more than three, are both fine: a zone can be
	// delegated several levels down.
	for _, hostname := range []string{"updates.example.com", "updates.eu.example.com"} {
		s.DomainName = hostname
		if err := s.Validate(); err != nil {
			t.Errorf("%s was rejected: %v", hostname, err)
		}
	}
}
