// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// CredentialStore names where the application's key is kept, for messages.
const CredentialStore = "~/.config/mymicrotunnel/aws (mode 0600)"

// Linux has no login keychain every machine can be assumed to run, and a
// headless host has no session to unlock one with. The key is kept the way
// ~/.aws/credentials keeps the user's own: a file only its owner can read, one
// per AWS account so a machine deploying into two keeps two keys.

func credentialPath(accountID string) (string, error) {
	if accountID == "" || strings.ContainsAny(accountID, `/\.`) {
		return "", fmt.Errorf("%q is not an AWS account id", accountID)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "mymicrotunnel", "aws", accountID), nil
}

func StoreAppCredentials(accountID string, credentials *AppCredentials) error {
	if credentials == nil || credentials.AccessKeyID == "" || credentials.SecretAccessKey == "" {
		return fmt.Errorf("refusing to store an incomplete credential")
	}
	path, err := credentialPath(accountID)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	// Written beside the target and renamed, so the file is never readable
	// by anyone else, not even briefly, and never half-written.
	staged := path + ".new"
	value := credentials.AccessKeyID + ":" + credentials.SecretAccessKey + "\n"
	if err := os.WriteFile(staged, []byte(value), 0o600); err != nil {
		return err
	}
	return os.Rename(staged, path)
}

// LoadAppCredentials returns nil, nil when there is nothing stored.
func LoadAppCredentials(accountID string) (*AppCredentials, error) {
	path, err := credentialPath(accountID)
	if err != nil {
		return nil, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, nil
	}
	id, secret, found := strings.Cut(strings.TrimSpace(string(content)), ":")
	if !found || id == "" || secret == "" {
		return nil, fmt.Errorf("the stored credential is not a key pair")
	}
	return &AppCredentials{AccessKeyID: id, SecretAccessKey: secret}, nil
}

// ForgetAppCredentials removes the stored key.
func ForgetAppCredentials(accountID string) error {
	path, err := credentialPath(accountID)
	if err != nil {
		return nil
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}
