// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"fmt"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
)

// The credentials the app runs on, and why they are not the ones the user
// typed.
//
// Setting up asks for an access key, or for a profile that holds one. That
// credential belongs to a person: it is often broad, it is used for other
// things, and on a laptop it would sit in ~/.aws for years being read by
// anything that runs as that user. The app needs far less than a person has,
// and it needs it unattended — to deploy a stack when a VPN profile is
// created, to delete it when the profile goes, and to wake a gateway several
// times a day.
//
// So the human credential is used exactly once: to mint a key for
// mymicrotunnel-app, the identity the one-click stack created with precisely
// the permissions this product uses and no others. That key goes into the
// login Keychain, which is encrypted at rest and gated by the user's own
// login, and the human credential is never read again.
//
// The app user cannot create access keys — not even its own. A key lifted off
// a laptop can deploy and delete this product's stacks, and cannot grow
// itself into anything better.

// AppUserName is the identity the app authenticates as. It matches the
// AppUserName parameter of cloudformation-deploy-role.yaml; the two are a
// contract, not a coincidence.
const AppUserName = "mymicrotunnel-app"

// AppCredentials is what the app authenticates with from then on.
type AppCredentials struct {
	AccessKeyID     string
	SecretAccessKey string
}

// EnsureAppCredentials returns the app's own credentials, minting them the
// first time with whatever the user authenticated as.
//
// Returns nil when the account has no app user, which is every deployment
// made before this existed: the caller then goes on using the credential it
// already has rather than refusing to work.
func EnsureAppCredentials(ctx context.Context, bootstrap *Client, accountID string) (*AppCredentials, error) {
	if stored, err := LoadAppCredentials(accountID); err == nil && stored != nil {
		return stored, nil
	}

	iamClient := iam.NewFromConfig(bootstrap.cfg)
	if _, err := iamClient.GetUser(ctx, &iam.GetUserInput{UserName: aws.String(AppUserName)}); err != nil {
		if strings.Contains(err.Error(), "NoSuchEntity") {
			return nil, nil
		}
		// An identity that may not even look at the app user cannot mint its
		// key either; say so plainly rather than failing later on the create.
		return nil, fmt.Errorf("looking for the %s user: %w", AppUserName, err)
	}

	// IAM allows two access keys per user, and a second machine setting itself
	// up would otherwise hit that limit with keys nobody holds. Keys this
	// account no longer has a record of are replaced rather than accumulated.
	existing, err := iamClient.ListAccessKeys(ctx, &iam.ListAccessKeysInput{
		UserName: aws.String(AppUserName),
	})
	if err != nil {
		return nil, fmt.Errorf("listing %s's keys: %w", AppUserName, err)
	}
	for _, key := range existing.AccessKeyMetadata {
		if len(existing.AccessKeyMetadata) < 2 {
			break
		}
		if _, err := iamClient.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
			UserName:    aws.String(AppUserName),
			AccessKeyId: key.AccessKeyId,
		}); err != nil {
			return nil, fmt.Errorf("making room for a new key: %w", err)
		}
		break
	}

	created, err := iamClient.CreateAccessKey(ctx, &iam.CreateAccessKeyInput{
		UserName: aws.String(AppUserName),
	})
	if err != nil {
		return nil, fmt.Errorf("creating a key for %s: %w", AppUserName, err)
	}

	credentials := &AppCredentials{
		AccessKeyID:     aws.ToString(created.AccessKey.AccessKeyId),
		SecretAccessKey: aws.ToString(created.AccessKey.SecretAccessKey),
	}
	if err := StoreAppCredentials(accountID, credentials); err != nil {
		return nil, fmt.Errorf("storing the key: %w", err)
	}
	return credentials, nil
}

// DeleteAppAccessKey revokes the key in AWS as well.
//
// Removing the Keychain item alone leaves a live credential belonging to a
// machine that has been uninstalled, which is exactly the kind of key nobody
// ever gets round to rotating.
func (c *Client) DeleteAppAccessKey(ctx context.Context, accessKeyID string) error {
	if accessKeyID == "" {
		return nil
	}
	iamClient := iam.NewFromConfig(c.cfg)
	_, err := iamClient.DeleteAccessKey(ctx, &iam.DeleteAccessKeyInput{
		UserName:    aws.String(AppUserName),
		AccessKeyId: aws.String(accessKeyID),
	})
	if err != nil && strings.Contains(err.Error(), "NoSuchEntity") {
		return nil
	}
	return err
}
