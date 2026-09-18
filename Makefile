# SPDX-License-Identifier: GPL-3.0-or-later
# The installer is a Go binary and the app is built with swiftc. Nothing here
# needs Node, an AWS CLI, or an Xcode project.

VERSION := $(shell cat VERSION)
COMMIT  := $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)
MODULE  := github.com/IceSkatingCoach/wiregard_mini_vpn
LDFLAGS := -s -w \
	-X $(MODULE)/internal/version.Version=$(VERSION) \
	-X $(MODULE)/internal/version.Commit=$(COMMIT)

export VERSION
export COMMIT

# Passed through to the app build, including the one cmd/build-pkg runs for
# itself. Without the export, packaging rebuilds the bundle without them and
# quietly produces an app that never asks for an update — a feed published for
# nobody. cmd/appcast --validate is what catches that.
APPCAST_FEED_URL ?=
SPARKLE_PUBLIC_KEY ?=
export APPCAST_FEED_URL
export SPARKLE_PUBLIC_KEY

# DOMAIN is the public hostname this deployment serves. There is no default:
# guessing one claims a name in somebody else's zone.
install: engine
	@test -n "$(DOMAIN)" || (echo "Usage: make install DOMAIN=updates.example.com" && false)
	./build/wiregard-mini-vpn install --domain $(DOMAIN) $(INSTALL_FLAGS)

uninstall: engine
	./build/wiregard-mini-vpn uninstall $(UNINSTALL_FLAGS)

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
	GOOS=darwin GOARCH=arm64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/wiregard-mini-vpn.arm64 ./cmd/wiregard-mini-vpn
	GOOS=darwin GOARCH=amd64 go build -trimpath -ldflags "$(LDFLAGS)" -o build/wiregard-mini-vpn.amd64 ./cmd/wiregard-mini-vpn
	lipo -create -output build/wiregard-mini-vpn build/wiregard-mini-vpn.arm64 build/wiregard-mini-vpn.amd64
	rm -f build/wiregard-mini-vpn.arm64 build/wiregard-mini-vpn.amd64
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

# Deploys the feed's own infrastructure — the vendor's bucket and distribution,
# not a customer's stack. Run once; it prints the two URLs a release needs.
#
#   make feed-setup FEED_BUCKET=xpremvpn-downloads FEED_DOMAIN=downloads.example.com
feed-setup:
	@test -n "$(FEED_BUCKET)" || (echo "Usage: make feed-setup FEED_BUCKET=<name> [FEED_DOMAIN=<hostname>]" && false)
	go run ./cmd/publish --setup --bucket $(FEED_BUCKET) --domain "$(FEED_DOMAIN)"

# Uploads the archive and the appcast, invalidates the cached feed, and reads
# the feed back over the public URL to prove what will actually be served.
publish:
	go run ./cmd/publish

# The whole release, in the order the steps depend on each other. Everything
# here is idempotent except publish, which refuses to overwrite a version that
# is already out.
#
#   make release PROFILE=wiregard \
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
		&& cfn-lint infra/cloudformation-xprem-onprem-vpn.yaml infra/cloudformation-updates.yaml \
		|| echo "cfn-lint is not installed; skipping (pip install cfn-lint)"

clean:
	$(MAKE) -C menubar clean
	rm -rf build

.PHONY: install uninstall wireguard sparkle sparkle-keys appcast appcast-validate feed-setup publish release engine app pkg pkg-notarized test check lint-template clean
