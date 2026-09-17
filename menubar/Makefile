APP    := XpremVpn.app
DEST   := build/$(APP)
BIN    := $(DEST)/Contents/MacOS/XpremVpn
ENGINE := $(DEST)/Contents/Resources/wiregard-mini-vpn

SOURCES := XpremVpn.swift SetupWindow.swift

# Ships as a plain swiftc build rather than an Xcode project: two source files,
# no dependencies, and nothing that needs the full Xcode install.
app: engine
	mkdir -p $(DEST)/Contents/MacOS $(DEST)/Contents/Resources
	swiftc -O -parse-as-library -target arm64-apple-macos13 -o $(BIN) $(SOURCES)
	cp Info.plist $(DEST)/Contents/Info.plist
	cp ../build/wiregard-mini-vpn $(ENGINE)
	# Ad-hoc signature keeps the bundle identity stable across rebuilds, so
	# macOS does not treat each build as a brand new app. Packaging replaces
	# this with the Developer ID signature.
	codesign --force --sign - $(DEST)

# The setup window drives this binary instead of reimplementing the deployment,
# so the app cannot be built without it.
engine:
	mkdir -p ../build
	cd .. && go build -trimpath -ldflags "-s -w" -o build/wiregard-mini-vpn ./cmd/wiregard-mini-vpn

install: app
	rm -rf /Applications/$(APP)
	cp -R $(DEST) /Applications/

clean:
	rm -rf build

.PHONY: app engine install clean
