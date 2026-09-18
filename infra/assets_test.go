// SPDX-License-Identifier: GPL-3.0-or-later
package infra

import (
	"regexp"
	"strings"
	"testing"
)

func TestTemplateIsEmbedded(t *testing.T) {
	// Leading comments are allowed — the licence header is one — so this looks
	// for the declaration at the start of a line rather than at the start of
	// the file.
	if !strings.HasPrefix(Template, "AWSTemplateFormatVersion") &&
		!strings.Contains(Template, "\nAWSTemplateFormatVersion") {
		t.Fatal("the embedded template does not declare AWSTemplateFormatVersion")
	}
	if !strings.Contains(Template, "\nResources:") {
		t.Fatal("the embedded template has no Resources section")
	}
}

func TestParametersFindsTheDeclaredInputs(t *testing.T) {
	found := map[string]bool{}
	required := map[string]bool{}
	for _, parameter := range Parameters() {
		found[parameter.Name] = true
		required[parameter.Name] = parameter.Required
	}

	// The account-specific ones. The first version of this template carried one
	// particular account's ids as defaults; a customer-owned account means
	// every one of them has to be supplied.
	for _, name := range []string{"VpcId", "VpcCidr", "SubnetIds", "GatewaySubnetIds", "RouteTableIds", "HostedZoneId", "ServiceDomainName"} {
		if !found[name] {
			t.Errorf("the template does not declare %s", name)
			continue
		}
		if !required[name] {
			t.Errorf("%s has a default, which would let it be silently wrong for this account", name)
		}
	}

	// The ones that should keep working with no answer at all.
	for _, name := range []string{"VpnCidr", "GatewayVpnAddress", "ServicePort", "HealthCheckPath", "GatewayInstanceType", "GatewayImageId", "AlarmEmail", "AlarmOnTunnelDown"} {
		if !found[name] {
			t.Errorf("the template does not declare %s", name)
			continue
		}
		if required[name] {
			t.Errorf("%s has no default, so every deploy has to answer for it", name)
		}
	}
}

// The old template named one account's resources. Shipping that to a customer
// deploys into a VPC that does not exist, or worse, one that does.
func TestTemplateCarriesNoAccountSpecificIdentifiers(t *testing.T) {
	forbidden := regexp.MustCompile(`(vpc-[0-9a-f]{8,}|subnet-[0-9a-f]{8,}|rtb-[0-9a-f]{8,}|\b\d{12}\b)`)
	for index, line := range strings.Split(Template, "\n") {
		if match := forbidden.FindString(line); match != "" {
			t.Errorf("line %d names %s, which belongs to one particular account:\n%s", index+1, match, line)
		}
	}
}

func TestTemplateCarriesNoParticularCustomersHostname(t *testing.T) {
	// maragato.ca was the first deployment's hostname, and it was the default
	// of ServiceDomainName. A customer who accepted that default would have
	// tried to claim somebody else's name.
	if strings.Contains(Template, "maragato.ca") {
		t.Error("the template still names the first deployment's hostname")
	}
}

// Fn::Sub substitutes every ${...} it sees, including the ones bash meant for
// itself. An unescaped one is not a subtle bug: CloudFormation rejects the
// whole template with "variable ... not defined", which is only discovered on a
// deploy.
func TestUserDataEscapesBashBraceExpansions(t *testing.T) {
	known := map[string]bool{
		"AWS::StackName": true, "AWS::Region": true, "AWS::AccountId": true,
		"AWS::Partition": true, "AWS::URLSuffix": true,
	}
	for _, parameter := range Parameters() {
		known[parameter.Name] = true
	}
	// Supplied through Fn::Sub's second argument.
	known["AllocationId"] = true
	known["RouteTables"] = true

	reference := regexp.MustCompile(`\$\{([^}!][^}]*)\}`)
	for index, line := range strings.Split(Template, "\n") {
		for _, match := range reference.FindAllStringSubmatch(line, -1) {
			name := strings.TrimSpace(match[1])
			if known[name] {
				continue
			}
			// A resource attribute, e.g. ${GatewayEip.AllocationId}.
			if strings.Contains(name, ".") {
				continue
			}
			t.Errorf("line %d refers to ${%s}, which is neither a parameter nor escaped as ${!%s}:\n%s",
				index+1, name, name, line)
		}
	}
}

// The gateway is a group of one so that a dead instance is replaced rather than
// mourned. Three things have to be true for the replacement to be a working
// gateway rather than merely a running one.
func TestGatewayReplacesItselfUsefully(t *testing.T) {
	for _, expected := range []string{
		// It takes over the address every workstation dials.
		"aws ec2 associate-address",
		// It repoints the route that carries the load balancer's traffic.
		"aws ec2 replace-route",
		// It forwards packets that are not addressed to it.
		"--no-source-dest-check",
		// It keeps the identity the workstations' configs name.
		"SecureString",
	} {
		if !strings.Contains(Template, expected) {
			t.Errorf("the boot script never runs %q, so a replaced gateway would not serve", expected)
		}
	}
}

func TestPeersAreNotAStackParameter(t *testing.T) {
	// A peer as a parameter rewrote the boot script, which replaced the
	// instance, which took every other workstation offline to add one.
	for _, parameter := range Parameters() {
		if parameter.Name == "ClientPublicKey" {
			t.Error("the template still takes a peer's public key as a parameter")
		}
	}
	if !strings.Contains(Template, "/wireguard/peers") {
		t.Error("the template does not point the gateway at a peer list")
	}
}
