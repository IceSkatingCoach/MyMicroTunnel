# SPDX-License-Identifier: GPL-3.0-or-later
# The installer is a Go binary and the app is built with swiftc. Nothing here
# needs Node, an AWS CLI, or an Xcode project.

VERSION := $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
MODULE  := github.com/IceSkatingCoach/MyMicroTunnel
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT)

export VERSION
export COMMIT

# Passed through to the app build, including the one cmd/build-pkg runs for
# itself. Without the export, packaging rebuilds the bundle without them and
# quietly produces an app that never asks for an update — a feed published for
# nobody. cmd/appcast --validate is what catches that.
# Defaulted, not left empty, because these three are the constants of this
# product rather than choices to be made per release. The feed URL and the
# public key are compiled into every copy that ships: a typo in either
# orphans every installed copy, permanently and silently, and a flag typed by
# hand on a 200-character command line is exactly where that typo comes from.
#
# Override them on the command line only to publish a different product.
APPCAST_FEED_URL ?= https://downloads.maragato.ca/appcast.xml
APPCAST_BASE_URL ?= https://downloads.maragato.ca/releases
SPARKLE_PUBLIC_KEY ?= TUNOwHLcBYol4jkGwdL7Cif3UQFyvToyBxkFvZDhaBU=
export APPCAST_FEED_URL
export SPARKLE_PUBLIC_KEY

# The website's own stack. Overridable, but there is only ever one of it.
SITE_STACK ?= mymicrotunnel-site

# DOMAIN is the public hostname this deployment serves. There is no default:
# guessing one claims a name in somebody else's zone.
install: engine
	@test -n "$(DOMAIN)" || (echo "Usage: make install DOMAIN=updates.example.com" && false)
	./build/mymicrotunnel install --domain $(DOMAIN) $(INSTALL_FLAGS)

uninstall: engine
	./build/mymicrotunnel uninstall $(UNINSTALL_FLAGS)

# Universal, because a customer's Mac is whichever one they own. A Rosetta
# translation of the engine would work, but the app bundle it ships inside has
# to be universal anyway, and half a universal bundle is a bundle that refuses
# to launch.
# The one third-party binary this product ships. macOS has no kernel WireGuard,
# so something has to move the packets in userspace; everything else the tunnel
# needs is in internal/tunnel. Pinned and checksummed, see cmd/fetch-wireguard.
wireguard:
	go run ./cmd/fetch-wireguard

engine: wireguard
	mkdir -p build
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/mymicrotunnel.arm64 ./cmd/mymicrotunnel
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/mymicrotunnel.amd64 ./cmd/mymicrotunnel
	lipo -create -output build/mymicrotunnel build/mymicrotunnel.arm64 build/mymicrotunnel.amd64
	rm -f build/mymicrotunnel.arm64 build/mymicrotunnel.amd64
	# Beside the engine, which is where internal/tunnel looks for it first.
	cp third_party/wireguard-go build/wireguard-go

# Sparkle, for the self-update path. Pinned and checksummed like wireguard-go.
sparkle:
	go run ./cmd/fetch-sparkle

# Creates the EdDSA keypair, once, into the login Keychain. Prints the public
# half, which goes into the build as SPARKLE_PUBLIC_KEY. Back the private half
# up: losing it means no installed copy can ever be updated again.
sparkle-keys: sparkle
	./third_party/sparkle/bin/generate_keys

# APPCAST_BASE_URL is where the archives are published.
appcast:
	go run ./cmd/appcast --base-url $(APPCAST_BASE_URL)

appcast-validate:
	go run ./cmd/appcast --validate

# Points the feed back at a version that is already published, fetching that
# archive from the URL the feed advertises so the rollback is signed over the
# bytes users will actually download.
#
#   make rollback ROLLBACK_TO=1.0.2 APPCAST_BASE_URL=https://.../releases
rollback:
	@test -n "$(ROLLBACK_TO)" || (echo "Usage: make rollback ROLLBACK_TO=<version> APPCAST_BASE_URL=<url>" && false)
	go run ./cmd/appcast --rollback $(ROLLBACK_TO) --base-url $(APPCAST_BASE_URL)
	go run ./cmd/publish --force

# Deploys the feed's own infrastructure — the vendor's bucket and distribution,
# not a customer's stack. Run once; it prints the two URLs a release needs.
#
#   make feed-setup FEED_BUCKET=mymicrotunnel-downloads FEED_DOMAIN=downloads.example.com
feed-setup:
	@test -n "$(FEED_BUCKET)" || (echo "Usage: make feed-setup FEED_BUCKET=<name> [FEED_DOMAIN=<hostname>]" && false)
	go run ./cmd/publish --setup --bucket $(FEED_BUCKET) --domain "$(FEED_DOMAIN)"

