// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import "testing"

// A trust policy that names an IAM Identity Center *session* is a role nobody
// can assume tomorrow: the session name changes on every sign-in, so the wake
// would work on the day of the install and never again.
func TestTrustablePrincipalReducesASessionToItsRole(t *testing.T) {
	cases := map[string]string{
		"arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Admin_abc/user@example.com": "arn:aws:iam::123456789012:role/AWSReservedSSO_Admin_abc",
		// Already stable: used as it stands.
		"arn:aws:iam::123456789012:user/deployer": "arn:aws:iam::123456789012:user/deployer",
		"arn:aws:iam::123456789012:role/Deployer": "arn:aws:iam::123456789012:role/Deployer",
		// Another partition keeps its own: "aws" is not hardcoded.
		"arn:aws-us-gov:sts::123456789012:assumed-role/Deployer/session": "arn:aws-us-gov:iam::123456789012:role/Deployer",
		// Nothing recognisable is returned untouched rather than mangled into
		// an ARN that would be rejected at deploy time with no explanation.
		"not-an-arn": "not-an-arn",
		"":           "",
	}

	for caller, want := range cases {
		if got := TrustablePrincipal(caller); got != want {
			t.Errorf("TrustablePrincipal(%q) = %q, want %q", caller, got, want)
		}
	}
}
