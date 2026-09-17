// Menu bar switch for the WireGuard tunnel that carries update.mobile.maragato.ca
// from the AWS Network Load Balancer to the xprem container on this machine.
//
// The tunnel is deliberately not a LaunchDaemon: it should be up only while
// someone wants the on-premises deployment reachable, and down the rest of the
// time. This app is that switch.
//
// Privileges: wg-quick has to run as root. Rather than prompting for a password
// on every toggle, the app shells out through `sudo -n`, which depends on the
// /etc/sudoers.d/xprem-vpn drop-in shipped next to this file. Without that
// drop-in every toggle fails with a "password is required" message surfaced in
// the menu, which is the intended failure mode: no silent privilege grab.

import AppKit

/// Settings the installer writes, so one deployment's addresses and hostname
/// are not compiled into the binary. The defaults match the CloudFormation
/// stack's own defaults, which keeps a hand-built copy of the app working with
/// no config file present.
struct Tunnel: Decodable {
    var interfaceName = "wg0"

    /// Address wg-quick assigns to this machine. Its presence on any utun
    /// interface is what the app treats as "connected" — cheaper than asking
    /// WireGuard, and it needs no privileges, unlike `wg show`.
    var clientAddress = "10.100.0.2"

    /// Tunnel address of the AWS-side gateway, pinged by "Test tunnel".
    var gatewayAddress = "10.100.0.1"

    var wgQuickPath = "/opt/homebrew/bin/wg-quick"
    var healthCheckUrl = "https://update.mobile.maragato.ca/hc"

    static let configURL = FileManager.default
        .homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/XpremVpn/config.json")

    static let current: Tunnel = {
        guard let data = try? Data(contentsOf: configURL),
              let decoded = try? JSONDecoder().decode(Tunnel.self, from: data)
        else {
            return Tunnel()
        }
        return decoded
    }()

    var healthCheckURL: URL? { URL(string: healthCheckUrl) }
}

/// Read once at launch. Changing the deployment means re-running the installer,
/// which rewrites the file and is expected to restart the app.
let tunnel = Tunnel.current

struct CommandResult {
    let status: Int32
    let output: String

    var succeeded: Bool { status == 0 }
}

/// Runs a command and merges stdout with stderr, because the messages worth
/// showing in the menu (sudo refusals, wg-quick errors) arrive on stderr.
func run(_ path: String, _ arguments: [String]) -> CommandResult {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: path)
    process.arguments = arguments

    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = pipe

    do {
        try process.run()
    } catch {
        return CommandResult(status: -1, output: "\(path): \(error.localizedDescription)")
    }

    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()

    let output = String(data: data, encoding: .utf8) ?? ""
    return CommandResult(
        status: process.terminationStatus,
        output: output.trimmingCharacters(in: .whitespacesAndNewlines)
    )
}

@MainActor
final class AppDelegate: NSObject, NSApplicationDelegate, NSMenuDelegate {
    private let statusItem = NSStatusBar.system.statusItem(withLength: NSStatusItem.variableLength)
    private var pollTimer: Timer?

    private var isConnected = false
    /// Set while a wg-quick call is in flight, so the menu can disable the
    /// toggle instead of letting two of them overlap.
    private var isBusy = false
    private var lastError: String?
    /// Held so the window is not deallocated the moment it is shown.
    private var setupController: SetupWindowController?

    func applicationDidFinishLaunching(_ notification: Notification) {
        // .accessory keeps the app out of the Dock and the app switcher; the
        // status item is the whole interface.
        NSApp.setActivationPolicy(.accessory)

        let menu = NSMenu()
        menu.delegate = self
        statusItem.menu = menu

        refreshState()

        // 3s is frequent enough that the icon tracks a tunnel brought up or
        // down from a terminal, and cheap enough to ignore: one ifconfig call.
        pollTimer = Timer.scheduledTimer(withTimeInterval: 3, repeats: true) { [weak self] _ in
            Task { @MainActor in self?.refreshState() }
        }

        // No config file means nothing has been deployed yet, so the first run
        // opens setup instead of leaving a switch that toggles nothing.
        if !FileManager.default.fileExists(atPath: Tunnel.configURL.path) {
            openSetup()
        }
    }

    // MARK: - State

    private func refreshState() {
        let result = run("/sbin/ifconfig", [])
        isConnected = result.output.contains(tunnel.clientAddress)
        updateStatusItemImage()
    }

    private func updateStatusItemImage() {
        guard let button = statusItem.button else { return }

        let symbol = isConnected ? "lock.shield.fill" : "lock.shield"
        let description = isConnected ? "xprem VPN connected" : "xprem VPN disconnected"
        let image = NSImage(systemSymbolName: symbol, accessibilityDescription: description)
        // Template images follow the menu bar through light, dark and tinted
        // appearances instead of staying one fixed colour.
        image?.isTemplate = true

        button.image = image
        button.appearsDisabled = isBusy
    }

