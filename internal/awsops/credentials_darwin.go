// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"fmt"
	"os/exec"
	"strings"
)

// CredentialStore names where the application's key is kept, for messages.
const CredentialStore = "login Keychain"

// keychainService namespaces the stored key. One entry per AWS account, so a
// machine that deploys into two accounts keeps two keys and neither is
// silently used for the other.
const keychainService = "ca.maragato.mymicrotunnel.aws"

// --- the Keychain ----------------------------------------------------------
//
// Through /usr/bin/security rather than the Security framework, because
// reaching the framework means cgo, and cgo means this tool stops
// cross-compiling for the two architectures it ships as one binary for.
//
// The item is a generic password, which is what the Keychain calls anything
// that is not tied to a server. It is encrypted with the login keychain and
// unlocked by the user's login, which is the protection this needs: a laptop
// that is off, or logged out, does not hand the key to anyone with the disk.

func StoreAppCredentials(accountID string, credentials *AppCredentials) error {
	if credentials == nil || credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return fmt.Errorf("refusing to store an incomplete credential")
	}

	// The two halves travel as one item: an access key id without its secret
	// is not a credential, and keeping them apart invites the pair to drift.
	value := credentials.AccessKeyID + ":" + credentials.SecretAccessKey

	// -U updates an existing item rather than failing, which is what a
	// re-mint after a revoked key has to do.
	command := exec.Command("/usr/bin/security", "add-generic-password",
		"-a", accountID, "-s", keychainService, "-w", value, "-U",
		"-D", "MyMicroTunnel AWS credentials",
		"-j", "Created by MyMicroTunnel. Deleting this makes the app ask for AWS credentials again.")
	if output, err := command.CombinedOutput(); err != nil {
		return fmt.Errorf("security add-generic-password: %s", strings.TrimSpace(string(output)))
	}
	return nil
}

// LoadAppCredentials returns nil, nil when there is nothing stored — a first
// run, or a machine whose Keychain item has been deleted on purpose.
func LoadAppCredentials(accountID string) (*AppCredentials, error) {
	command := exec.Command("/usr/bin/security", "find-generic-password",
		"-a", accountID, "-s", keychainService, "-w")
	output, err := command.Output()
	if err != nil {
		return nil, nil
	}

	id, secret, found := strings.Cut(strings.TrimSpace(string(output)), ":")
	if !found || id == "" || secret == "" {
		return nil, fmt.Errorf("the stored credential is not a key pair")
	}
	return &AppCredentials{AccessKeyID: id, SecretAccessKey: secret}, nil
}

// ForgetAppCredentials removes the stored key. Uninstalling should not leave
// a working AWS credential behind on a machine that no longer runs this.
func ForgetAppCredentials(accountID string) error {
	command := exec.Command("/usr/bin/security", "delete-generic-password",
		"-a", accountID, "-s", keychainService)
	if output, err := command.CombinedOutput(); err != nil {
		if strings.Contains(string(output), "could not be found") {
			return nil
		}
		return fmt.Errorf("security delete-generic-password: %s", strings.TrimSpace(string(output)))
	}
	return nil
}
