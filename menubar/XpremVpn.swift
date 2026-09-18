// SPDX-License-Identifier: GPL-3.0-or-later
// Menu bar switch for the WireGuard tunnel that carries this deployment's
// hostname from the AWS Network Load Balancer to the service on this machine.
//
// The tunnel is deliberately not held up by a daemon that ignores the user: it
// should be up only while someone wants the on-premises deployment reachable,
// and down the rest of the time. This app is that switch.
//
// When the installer was asked for it, a LaunchDaemon watches the desired-state
// file this app writes and restores that state after a reboot or a sleep. The
// switch is still the only thing that decides; the daemon only remembers.
//
// Privileges: moving the tunnel has to happen as root. Rather than prompting
// for a password on every toggle, the app shells out through `sudo -n` to the
// helper in /Library/PrivilegedHelperTools, which the /etc/sudoers.d/xprem-vpn
// drop-in allows for those two exact command lines. Without that drop-in every
// toggle fails with a "password is required" message surfaced in the menu,
// which is the intended failure mode: no silent privilege grab.
//
// The helper is deliberately not the copy in /usr/local/bin. That directory is
// one Homebrew takes ownership of on Intel Macs, and a passwordless root grant
// on a file the user can overwrite is not a grant, it is a root shell.

import AppKit
import Sparkle

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

    /// The only binary the sudoers rule allows, read from the config rather
    /// than compiled in so the app and the rule cannot disagree about it.
    var helperPath = "/Library/PrivilegedHelperTools/ca.maragato.xprem.vpn.helper"

    /// Empty until an install has written one. There is no sensible default:
    /// the hostname belongs to whoever deployed the stack.
    var healthCheckUrl = ""
    var serviceUrl = ""

    /// True when the installer registered the supervisor daemon, which reads
    /// the file below and puts the tunnel back where the user left it.
    var supervised = false

    /// Where this app records what the user last asked for. Writable without
    /// any privilege, which is the point: root reads it, the user writes it.
    var desiredStatePath = Tunnel.defaultDesiredStatePath

    static let supportDirectory = FileManager.default
        .homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/XpremVpn")

    static let configURL = supportDirectory.appendingPathComponent("config.json")

    static let defaultDesiredStatePath =
        supportDirectory.appendingPathComponent("desired-state").path

    static let current: Tunnel = {
        guard let data = try? Data(contentsOf: configURL),
              let decoded = try? JSONDecoder().decode(Tunnel.self, from: data)
        else {
            return Tunnel()
        }
        return decoded
    }()

    var healthCheckURL: URL? { healthCheckUrl.isEmpty ? nil : URL(string: healthCheckUrl) }

    /// Records the user's decision for the supervisor to act on later. Writing
    /// it always, supervised or not, means turning supervision on afterwards
    /// starts from the right state instead of from nothing.
    func recordDesiredState(_ up: Bool) {
        try? FileManager.default.createDirectory(
            at: Tunnel.supportDirectory, withIntermediateDirectories: true)
        try? (up ? "up\n" : "down\n").write(
            toFile: desiredStatePath, atomically: true, encoding: .utf8)
    }
}

/// Shown in the menu so a support conversation starts from a fact. The commit
/// is its own key: CFBundleVersion is what Sparkle compares, so it carries the
/// release number rather than a hash that does not order.
var appVersion: String {
    let short = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    let commit = Bundle.main.object(forInfoDictionaryKey: "XpremBuildCommit") as? String ?? ""
    return commit.isEmpty ? short : "\(short) (\(commit))"
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

    /// Sparkle, or nil when this build was made without a feed URL.
    ///
    /// A build with no feed is a perfectly good build — a source checkout, or a
    /// package handed over directly — and it should say so rather than start an
    /// updater that has nowhere to look. Sparkle logs an error and gives up in
    /// that case, which nobody sees.
    private let updater: SPUStandardUpdaterController? = {
        guard let feed = Bundle.main.object(forInfoDictionaryKey: "SUFeedURL") as? String,
              !feed.isEmpty
        else {
            return nil
        }
        return SPUStandardUpdaterController(
            startingUpdater: true,
            updaterDelegate: nil,
            userDriverDelegate: nil
        )
    }()

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
            // Bound to a constant before the Task rather than used as `self?.`
            // inside it. A weak capture is a mutable binding, and referring to
            // it from a concurrently-executing closure is an error under strict
            // concurrency checking — which the Swift on CI applies and the one
            // on a developer's Mac may not.
            guard let self else { return }
            Task { @MainActor in self.refreshState() }
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

        if tunnel.supervised {
            menu.addItem(disabledItem("Restored after a reboot"))
        }

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
        // Nothing has been deployed yet, so there is no hostname to open.
        open.isEnabled = tunnel.healthCheckURL != nil
        menu.addItem(open)

        let setup = NSMenuItem(title: "Setup…", action: #selector(openSetup), keyEquivalent: ",")
        setup.target = self
        setup.isEnabled = !isBusy
        menu.addItem(setup)

        if let updater {
            let check = NSMenuItem(
                title: "Check for Updates…",
                action: #selector(SPUStandardUpdaterController.checkForUpdates(_:)),
                keyEquivalent: ""
            )
            check.target = updater
            menu.addItem(check)
        }

        menu.addItem(.separator())
        menu.addItem(disabledItem("Xprem VPN \(appVersion)"))
        if updater == nil {
            menu.addItem(disabledItem("Built without an update feed"))
        }

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
        let wantUp = !isConnected
        let subcommand = wantUp ? "up" : "down"
        isBusy = true
        lastError = nil
        updateStatusItemImage()

        // Recorded before the attempt rather than after it. The supervisor
        // reconciles towards this file, so a wg-quick that fails here is a
        // failure the daemon will retry rather than a decision that was lost.
        tunnel.recordDesiredState(wantUp)

        // wg-quick takes about a second; off the main thread so the menu bar
        // does not freeze while it runs.
        Task.detached(priority: .userInitiated) {
            let result = run(
                "/usr/bin/sudo",
                ["-n", tunnel.helperPath, "tunnel", subcommand, tunnel.interfaceName]
            )
            await MainActor.run {
                self.isBusy = false
                self.lastError = result.succeeded ? nil : result.output
                self.refreshState()
                if !result.succeeded {
                    self.report(
                        title: "Could not bring the tunnel \(subcommand)",
                        message: result.output.isEmpty
                            ? "The helper at \(tunnel.helperPath) did not run. Re-run setup to reinstall it."
                            : result.output
                    )
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
