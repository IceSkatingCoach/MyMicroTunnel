// SPDX-License-Identifier: GPL-3.0-or-later
// Menu bar switch for the WireGuard tunnels that carry this machine's
// deployments from their AWS Network Load Balancers to the services here.
//
// A tunnel is deliberately not held up by a daemon that ignores the user: it
// should be up only while someone wants that deployment reachable, and down the
// rest of the time. This app is that switch, once per VPN profile.
//
// When the installer was asked for it, a LaunchDaemon watches the desired-state
// files this app writes and restores those states after a reboot or a sleep.
// The switch is still the only thing that decides; the daemon only remembers.
//
// Privileges: moving a tunnel has to happen as root. Rather than prompting
// for a password on every toggle, the app shells out through `sudo -n` to the
// helper in /Library/PrivilegedHelperTools, which the /etc/sudoers.d/mymicrotunnel
// drop-in allows for those two exact command lines per interface. Without that
// drop-in every toggle fails with a "password is required" message surfaced in
// the menu, which is the intended failure mode: no silent privilege grab.
//
// The helper is deliberately not the copy in /usr/local/bin. That directory is
// one Homebrew takes ownership of on Intel Macs, and a passwordless root grant
// on a file the user can overwrite is not a grant, it is a root shell.
//
// Waking is the one thing here that talks to AWS, and it does it by running the
// same helper *without* sudo — as the user, whose ~/.aws holds the profile. A
// deployment with an idle timeout switches its gateway off after a quiet spell,
// so connecting to one has to ask for the gateway back before there is anything
// at the far end to handshake with.

import AppKit
import Sparkle

/// One VPN profile as the installer described it, so no deployment's addresses
/// or hostname are compiled into the binary. The defaults match the
/// CloudFormation stack's own defaults, which keeps a hand-built copy of the
/// app working with no config file present.
struct Tunnel: Decodable {
    /// Which deployment on this machine. One Mac can be behind several.
    var profileName = "default"

    var interfaceName = "wg0"

    /// Address wg-quick assigns to this machine. Its presence on any utun
    /// interface is what the app treats as "connected" — cheaper than asking
    /// WireGuard, and it needs no privileges, unlike `wg show`.
    var clientAddress = "10.100.0.2"

    /// Tunnel address of the AWS-side gateway, pinged by "Test tunnel".
    var gatewayAddress = "10.100.0.1"

    /// The only binary the sudoers rule allows, read from the config rather
    /// than compiled in so the app and the rule cannot disagree about it.
    var helperPath = "/Library/PrivilegedHelperTools/ca.maragato.mymicrotunnel.helper"

    /// Empty until an install has written one. There is no sensible default:
    /// the hostname belongs to whoever deployed the stack.
    var healthCheckUrl = ""
    var serviceUrl = ""

    /// True when the installer registered the supervisor daemon, which reads
    /// the file below and puts the tunnel back where the user left it.
    var supervised = false

    /// Where this app records what the user last asked for. Writable without
    /// any privilege, which is the point: root reads it, the user writes it.
    var desiredStatePath = Tunnel.profilesDirectory
        .appendingPathComponent("default/desired-state").path

    /// Shown in the menu so the ports a deployment publishes are visible
    /// without opening the AWS console.
    var tcpPorts: [String] = []

    /// Non-zero when the gateway switches itself off after this many quiet
    /// minutes, which is what makes waking necessary before a connect.
    var idleTimeoutMinutes = 0

    static let supportDirectory = FileManager.default
        .homeDirectoryForCurrentUser
        .appendingPathComponent("Library/Application Support/MyMicroTunnel")

    static let profilesDirectory = supportDirectory.appendingPathComponent("profiles")

    // Decoded key by key rather than by the compiler's synthesised initialiser.
    //
    // That initialiser requires *every* key to be present: a property's default
    // value is used when you write `Tunnel()`, not when a key is missing from
    // the JSON. So the moment a new field is added here, every config written by
    // an older version fails to decode — not partially, entirely — and the app
    // falls back to compiled defaults. The switch then drives the wrong
    // interface at the wrong address through the wrong helper, with nothing
    // logged and nothing shown.
    //
    // That is a live hazard now that updates install themselves: the config on
    // disk is always at least one version behind the app that reads it.
    private enum CodingKeys: String, CodingKey {
        case profileName, interfaceName, clientAddress, gatewayAddress, helperPath
        case healthCheckUrl, serviceUrl, supervised, desiredStatePath
        case tcpPorts, idleTimeoutMinutes
    }

