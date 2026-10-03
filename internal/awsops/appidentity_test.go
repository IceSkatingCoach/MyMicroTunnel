// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

const appUser = `<User><Path>/</Path><UserName>mymicrotunnel-app</UserName><UserId>AIDAAPP</UserId>` +
	`<Arn>arn:aws:iam::123456789012:user/mymicrotunnel-app</Arn><CreateDate>2026-01-01T00:00:00Z</CreateDate></User>`

func accessKeys(ids ...string) awsReply {
	var members strings.Builder
	for _, id := range ids {
		members.WriteString("<member><UserName>mymicrotunnel-app</UserName><AccessKeyId>" + id + "</AccessKeyId><Status>Active</Status></member>")
	}
	return queryOK("ListAccessKeys", "<AccessKeyMetadata>"+members.String()+"</AccessKeyMetadata><IsTruncated>false</IsTruncated>")
}

// mintingFake answers an account whose app user exists and holds the given
// keys, and that will mint AKIANEW.
func mintingFake(t *testing.T, existing ...string) *fakeAWS {
	t.Helper()
	fake := newFakeAWS(t)
	fake.reply("iam GetUser", queryOK("GetUser", appUser))
	fake.reply("iam ListAccessKeys", accessKeys(existing...))
	fake.reply("iam DeleteAccessKey", queryOK("DeleteAccessKey", ""))
	fake.reply("iam CreateAccessKey", queryOK("CreateAccessKey",
		"<AccessKey><UserName>mymicrotunnel-app</UserName><AccessKeyId>AKIANEW</AccessKeyId><Status>Active</Status><SecretAccessKey>newsecret</SecretAccessKey></AccessKey>"))
	return fake
}

func TestEnsureAppCredentialsMintsAndStoresAKey(t *testing.T) {
	isolateCredentialStore(t)
	fake := mintingFake(t, "AKIAOTHER")

	minted, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	if minted == nil || *minted != (AppCredentials{AccessKeyID: "AKIANEW", SecretAccessKey: "newsecret"}) {
		t.Errorf("got %+v", minted)
	}
	// One key in use elsewhere leaves room for this one; it is not revoked.
	if deleted := fake.made("iam DeleteAccessKey"); len(deleted) != 0 {
		t.Errorf("revoked a key with room to spare: %v", deleted)
	}
	for _, operation := range []string{"iam GetUser", "iam ListAccessKeys", "iam CreateAccessKey"} {
		if name := fake.made(operation)[0].Form.Get("UserName"); name != AppUserName {
			t.Errorf("%s asked about %q", operation, name)
		}
	}
	if stored, err := LoadAppCredentials("123456789012"); err != nil || stored == nil || *stored != *minted {
		t.Errorf("stored %+v, %v", stored, err)
	}
}

func TestEnsureAppCredentialsUsesTheStoredKeyWithoutAskingAWS(t *testing.T) {
	isolateCredentialStore(t)
	if err := StoreAppCredentials("123456789012", &AppCredentials{AccessKeyID: "AKIAKEPT", SecretAccessKey: "kept"}); err != nil {
		t.Fatal(err)
	}
	fake := newFakeAWS(t)

	got, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012")
	if err != nil || got == nil || got.AccessKeyID != "AKIAKEPT" {
		t.Errorf("got %+v, %v", got, err)
	}
	if calls := fake.operations(); len(calls) != 0 {
		t.Errorf("asked AWS anyway: %v", calls)
	}
}

// IAM allows two keys per user; a third machine would otherwise be refused.
func TestEnsureAppCredentialsMakesRoomForTheNewKey(t *testing.T) {
	isolateCredentialStore(t)
	fake := mintingFake(t, "AKIAOLD1", "AKIAOLD2")

	if _, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012"); err != nil {
		t.Fatal(err)
	}
	deleted := fake.made("iam DeleteAccessKey")
	if len(deleted) != 1 || deleted[0].Form.Get("AccessKeyId") != "AKIAOLD1" || deleted[0].Form.Get("UserName") != AppUserName {
		t.Errorf("expected exactly AKIAOLD1 to be revoked: %v", deleted)
	}
}

// A deployment from before the app user existed carries on with the
// credential it already has.
func TestEnsureAppCredentialsWithoutAnAppUser(t *testing.T) {
	isolateCredentialStore(t)
	fake := newFakeAWS(t)
	fake.reply("iam GetUser", queryError(http.StatusNotFound, "NoSuchEntity", "The user with name mymicrotunnel-app cannot be found."))

	got, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012")
	if got != nil || err != nil {
		t.Errorf("got %+v, %v, want nil, nil", got, err)
	}
	if len(fake.made("iam CreateAccessKey")) != 0 {
		t.Error("minted a key for a user that does not exist")
	}
}

func TestEnsureAppCredentialsSurfacesEachFailure(t *testing.T) {
	cases := []struct {
		operation string
		want      string
	}{
		{"iam GetUser", "looking for the mymicrotunnel-app user"},
		{"iam ListAccessKeys", "listing mymicrotunnel-app's keys"},
		{"iam DeleteAccessKey", "making room for a new key"},
		{"iam CreateAccessKey", "creating a key for mymicrotunnel-app"},
	}
	for _, test := range cases {
		t.Run(test.operation, func(t *testing.T) {
			isolateCredentialStore(t)
			fake := mintingFake(t, "AKIAOLD1", "AKIAOLD2")
			fake.reply(test.operation, queryError(http.StatusForbidden, "AccessDenied", "not allowed"))

			got, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012")
			if got != nil || err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "not allowed") {
				t.Errorf("got %+v, %v, want an error mentioning %q", got, err, test.want)
			}
		})
	}
}

func TestEnsureAppCredentialsReportsAKeyItCouldNotStore(t *testing.T) {
	isolateCredentialStore(t)
	breakCredentialStore(t)
	fake := mintingFake(t)

	got, err := EnsureAppCredentials(context.Background(), fake.client(), "123456789012")
	if got != nil || err == nil || !strings.Contains(err.Error(), "storing the key") {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestDeleteAppAccessKeyRevokesItInAWS(t *testing.T) {
	fake := newFakeAWS(t)
	fake.sequence("iam DeleteAccessKey",
		queryOK("DeleteAccessKey", ""),
		queryError(http.StatusNotFound, "NoSuchEntity", "The Access Key with id AKIAGONE cannot be found."),
		queryError(http.StatusForbidden, "AccessDenied", "not allowed"))
	client := fake.client()

	if err := client.DeleteAppAccessKey(context.Background(), ""); err != nil || len(fake.operations()) != 0 {
		t.Errorf("no key to revoke: got %v after %v", err, fake.operations())
	}
	if err := client.DeleteAppAccessKey(context.Background(), "AKIAAPP"); err != nil {
		t.Fatal(err)
	}
	form := fake.made("iam DeleteAccessKey")[0].Form
	if form.Get("AccessKeyId") != "AKIAAPP" || form.Get("UserName") != AppUserName {
		t.Errorf("unexpected revocation: %v", form)
	}
	if err := client.DeleteAppAccessKey(context.Background(), "AKIAGONE"); err != nil {
		t.Errorf("a key already revoked: %v", err)
	}
	if err := client.DeleteAppAccessKey(context.Background(), "AKIAAPP"); err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Errorf("got %v", err)
	}
}
