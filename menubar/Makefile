APP  := XpremVpn.app
DEST := build/$(APP)
BIN  := $(DEST)/Contents/MacOS/XpremVpn

# Ships as a plain swiftc build rather than an Xcode project: one source file,
# no dependencies, and nothing that needs the full Xcode install.
app:
	mkdir -p $(DEST)/Contents/MacOS $(DEST)/Contents/Resources
	swiftc -O -parse-as-library -target arm64-apple-macos13 -o $(BIN) XpremVpn.swift
	cp Info.plist $(DEST)/Contents/Info.plist
	# Ad-hoc signature keeps the bundle identity stable across rebuilds, so
	# macOS does not treat each build as a brand new app.
	codesign --force --sign - $(DEST)

install: app
	rm -rf /Applications/$(APP)
	cp -R $(DEST) /Applications/

clean:
	rm -rf build

.PHONY: app install clean