    init() {}

    init(from decoder: Decoder) throws {
        let values = try decoder.container(keyedBy: CodingKeys.self)

        if let value = try values.decodeIfPresent(String.self, forKey: .profileName) {
            profileName = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .interfaceName) {
            interfaceName = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .clientAddress) {
            clientAddress = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .gatewayAddress) {
            gatewayAddress = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .helperPath) {
            helperPath = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .healthCheckUrl) {
            healthCheckUrl = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .serviceUrl) {
            serviceUrl = value
        }
        if let value = try values.decodeIfPresent(Bool.self, forKey: .supervised) {
            supervised = value
        }
        if let value = try values.decodeIfPresent(String.self, forKey: .desiredStatePath) {
            desiredStatePath = value
        }
        if let value = try values.decodeIfPresent([String].self, forKey: .tcpPorts) {
            tcpPorts = value
        }
        if let value = try values.decodeIfPresent(Int.self, forKey: .idleTimeoutMinutes) {
            idleTimeoutMinutes = value
        }
    }

    /// Every profile installed on this machine.
    ///
    /// A profile with an unreadable config is skipped rather than replaced with
    /// defaults: guessing wg0 at 10.100.0.2 for a deployment that uses neither
    /// would put a switch in the menu that moves somebody else's tunnel.
    static func installed() -> [Tunnel] {
        let contents = (try? FileManager.default.contentsOfDirectory(
            at: profilesDirectory, includingPropertiesForKeys: nil)) ?? []

        var found: [Tunnel] = []
        for directory in contents.sorted(by: { $0.lastPathComponent < $1.lastPathComponent }) {
            let config = directory.appendingPathComponent("config.json")
            guard let data = try? Data(contentsOf: config),
                  var decoded = try? JSONDecoder().decode(Tunnel.self, from: data)
            else {
                continue
            }
            if decoded.profileName.isEmpty {
                decoded.profileName = directory.lastPathComponent
            }
            found.append(decoded)
        }
        return found
    }

    var healthCheckURL: URL? { healthCheckUrl.isEmpty ? nil : URL(string: healthCheckUrl) }

    /// Turns "reconnect at login" on or off for this profile.
    ///
    /// Written straight into the profile's own files rather than through a
    /// privileged helper, because nothing here needs privilege: the daemon
    /// already runs as root and reads the store on every pass, so flipping
    /// this field is the whole change. Both files are updated — settings.json
    /// is what the daemon reads, config.json is what this menu reads — and
    /// the rest of each file is preserved, since neither is this app's to
    /// rewrite from scratch.
    func setReconnectAtLogin(_ wanted: Bool) -> String? {
        let directory = Tunnel.profilesDirectory.appendingPathComponent(profileName)
        for (name, key) in [("settings.json", "supervise"), ("config.json", "supervised")] {
            let path = directory.appendingPathComponent(name)
            guard let data = try? Data(contentsOf: path),
                  var stored = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
            else {
                continue
            }
            stored[key] = wanted
            guard let encoded = try? JSONSerialization.data(
                    withJSONObject: stored, options: [.prettyPrinted, .sortedKeys]),
                  (try? encoded.write(to: path, options: .atomic)) != nil
            else {
                return "Could not write \(path.path)"
            }
        }
        return nil
    }

    /// Records the user's decision for the supervisor to act on later. Writing
    /// it always, supervised or not, means turning supervision on afterwards
    /// starts from the right state instead of from nothing.
    func recordDesiredState(_ up: Bool) {
        let directory = URL(fileURLWithPath: desiredStatePath).deletingLastPathComponent()
        try? FileManager.default.createDirectory(at: directory, withIntermediateDirectories: true)
        try? (up ? "up\n" : "down\n").write(
            toFile: desiredStatePath, atomically: true, encoding: .utf8)
    }
}

