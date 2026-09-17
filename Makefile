# Node 22.6+ strips the installer's types on its own, so `make install` needs
# no build step and no node_modules.
install:
	node installer/install.ts

uninstall:
	node installer/uninstall.ts

app:
	$(MAKE) -C menubar app

clean:
	$(MAKE) -C menubar clean

.PHONY: install uninstall app clean
