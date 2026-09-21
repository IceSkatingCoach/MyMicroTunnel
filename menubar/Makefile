# SPDX-License-Identifier: GPL-3.0-or-later
APP    := MyMicroTunnel.app
DEST   := build/$(APP)
BIN    := $(DEST)/Contents/MacOS/MyMicroTunnel
ENGINE := $(DEST)/Contents/Resources/mymicrotunnel
WGGO   := $(DEST)/Contents/Resources/wireguard-go

SOURCES := MyMicroTunnel.swift SetupWindow.swift

SPARKLE  := ../third_party/sparkle
# Set both to build an app that can update itself. Left empty, the app builds
# and runs with the updater switched off and says so in its menu, which is what
# a source checkout wants.
APPCAST_FEED_URL   ?=
SPARKLE_PUBLIC_KEY ?=

# Inherited from the parent Makefile when it drives this one, and worked out
# here when somebody builds the app on its own.
VERSION ?= $(shell cat ../VERSION)
COMMIT  ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo unknown)

# Ships as a plain swiftc build rather than an Xcode project: two source files,
# no dependencies, and nothing that needs the full Xcode install.
#
# Built once per architecture and merged, because swiftc emits one slice at a
# time. macOS 13 is the floor for both.
app: engine
	mkdir -p $(DEST)/Contents/MacOS $(DEST)/Contents/Resources build/slices
	mkdir -p $(DEST)/Contents/Frameworks
	# Sparkle is a framework, so it is linked against here and copied into the
	# bundle below; @rpath is what lets the binary find it there at launch.
	swiftc -O -parse-as-library -target arm64-apple-macos13 \
		-F $(SPARKLE) -framework Sparkle \
		-Xlinker -rpath -Xlinker @executable_path/../Frameworks \
		-o build/slices/MyMicroTunnel.arm64 $(SOURCES)
	swiftc -O -parse-as-library -target x86_64-apple-macos13 \
		-F $(SPARKLE) -framework Sparkle \
		-Xlinker -rpath -Xlinker @executable_path/../Frameworks \
		-o build/slices/MyMicroTunnel.x86_64 $(SOURCES)
	# Removed first: lipo refuses to write over an existing output, and a
	# previous build performed as root leaves one nobody can overwrite.
	rm -f $(BIN)
	lipo -create -output $(BIN) build/slices/MyMicroTunnel.arm64 build/slices/MyMicroTunnel.x86_64
	rm -rf build/slices
	rm -rf $(DEST)/Contents/Frameworks/Sparkle.framework
	cp -R $(SPARKLE)/Sparkle.framework $(DEST)/Contents/Frameworks/
	sed -e 's/__VERSION__/$(VERSION)/g' -e 's/__BUILD__/$(COMMIT)/g' Info.plist > $(DEST)/Contents/Info.plist
	cp ../build/mymicrotunnel $(ENGINE)
	# Shipped inside the bundle so a customer needs neither Homebrew nor a
	# second installer before this one.
	cp ../third_party/wireguard-go $(WGGO)
	# The donation QR, shown by the setup window. Bundled rather than fetched,
	# so the panel works on a machine that is offline or behind a proxy.
	cp Resources/donate-qr.png $(DEST)/Contents/Resources/donate-qr.png
	# The updater keys are added only when there is somewhere to update from.
	# An SUFeedURL of "" makes Sparkle log an error nobody reads; no key at all
	# makes the app say plainly that it cannot update.
	@if [ -n "$(APPCAST_FEED_URL)" ] && [ -n "$(SPARKLE_PUBLIC_KEY)" ]; then \
		/usr/libexec/PlistBuddy -c 'Add :SUFeedURL string $(APPCAST_FEED_URL)' $(DEST)/Contents/Info.plist; \
		/usr/libexec/PlistBuddy -c 'Add :SUPublicEDKey string $(SPARKLE_PUBLIC_KEY)' $(DEST)/Contents/Info.plist; \
		/usr/libexec/PlistBuddy -c 'Add :SUEnableInstallerLauncherService bool true' $(DEST)/Contents/Info.plist; \
		echo "  updates: $(APPCAST_FEED_URL)"; \
	elif [ -n "$(APPCAST_FEED_URL)$(SPARKLE_PUBLIC_KEY)" ]; then \
		echo "  ! APPCAST_FEED_URL and SPARKLE_PUBLIC_KEY must both be set; building without updates"; \
	else \
		echo "  updates: disabled (no APPCAST_FEED_URL)"; \
	fi
	# Ad-hoc signature keeps the bundle identity stable across rebuilds, so
	# macOS does not treat each build as a brand new app. Packaging replaces
	# this with the Developer ID signature.
	codesign --force --sign - $(DEST)

# The setup window drives this binary instead of reimplementing the deployment,
# so the app cannot be built without it.
engine:
	$(MAKE) -C .. engine
	$(MAKE) -C .. sparkle

install: app
	rm -rf /Applications/$(APP)
	cp -R $(DEST) /Applications/

clean:
	rm -rf build

.PHONY: app engine install clean