/// Shown in the menu so a support conversation starts from a fact. The commit
/// is its own key: CFBundleVersion is what Sparkle compares, so it carries the
/// release number rather than a hash that does not order.
var appVersion: String {
    let short = Bundle.main.object(forInfoDictionaryKey: "CFBundleShortVersionString") as? String ?? "dev"
    let commit = Bundle.main.object(forInfoDictionaryKey: "MyMicroTunnelBuildCommit") as? String ?? ""
    return commit.isEmpty ? short : "\(short) (\(commit))"
}

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

    /// Re-read on every poll rather than once at launch: installing a second
    /// profile while the app is running should put it in the menu, not require
    /// a quit and relaunch.
    private var profiles: [Tunnel] = []
    private var connected: Set<String> = []

    /// Names of the profiles with a helper call in flight, so each switch can
    /// be disabled on its own instead of freezing the whole menu.
    private var busy: Set<String> = []
    private var lastError: [String: String] = [:]
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

        // No profile means nothing has been deployed yet, so the first run
        // opens setup instead of leaving a switch that toggles nothing.
        if profiles.isEmpty {
            openSetup()
        }
    }

    // MARK: - State

    private func refreshState() {
        profiles = Tunnel.installed()

        // One ifconfig for every profile rather than one each: the addresses
        // are all in the same output, and this runs every three seconds.
        let addresses = run("/sbin/ifconfig", []).output
        connected = Set(profiles.filter { addresses.contains($0.clientAddress) }.map(\.profileName))

        updateStatusItemImage()
    }

    private func isConnected(_ profile: Tunnel) -> Bool { connected.contains(profile.profileName) }
    private func isBusy(_ profile: Tunnel) -> Bool { busy.contains(profile.profileName) }

    private func updateStatusItemImage() {
        guard let button = statusItem.button else { return }

        // The icon is the machine's state, not one profile's: filled when any
        // tunnel is up, because that is the question somebody glancing at the
        // menu bar is asking.
        let anyConnected = !connected.isEmpty
        let symbol = anyConnected ? "lock.shield.fill" : "lock.shield"
        let description = anyConnected ? "MyMicroTunnel connected" : "MyMicroTunnel disconnected"
        let image = NSImage(systemSymbolName: symbol, accessibilityDescription: description)
        // Template images follow the menu bar through light, dark and tinted
        // appearances instead of staying one fixed colour.
        image?.isTemplate = true

        button.image = image
        button.appearsDisabled = !busy.isEmpty
    }

    // MARK: - Menu

    func menuNeedsUpdate(_ menu: NSMenu) {
        // Rebuilt on every open so the state line cannot go stale between polls.
        refreshState()
        menu.removeAllItems()

        menu.addItem(disabledItem("VPN Profiles"))
        if profiles.isEmpty {
            menu.addItem(disabledItem("  none yet — Setup… creates the first"))
        }

        menu.addItem(.separator())
        addProfiles(to: menu)

        menu.addItem(.separator())

        let newProfile = NSMenuItem(title: "New VPN Profile…", action: #selector(openNewProfile),
                                    keyEquivalent: "n")
        newProfile.target = self
        newProfile.isEnabled = busy.isEmpty
        menu.addItem(newProfile)

        let setup = NSMenuItem(title: "Setup…", action: #selector(openSetup), keyEquivalent: ",")
        setup.target = self
        setup.isEnabled = busy.isEmpty
        menu.addItem(setup)

        let uninstall = NSMenuItem(title: "Uninstall…", action: #selector(uninstall), keyEquivalent: "")
        uninstall.target = self
        uninstall.isEnabled = busy.isEmpty
        menu.addItem(uninstall)

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
        menu.addItem(disabledItem("MyMicroTunnel \(appVersion)"))
        if updater == nil {
            menu.addItem(disabledItem("Built without an update feed"))
        }

        let quit = NSMenuItem(title: "Quit", action: #selector(quit), keyEquivalent: "q")
        quit.target = self
        menu.addItem(quit)
    }

    /// One profile's block of the menu.
    /// One line per profile, carrying its state, with everything that acts on
    /// that profile in a submenu beneath it.
    ///
    /// Flat until there are two. With one deployment the submenu is a second
    /// click for no information — there is nothing to tell it apart from —
    /// and with several, a flat menu is a list of identical verbs where
    /// choosing the wrong one moves somebody else's tunnel.
    private func addProfiles(to menu: NSMenu) {
        let nested = profiles.count > 1

        for profile in profiles {
            if !nested {
                menu.addItem(disabledItem(profile.profileName))
                menu.addItem(disabledItem("  " + state(of: profile)))
                addNotes(for: profile, to: menu)
                for item in actions(for: profile, shortcuts: true) {
                    menu.addItem(item)
                }
                continue
            }

            let header = NSMenuItem(
                title: "\(profile.profileName) — \(state(of: profile))",
                action: nil, keyEquivalent: "")
            // A tick on the profiles that are up, so the whole machine's state
            // is readable without opening anything.
            header.state = isConnected(profile) ? .on : .off

            let submenu = NSMenu()
            addNotes(for: profile, to: submenu)
            for item in actions(for: profile, shortcuts: false) {
                submenu.addItem(item)
            }
            header.submenu = submenu
            menu.addItem(header)
        }
    }

    private func state(of profile: Tunnel) -> String {
        if isBusy(profile) {
            return "Working…"
        }
        if isConnected(profile) {
            return "Connected — \(profile.clientAddress)"
        }
        return "Disconnected"
    }

    /// What is worth knowing about a profile but cannot be acted on.
    private func addNotes(for profile: Tunnel, to menu: NSMenu) {
        if let error = lastError[profile.profileName] {
            menu.addItem(disabledItem("Last error:"))
            for line in error.split(separator: "\n").prefix(4) {
                menu.addItem(disabledItem("  \(line)"))
            }
        }
        if profile.idleTimeoutMinutes > 0 {
            menu.addItem(disabledItem("Gateway sleeps after \(profile.idleTimeoutMinutes) idle minutes"))
        }
        if !profile.tcpPorts.isEmpty {
            menu.addItem(disabledItem("Also published: TCP \(profile.tcpPorts.joined(separator: ", "))"))
        }
        if menu.numberOfItems > 0 {
            menu.addItem(.separator())
        }
    }

    /// Everything that acts on one profile. Every item carries the profile's
    /// name, so an action can never reach the wrong tunnel.
    ///
    /// Shortcuts only in the flat layout: two items sharing one key
    /// equivalent means the key picks one of them at random, and for Connect
    /// that is somebody else's deployment.
    private func actions(for profile: Tunnel, shortcuts: Bool) -> [NSMenuItem] {
        var items: [NSMenuItem] = []

        let toggle = NSMenuItem(
            title: isConnected(profile) ? "Disconnect" : "Connect",
            action: #selector(toggleTunnel(_:)),
            keyEquivalent: shortcuts ? "c" : "")
        toggle.isEnabled = !isBusy(profile)
        items.append(toggle)

        let test = NSMenuItem(title: "Test tunnel", action: #selector(testTunnel(_:)),
                              keyEquivalent: shortcuts ? "t" : "")
        test.isEnabled = isConnected(profile) && !isBusy(profile)
        items.append(test)

        let open = NSMenuItem(title: "Open health check", action: #selector(openHealthCheck(_:)),
                              keyEquivalent: shortcuts ? "h" : "")
        // Nothing deployed yet means no hostname to open.
        open.isEnabled = profile.healthCheckURL != nil
        items.append(open)

        // Only for a deployment that can actually be asleep; on one that is
        // always running this is a button that does nothing.
        if profile.idleTimeoutMinutes > 0 {
            let wake = NSMenuItem(title: "Wake gateway", action: #selector(wakeGateway(_:)),
                                  keyEquivalent: "")
            wake.isEnabled = !isBusy(profile)
            items.append(wake)
        }

        // One decision with two states, so a checkmark rather than two items.
        let reconnect = NSMenuItem(title: "Reconnect at login", action: #selector(toggleReconnect(_:)),
                                   keyEquivalent: "")
        reconnect.state = profile.supervised ? .on : .off
        reconnect.isEnabled = !isBusy(profile)
        items.append(reconnect)

        for item in items {
            item.target = self
            item.representedObject = profile.profileName
        }
        return items
    }

    private func disabledItem(_ title: String) -> NSMenuItem {
        let item = NSMenuItem(title: title, action: nil, keyEquivalent: "")
        item.isEnabled = false
        return item
    }

    /// The profile a menu item belongs to. Menu items carry the name rather
    /// than the struct so a rebuilt menu never acts on a stale copy of a
    /// config that has since been rewritten by an install.
    private func profile(for sender: Any?) -> Tunnel? {
        guard let name = (sender as? NSMenuItem)?.representedObject as? String else {
            return profiles.first
        }
        return profiles.first { $0.profileName == name }
    }

    // MARK: - Actions

    @objc private func toggleTunnel(_ sender: Any?) {
        guard let profile = profile(for: sender) else { return }

        let wantUp = !isConnected(profile)
        let subcommand = wantUp ? "up" : "down"
        busy.insert(profile.profileName)
        lastError[profile.profileName] = nil
        updateStatusItemImage()

        // Recorded before the attempt rather than after it. The supervisor
        // reconciles towards this file, so a helper call that fails here is a
        // failure the daemon will retry rather than a decision that was lost.
        profile.recordDesiredState(wantUp)

        // The helper takes about a second; off the main thread so the menu bar
        // does not freeze while it runs.
        Task.detached(priority: .userInitiated) {
            // A gateway that switched itself off answers no handshake, and
            // nothing about that looks different from a broken tunnel. Asking
            // for it back first costs one API call on a deployment that is
            // already running, and is the difference between connecting and
            // staring at a switch that will not stay on.
            if wantUp && profile.idleTimeoutMinutes > 0 {
                _ = run(profile.helperPath, ["wake", "--vpn-profile", profile.profileName, "--quiet"])
            }

            let result = run(
                "/usr/bin/sudo",
                ["-n", profile.helperPath, "tunnel", subcommand, profile.interfaceName]
            )
            await MainActor.run {
                self.busy.remove(profile.profileName)
                self.lastError[profile.profileName] = result.succeeded ? nil : result.output
                self.refreshState()
                if !result.succeeded {
                    self.report(
                        title: "Could not bring \(profile.profileName) \(subcommand)",
                        message: result.output.isEmpty
                            ? "The helper at \(profile.helperPath) did not run. Re-run setup to reinstall it."
                            : result.output
                    )
                }
            }
        }
    }

    /// Interface presence only proves the helper ran. This proves packets cross.
    @objc private func testTunnel(_ sender: Any?) {
        guard let profile = profile(for: sender) else { return }

        busy.insert(profile.profileName)
        updateStatusItemImage()

        Task.detached(priority: .userInitiated) {
            let result = run("/sbin/ping", ["-c", "1", "-t", "3", profile.gatewayAddress])
            await MainActor.run {
                self.busy.remove(profile.profileName)
                self.updateStatusItemImage()
                if result.succeeded {
                    self.report(
                        title: "\(profile.profileName) is up",
                        message: "Gateway \(profile.gatewayAddress) answered."
                    )
                } else {
                    self.lastError[profile.profileName] = result.output
                    self.report(
                        title: "Gateway did not answer",
                        message: "The interface exists but \(profile.gatewayAddress) is unreachable. "
                            + (profile.idleTimeoutMinutes > 0
                                ? "Try \"Wake gateway\", then reconnect."
                                : "Reconnect to re-pin the tunnel after an address change.")
                    )
                }
            }
        }
    }

    /// Brings a gateway back that switched itself off on its idle timeout.
    ///
    /// Run through the helper without sudo: it needs the user's AWS profile,
    /// which lives in the user's home directory, and it needs no privileges at
    /// all beyond that.
    @objc private func wakeGateway(_ sender: Any?) {
        guard let profile = profile(for: sender) else { return }

        busy.insert(profile.profileName)
        updateStatusItemImage()

        Task.detached(priority: .userInitiated) {
            let result = run(profile.helperPath, ["wake", "--vpn-profile", profile.profileName])
            await MainActor.run {
                self.busy.remove(profile.profileName)
                self.updateStatusItemImage()
                if result.succeeded {
                    self.report(
                        title: "Gateway for \(profile.profileName) is up",
                        message: result.output.isEmpty ? "The gateway is running." : result.output
                    )
                } else {
                    self.lastError[profile.profileName] = result.output
                    self.report(title: "Could not wake the gateway", message: result.output)
                }
            }
        }
    }

    /// Flips whether a profile is put back up on its own after a reboot.
    ///
    /// The supervisor is machine-wide and is installed by setup, so a profile
    /// can ask to be reconnected on a Mac where no daemon has ever been
    /// installed. Saying so is better than silently recording a wish nothing
    /// acts on.
    @objc private func toggleReconnect(_ sender: Any?) {
        guard let profile = profile(for: sender) else { return }

        let wanted = !profile.supervised
        if let problem = profile.setReconnectAtLogin(wanted) {
            report(title: "Could not change that", message: problem)
            return
        }
        refreshState()

        let daemon = "/Library/LaunchDaemons/ca.maragato.mymicrotunnel.supervisor.plist"
        if wanted && !FileManager.default.fileExists(atPath: daemon) {
            report(
                title: "Recorded, but nothing is watching yet",
                message: "\(profile.profileName) will reconnect at login once the background "
                    + "service is installed. Run Setup… for this profile and tick "
                    + "\"Reconnect this profile at login\"; that is the step that installs it."
            )
        }
    }

    @objc private func openNewProfile() {
        openSetup()
        setupController?.startNewProfile()
    }

    @objc private func openHealthCheck(_ sender: Any?) {
        guard let url = profile(for: sender)?.healthCheckURL else { return }
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

    /// Removes this machine from the deployment.
    ///
    /// Local only, and the dialog says so plainly. Taking down the AWS side
    /// needs credentials this app does not hold and should not: it would mean
    /// carrying a key capable of deleting a load balancer, in a menu bar item,
    /// for the one day somebody clicks it. The stack is removed from a terminal,
    /// deliberately, with the command spelled out below.
    @objc private func uninstall() {
        let target = profiles.first?.profileName ?? "default"

        let alert = NSAlert()
        alert.messageText = profiles.count > 1
            ? "Remove the \(target) profile from this Mac?"
            : "Remove MyMicroTunnel from this Mac?"
        alert.informativeText = """
            This drops the tunnel and removes the configuration for \(target). \
            The app and the background service go too when it is the last profile left.

            It leaves your private key at /etc/wireguard.

            "Remove" leaves the AWS stack running: the hostname keeps answering from \
            any other Mac registered to it, and keeps costing money. "Remove and delete \
            the AWS stack" takes the deployment down as well.

            To remove a different profile, run this in a terminal instead:
                mymicrotunnel uninstall --vpn-profile NAME --delete-stack --delete-keys
            """
        alert.alertStyle = .warning
        alert.addButton(withTitle: "Remove")
        alert.addButton(withTitle: "Remove and delete the AWS stack")
        alert.addButton(withTitle: "Cancel")

        NSApp.activate(ignoringOtherApps: true)
        let answer = alert.runModal()
        guard answer != .alertThirdButtonReturn else { return }

        // The stack is the part that keeps costing money, so it is worth one
        // question rather than a paragraph telling people to go and delete it
        // themselves — which is a thing nobody does. It is a separate button
        // because it is a separate consequence: the hostname stops answering
        // for every Mac registered to that deployment, not just this one.
        let deleteStack = answer == .alertSecondButtonReturn
        if deleteStack {
            let confirm = NSAlert()
            confirm.messageText = "Delete the AWS deployment for \(target)?"
            confirm.informativeText = """
                The load balancer, the gateway, the certificate and the DNS record go with it. \
                Any other Mac serving this hostname stops working, and the name stops resolving.

                This cannot be undone.
                """
            confirm.alertStyle = .critical
            confirm.addButton(withTitle: "Delete it")
            confirm.addButton(withTitle: "Cancel")
            guard confirm.runModal() == .alertFirstButtonReturn else { return }
        }

        let helper = profiles.first?.helperPath ?? Tunnel().helperPath
        // Started detached and then this app exits, because the uninstall may
        // remove /Applications/MyMicroTunnel.app — which is to say, the bundle
        // this code is running out of.
        var command = "\(shellQuote(helper)) uninstall --non-interactive --vpn-profile \(shellQuote(target))"
        if deleteStack {
            command += " --delete-stack"
        }
        let script = "do shell script \(appleScriptQuote(command)) with administrator privileges"

        let process = Process()
        process.executableURL = URL(fileURLWithPath: "/usr/bin/osascript")
        process.arguments = ["-e", script]
        do {
            try process.run()
        } catch {
            report(title: "Could not start the uninstall", message: error.localizedDescription)
            return
        }

        // Waited for, so the authorisation dialog is answered before the app
        // that raised it disappears.
        process.waitUntilExit()

        if process.terminationStatus != 0 {
            report(title: "Uninstall did not finish",
                   message: "Nothing was removed, or only part of it was. Run "
                          + "`mymicrotunnel uninstall` in a terminal to see why.")
            return
        }
        NSApp.terminate(nil)
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
enum MyMicroTunnelApp {
    @MainActor
    static func main() {
        let application = NSApplication.shared
        let delegate = AppDelegate()
        application.delegate = delegate
        application.run()
    }
}
