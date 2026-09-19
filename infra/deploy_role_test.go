// SPDX-License-Identifier: GPL-3.0-or-later
package infra

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"testing"
)

// The permissions exist in two places: docs/deploy-policy.json, which a person
// pastes into the console, and cloudformation-deploy-role.yaml, which creates
// the same policy for them. Two copies of a security boundary drift, and the
// drift is silent — an install that works for whoever used the template and
// fails for whoever pasted the JSON, or worse, the reverse.
//
// These tests hold them together.

type policyDocument struct {
	Statement []struct {
		Sid      string `json:"Sid"`
		Effect   string `json:"Effect"`
		Action   any    `json:"Action"`
		Resource any    `json:"Resource"`
	} `json:"Statement"`
}

func loadPolicyJSON(t *testing.T) policyDocument {
	t.Helper()

	content, err := os.ReadFile("../docs/deploy-policy.json")
	if err != nil {
		t.Fatalf("reading the policy document: %v", err)
	}

	var document policyDocument
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatalf("docs/deploy-policy.json is not valid JSON: %v", err)
	}
	return document
}

func deployRoleTemplate(t *testing.T) string {
	t.Helper()

	content, err := os.ReadFile("cloudformation-deploy-role.yaml")
	if err != nil {
		t.Fatalf("reading the deploy-role template: %v", err)
	}
	return string(content)
}

// withoutComments strips the prose. The template explains at length why it does
// *not* create an access key, and a check for that string against the whole
// file matches the explanation rather than a resource — a test that fails on
// the presence of the reason it passes.
func withoutComments(template string) string {
	var kept []string
	for _, line := range strings.Split(template, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

func TestDeployPolicyJSONIsValid(t *testing.T) {
	document := loadPolicyJSON(t)
	if len(document.Statement) == 0 {
		t.Fatal("the policy has no statements")
	}
	for index, statement := range document.Statement {
		if statement.Sid == "" {
			t.Errorf("statement %d has no Sid; the template matches on them", index)
		}
		if statement.Effect != "Allow" {
			t.Errorf("%s has effect %q; an installer policy should only allow",
				statement.Sid, statement.Effect)
		}
	}
}

// Every statement in the pasteable policy has to exist in the template, and the
// other way round. A permission in one and not the other is an install that
// works for half the people who follow the instructions.
func TestDeployRoleTemplateMatchesThePolicyDocument(t *testing.T) {
	document := loadPolicyJSON(t)
	template := deployRoleTemplate(t)

	var fromJSON []string
	for _, statement := range document.Statement {
		fromJSON = append(fromJSON, statement.Sid)
		if !strings.Contains(template, "Sid: "+statement.Sid) {
			t.Errorf("%s is in docs/deploy-policy.json but not in the template", statement.Sid)
		}
	}

	var fromTemplate []string
	for _, line := range strings.Split(template, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, found := strings.CutPrefix(trimmed, "- Sid: "); found {
			fromTemplate = append(fromTemplate, after)
		}
	}

	sort.Strings(fromJSON)
	sort.Strings(fromTemplate)
	if len(fromJSON) != len(fromTemplate) {
		t.Errorf("the policy has %d statements and the template %d:\n  json:     %v\n  template: %v",
			len(fromJSON), len(fromTemplate), fromJSON, fromTemplate)
	}
}

func TestDeployRoleTemplateGrantsEveryActionTheInstallerUses(t *testing.T) {
	template := deployRoleTemplate(t)

	// Not an exhaustive list — the one above covers that. These are the calls
	// whose absence produces a failure late in a deploy, after the customer has
	// already waited several minutes.
	for _, action := range []string{
		"cloudformation:CreateChangeSet",
		"cloudformation:GetTemplateSummary",
		"ec2:DescribeVpcs",
		"ec2:DescribeRouteTables",
		"route53:ListHostedZones",
		"route53:ChangeResourceRecordSets",
		"ssm:PutParameter",
		"elasticloadbalancing:RegisterTargets",
		"iam:PassRole",
		"autoscaling:CreateAutoScalingGroup",
	} {
		if !strings.Contains(template, action) {
			t.Errorf("the template does not grant %s", action)
		}
	}
}

// The template could create an access key and put the secret in an output.
// Stack outputs are stored by CloudFormation and readable by anyone who can
// describe the stack, for as long as it exists.
func TestDeployRoleTemplateDoesNotHandOutASecret(t *testing.T) {
	template := withoutComments(deployRoleTemplate(t))

	if strings.Contains(template, "AWS::IAM::AccessKey") {
		t.Error("the template creates an access key; its secret would live in the stack outputs")
	}
	if strings.Contains(template, "SecretAccessKey") {
		t.Error("the template references a secret access key")
	}
}

func TestDeployRoleTemplateOnlyAllows(t *testing.T) {
	// A Deny here would be a permission boundary, which is a different tool for
	// a different job, and easy to add by accident while copying statements.
	if strings.Contains(withoutComments(deployRoleTemplate(t)), "Effect: Deny") {
		t.Error("the deploy policy contains a Deny; it should only grant")
	}
}

// resourceBlock returns one resource's own lines: from its logical id to the
// next one at the same indentation. Slicing to the end of the file instead
// picks up every resource defined after it, which is how the first version of
// the test below reported the app user as carrying a policy that is merely
// declared underneath it.
func resourceBlock(template, logicalID string) string {
	start := strings.Index(template, "\n  "+logicalID+":\n")
	if start < 0 {
		return ""
	}
	rest := template[start+1:]
	for offset, line := range strings.Split(rest, "\n")[1:] {
		if strings.HasPrefix(line, "  ") && !strings.HasPrefix(line, "   ") &&
			strings.HasSuffix(strings.TrimSpace(line), ":") {
			return strings.Join(strings.Split(rest, "\n")[:offset+1], "\n")
		}
	}
	return rest
}

// The app's identity is the one that runs unattended for years on a laptop,
// so what it can do matters more than what the installer can.
func TestTheAppIdentityCanDeployAndDeleteOnlyItsOwnStacks(t *testing.T) {
	template := deployRoleTemplate(t)

	// It must be able to delete: a VPN profile that is removed takes its
	// stack with it, and an app that can only create leaves the expensive
	// half behind for somebody to find on a bill.
	if !strings.Contains(template, "cloudformation:DeleteStack") {
		t.Error("the app cannot delete a stack, so removing a profile would leave it running")
	}

	// And only its own. Scoped by name, because "delete any stack in the
	// account" is not a permission worth having on a laptop.
	if !strings.Contains(template, "stack/${StackNamePrefix}*") {
		t.Error("stack permissions are not scoped to this product's own stacks")
	}

	// The app must not be able to mint credentials — not even its own. That
	// grant belongs to the bootstrap identity, which is used once.
	if strings.Contains(resourceBlock(template, "AppUser"), "BootstrapPolicy") {
		t.Error("the app user carries the policy that can create access keys")
	}
	if !strings.Contains(template, "Sid: MintTheAppsKey") {
		t.Fatal("nothing can create the app's key, so setup cannot finish")
	}
	if !strings.Contains(template, "Resource: !GetAtt AppUser.Arn") {
		t.Error("the key-minting grant is not scoped to the app user alone")
	}
}
