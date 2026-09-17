# Node 22.6+ strips the installer's types on its own, so `make install` needs
# no build step and no node_modules.
install:
	node installer/install.ts

uninstall:
	node installer/uninstall.ts

app:
	$(MAKE) -C menubar app

# Signs with the Developer ID identities if they are in the keychain, and says
# what is missing if they are not.
pkg:
	node packaging/build-pkg.ts

# PROFILE is a notarytool keychain profile created once with
# `xcrun notarytool store-credentials`.
pkg-notarized:
	node packaging/build-pkg.ts --notarize $(PROFILE)

clean:
	$(MAKE) -C menubar clean
	rm -rf build

.PHONY: install uninstall app pkg pkg-notarized clean
