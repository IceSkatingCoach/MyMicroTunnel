# The installer is a Go binary and the app is built with swiftc. Nothing here
# needs Node, an AWS CLI, or an Xcode project.

install: engine
	./build/wiregard-mini-vpn install

uninstall: engine
	./build/wiregard-mini-vpn uninstall

engine:
	mkdir -p build
	go build -trimpath -ldflags "-s -w" -o build/wiregard-mini-vpn ./cmd/wiregard-mini-vpn

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
	gofmt -l .

clean:
	$(MAKE) -C menubar clean
	rm -rf build

.PHONY: install uninstall engine app pkg pkg-notarized test clean
