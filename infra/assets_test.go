// SPDX-License-Identifier: GPL-3.0-or-later
package infra

import (
	"fmt"
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
	// A resource's own logical id is a legal Fn::Sub reference too — an IAM
	// policy that scopes itself to this stack's Auto Scaling group is written
	// that way. Only a name that is neither is a bash expansion that escaped.
	for _, id := range logicalIDs() {
		known[id] = true
	}

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

// logicalIDs scans the resources the template declares, the same way
// Parameters scans its inputs and for the same reason: reading the one file
// that has to keep working on its own should not need a YAML library.
func logicalIDs() []string {
	var (
		found     []string
		inSection bool
	)
	for _, line := range strings.Split(Template, "\n") {
		if topLevelKey.MatchString(line) {
			inSection = topLevelKey.FindStringSubmatch(line)[1] == "Resources"
			continue
		}
		if !inSection {
			continue
		}
		if match := parameterKey.FindStringSubmatch(line); match != nil {
			found = append(found, match[1])
		}
	}
	return found
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

// The idle timeout has to be able to reach zero instances, and a group whose
// minimum is one cannot. The alarm, the policy and the floor are three halves
// of one feature: any of them missing leaves a deployment that claims to
// switch itself off and never does.
func TestTheIdleTimeoutCanActuallyReachZero(t *testing.T) {
	for _, expected := range []string{
		// The group is allowed to empty, but only when the timeout is set.
		"MinSize: !If [IdleStop, '0', '1']",
		// Something has to do the emptying, and a CloudWatch alarm cannot
		// terminate an instance it does not name by id.
		"AdjustmentType: ExactCapacity",
		"ScalingAdjustment: 0",
		// Silence is the signal, and an idle load balancer publishes no
		// datapoints at all — so missing data has to count as idle.
		"MetricName: ActiveFlowCount",
		"TreatMissingData: breaching",
	} {
		if !strings.Contains(Template, expected) {
			t.Errorf("the template does not contain %q, so the gateway would never switch itself off", expected)
		}
	}
}

// The workstation wakes its own gateway, several times a day, unattended. The
// credential it does that with must not be the one that can delete the
// deployment.
func TestTheWakeRoleCanOnlyWake(t *testing.T) {
	if !strings.Contains(Template, "autoscaling:SetDesiredCapacity") {
		t.Fatal("the template grants nothing that could bring the gateway back")
	}

	// Scoped to this stack's own group rather than to every group in the
	// account.
	if !strings.Contains(Template, "autoScalingGroupName/${GatewayGroup}") {
		t.Error("the wake grant is not scoped to this deployment's Auto Scaling group")
	}

	// The grant is checked by listing it rather than by looking for known-bad
	// strings: a new action added to this role should have to be justified
	// here, and a deny-list only catches the ones somebody thought of.
	start := strings.Index(Template, "PolicyName: wake-the-gateway")
	if start < 0 {
		t.Fatal("there is no wake role")
	}
	policy := Template[start:]
	if end := strings.Index(policy, "\n  # --- TLS"); end > 0 {
		policy = policy[:end]
	}

	action := regexp.MustCompile(`^\s+(?:- |Action: )((?:autoscaling|ec2|iam|ssm|sts|cloudformation|elasticloadbalancing|s3):[A-Za-z*]+)\s*$`)
	granted := map[string]bool{}
	for _, line := range strings.Split(policy, "\n") {
		if match := action.FindStringSubmatch(line); match != nil {
			granted[match[1]] = true
		}
	}

	allowed := map[string]bool{
		"autoscaling:SetDesiredCapacity":        true,
		"autoscaling:DescribeAutoScalingGroups": true,
		"ec2:DescribeInstances":                 true,
		"ec2:DescribeInstanceStatus":            true,
		"ec2:StartInstances":                    true,
	}
	for name := range granted {
		if !allowed[name] {
			t.Errorf("the wake role may also %s, which waking does not need", name)
		}
	}
	if !granted["autoscaling:SetDesiredCapacity"] {
		t.Error("the wake role cannot ask for an instance")
	}
}

// Ten slots, because the installer sends ten and CloudFormation rejects a
// change set that names a parameter the template does not declare.
func TestTheTemplateHasTenPortSlots(t *testing.T) {
	declared := map[string]bool{}
	for _, parameter := range Parameters() {
		declared[parameter.Name] = true
	}
	for slot := 1; slot <= 10; slot++ {
		name := fmt.Sprintf("TcpPort%d", slot)
		if !declared[name] {
			t.Errorf("the template does not declare %s", name)
		}
		for _, resource := range []string{"TcpTargetGroup%d", "TcpListener%d", "GatewayTcpIngress%d"} {
			if !strings.Contains(Template, fmt.Sprintf("  "+resource+":", slot)) {
				t.Errorf("slot %d has no %s", slot, fmt.Sprintf(resource, slot))
			}
		}
	}
}

// CloudFormation refuses a TemplateBody over 51,200 bytes, and the installer
// sends this one inline — it is embedded in the binary precisely so a deploy
// needs no bucket to stage it in.
//
// The limit was hit twice while this template grew, and shaving paragraphs to
// fit is a losing game that costs the next reader. What goes to the service is
// now the template without its prose, so this measures that.
func TestTemplateFitsCloudFormationsInlineLimit(t *testing.T) {
	const limit = 51200
	const margin = 1024

	if size := len(TemplateForDeploy()); size > limit-margin {
		t.Errorf("the deployable template is %d bytes; CloudFormation accepts %d inline and "+
			"this test keeps %d in reserve. Shorten a comment or stage the template in S3.",
			size, limit, margin)
	}
}

// Stripping prose must not touch the boot script, where a # line is content.
func TestStrippingProseLeavesTheBootScriptAlone(t *testing.T) {
	deployable := TemplateForDeploy()

	// The shebang above all: without it the instance runs the script under
	// whatever happens to be reading it, and nothing about the failure says
	// the template was edited on its way out.
	if !strings.Contains(deployable, "#!/bin/bash") {
		t.Error("the boot script lost its shebang")
	}
	// A comment from inside the script, at the script's indentation.
	if !strings.Contains(deployable, "# dnf's metadata parse is the memory high-water mark") {
		t.Error("the boot script lost the comments a person reads on the gateway at 2am")
	}
	// And a template-level one is gone, which is the point.
	if strings.Contains(deployable, "# WHY NOT AWS Site-to-Site VPN") {
		t.Error("template prose was not stripped, so the byte budget buys nothing")
	}

	// Nothing but comments may go: every parameter and resource must survive.
	for _, parameter := range Parameters() {
		if !strings.Contains(deployable, "  "+parameter.Name+":") {
			t.Errorf("stripping prose lost the parameter %s", parameter.Name)
		}
	}
	for _, id := range logicalIDs() {
		if !strings.Contains(deployable, "  "+id+":") {
			t.Errorf("stripping prose lost the resource %s", id)
		}
	}
}