# The product website, deployed once: the vendor's bucket and distribution,
# not a customer's stack. The hostname is a new one rather than an edit of an
# existing record, so this does not disturb whatever the zone already serves.
#
#   make site-setup SITE_BUCKET=mymicrotunnel-site-985658740042 \
#        SITE_DOMAIN=mymicrotunnel.maragato.ca HOSTED_ZONE_ID=Z00534512C27FKRK6YRDT
site-setup:
	@test -n "$(SITE_BUCKET)" || (echo "Usage: make site-setup SITE_BUCKET=<name> SITE_DOMAIN=<hostname> HOSTED_ZONE_ID=<id>" && false)
	@test -n "$(SITE_DOMAIN)" || (echo "site-setup needs SITE_DOMAIN" && false)
	@test -n "$(HOSTED_ZONE_ID)" || (echo "site-setup needs HOSTED_ZONE_ID" && false)
	aws cloudformation deploy --region us-east-1 \
		--template-file infra/cloudformation-site.yaml \
		--stack-name $(SITE_STACK) \
		--parameter-overrides BucketName=$(SITE_BUCKET) DomainName=$(SITE_DOMAIN) HostedZoneId=$(HOSTED_ZONE_ID) \
		--no-fail-on-empty-changeset
	aws cloudformation describe-stacks --region us-east-1 --stack-name $(SITE_STACK) \
		--query 'Stacks[0].Outputs' --output table

# Uploads the page and the one-click deploy-role template, then invalidates the
# cached copies. The bucket and distribution are read from the stack rather
# than repeated here, so the two cannot disagree about which site this is.
#
# deploy-role.yaml is uploaded twice on purpose: launch/ is the path the
# one-click link points at, and the copy at the root is the one people are
# told to read before they trust it.
site-publish:
	$(eval SITE_BUCKET_NAME := $(shell aws cloudformation describe-stacks --region us-east-1 \
		--stack-name $(SITE_STACK) --query 'Stacks[0].Outputs[?OutputKey==`BucketName`].OutputValue' --output text))
	$(eval SITE_DISTRIBUTION := $(shell aws cloudformation describe-stacks --region us-east-1 \
		--stack-name $(SITE_STACK) --query 'Stacks[0].Outputs[?OutputKey==`DistributionId`].OutputValue' --output text))
	@test -n "$(SITE_BUCKET_NAME)" || (echo "no $(SITE_STACK) stack; run make site-setup first" && false)
	aws s3 cp site/index.html s3://$(SITE_BUCKET_NAME)/index.html --content-type text/html
	aws s3 cp infra/cloudformation-deploy-role.yaml s3://$(SITE_BUCKET_NAME)/launch/deploy-role.yaml --content-type text/yaml
	aws s3 cp infra/cloudformation-deploy-role.yaml s3://$(SITE_BUCKET_NAME)/deploy-role.yaml --content-type text/yaml
	aws cloudfront create-invalidation --distribution-id $(SITE_DISTRIBUTION) --paths '/*' \
		--query 'Invalidation.Status' --output text

# Uploads the archive and the appcast, invalidates the cached feed, and reads
# the feed back over the public URL to prove what will actually be served.
publish:
	go run ./cmd/publish

# The whole release, in the order the steps depend on each other. Everything
# here is idempotent except publish, which refuses to overwrite a version that
# is already out.
#
#   make release PROFILE=microtunnel \
#        APPCAST_FEED_URL=https://downloads.example.com/appcast.xml \
#        SPARKLE_PUBLIC_KEY=... \
#        APPCAST_BASE_URL=https://downloads.example.com/releases
release:
	@test -n "$(PROFILE)" || (echo "release needs PROFILE=<notarytool profile>" && false)
	@test -n "$(APPCAST_FEED_URL)" || (echo "release needs APPCAST_FEED_URL" && false)
	@test -n "$(SPARKLE_PUBLIC_KEY)" || (echo "release needs SPARKLE_PUBLIC_KEY" && false)
	@test -n "$(APPCAST_BASE_URL)" || (echo "release needs APPCAST_BASE_URL" && false)
	$(MAKE) check
	$(MAKE) pkg-notarized PROFILE=$(PROFILE)
	$(MAKE) appcast
	$(MAKE) publish

app:
	$(MAKE) -C menubar app

# Signs with the Developer ID identities if they are in the keychain, and says
# what is missing if they are not.
pkg:
	go run ./cmd/build-pkg

# PROFILE is a notarytool keychain profile created once with
# `xcrun notarytool store-credentials`.
pkg-notarized:
	go run ./cmd/build-pkg --notarize $(PROFILE)

test:
	go vet ./...
	@test -z "$$(gofmt -l . | tee /dev/stderr)" || (echo "gofmt would change the files above" && false)
	go test ./...

# Everything CI runs, so a green local run means a green pull request.
check: test lint-template

# cfn-lint is optional locally and required in CI; skipping it quietly would
# make the two disagree.
lint-template:
	@command -v cfn-lint >/dev/null 2>&1 \
		&& cfn-lint infra/cloudformation-microtunnel.yaml infra/cloudformation-updates.yaml \
		|| echo "cfn-lint is not installed; skipping (pip install cfn-lint)"

clean:
	$(MAKE) -C menubar clean
	rm -rf build

.PHONY: install uninstall wireguard sparkle sparkle-keys appcast appcast-validate rollback feed-setup site-setup site-publish publish release engine app pkg pkg-notarized test check lint-template clean