    // MARK: - Menu

    func menuNeedsUpdate(_ menu: NSMenu) {
        // Rebuilt on every open so the state line cannot go stale between polls.
        refreshState()
        menu.removeAllItems()

        let state: String
        if isBusy {
            state = "Working…"
        } else if isConnected {
            state = "Connected — \(tunnel.clientAddress)"
        } else {
            state = "Disconnected"
        }
        menu.addItem(disabledItem(state))

        if let lastError {
            menu.addItem(.separator())
            menu.addItem(disabledItem("Last error:"))
            for line in lastError.split(separator: "\n").prefix(4) {
                menu.addItem(disabledItem("  \(line)"))
            }
        }

        menu.addItem(.separator())

        let toggle = NSMenuItem(
            title: isConnected ? "Disconnect" : "Connect",
            action: #selector(toggleTunnel),
            keyEquivalent: "c"
        )
        toggle.target = self
        toggle.isEnabled = !isBusy
        menu.addItem(toggle)

        let test = NSMenuItem(title: "Test tunnel", action: #selector(testTunnel), keyEquivalent: "t")
        test.target = self
        test.isEnabled = isConnected && !isBusy
        menu.addItem(test)

        let open = NSMenuItem(title: "Open health check", action: #selector(openHealthCheck), keyEquivalent: "h")
        open.target = self
        menu.addItem(open)

        let setup = NSMenuItem(title: "Setup…", action: #selector(openSetup), keyEquivalent: ",")
        setup.target = self
        setup.isEnabled = !isBusy
        menu.addItem(setup)

        menu.addItem(.separator())

        let quit = NSMenuItem(title: "Quit", action: #selector(quit), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)
    }

    private func disabledItem(_ title: String) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        return item
    }

    // MARK: - Actions

    @objc private func toggleTunnel() {
        let subcommand = isConnected ? "down" : "up"
        isBusy = true
        lastError = nil
        updateStatusItemImage()

        // wg-quick takes about a second; off the main thread so the menu bar
        // does not freeze while it runs.
        Task.detached(priority: .userInitiated) {
            let result = run("/usr/bin/sudo", ["-n", tunnel.wgQuickPath, subcommand, tunnel.interfaceName])
            await MainActor.run {
                self.isBusy = false
                self.lastError = result.succeeded ? nil : result.output
                self.refreshState()
                if !result.succeeded {
                    self.report(title: "wg-quick \(subcommand) failed", message: result.output)
                }
            }
        }
    }

    /// Interface presence only proves wg-quick ran. This proves packets cross.
    @objc private func testTunnel() {
        isBusy = true
        updateStatusItemImage()

        Task.detached(priority: .userInitiated) {
            let result = run("/sbin/ping", ["-c", "1", "-t", "3", tunnel.gatewayAddress])
            await MainActor.run {
                self.isBusy = false
                self.updateStatusItemImage()
                if result.succeeded {
                    self.report(
                        title: "Tunnel is up",
                        message: "Gateway \(tunnel.gatewayAddress) answered."
                    )
                } else {
                    self.lastError = result.output
                    self.report(
                        title: "Gateway did not answer",
                        message: "The interface exists but \(tunnel.gatewayAddress) is unreachable. "
                            + "Reconnect to re-pin the tunnel after an address change."
                    )
                }
            }
        }
    }

    @objc private func openHealthCheck() {
        guard let url = tunnel.healthCheckURL else { return }
        NSWorkspace.shared.open(url)
    }

    @objc private func openSetup() {
        if setupController == nil {
            setupController = SetupWindowController()
        }
        // An .accessory app has no windows of its own to come forward with.
        NSApp.activate(ignoringOtherApps: true)
        setupController?.showWindow(nil)
        setupController?.window?.makeKeyAndOrderFront(nil)
    }

    @objc private func quit() {
        NSApp.terminate(nil)
    }

    private func report(title: String, message: String) {
        let alert = NSAlert()
        alert.messageText = title
        alert.informativeText = message
        alert.alertStyle = .informational
        // An .accessory app has no windows to come forward on its own.
        NSApp.activate(ignoringOtherApps: true)
        alert.runModal()
    }
}

// Entry point is a @main type rather than top-level code because AppDelegate is
// @MainActor-isolated, and top-level statements are not. Built with
// -parse-as-library so swiftc honours this instead of script mode.
@main
enum XpremVpnApp {
    @MainActor
    static func main() {
        let application = NSApplication.shared
        let delegate = AppDelegate()
        application.delegate = delegate
        application.run()
    }
}
