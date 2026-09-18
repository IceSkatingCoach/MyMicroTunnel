// SPDX-License-Identifier: GPL-3.0-or-later
// First-run setup, as a window rather than a terminal session.
//
// It does not reimplement the deployment: it drives the wiregard-mini-vpn
// binary shipped inside this bundle, reading the NDJSON events that binary
// emits with --json. One implementation of the AWS logic, two front ends that
// cannot drift apart.
//
// The run is split into three because a GUI has no terminal for sudo to prompt
// on:
//
//   1. --stage deploy   as the user, with their AWS credentials
//   2. --stage root     through one macOS authorisation dialog
//   3. --stage finish   as the user again: app config, login item, verification

import AppKit

@MainActor
final class SetupWindowController: NSWindowController {
    private let credentialMode = NSPopUpButton(frame: .zero, pullsDown: false)
    private let profileField = NSPopUpButton(frame: .zero, pullsDown: false)
    private let accessKeyField = NSTextField()
    private let secretKeyField = NSSecureTextField()
    private let regionField = NSTextField()
    private let stackField = NSTextField()
    private let domainField = NSTextField()
    private let portField = NSTextField()
    private let healthPathField = NSTextField()
    private let alarmEmailField = NSTextField()
    private let superviseCheckbox = NSButton(
        checkboxWithTitle: "Restore the tunnel after a reboot", target: nil, action: nil)

    private let logView = NSTextView()
    private let progress = NSProgressIndicator()
    private let installButton = NSPushButton(title: "Install", target: nil, action: nil)
    private let statusLabel = NSTextField(labelWithString: "")

    private var isRunning = false
    private let settingsPath = NSTemporaryDirectory() + "wiregard-setup.json"

    convenience init() {
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 640, height: 620),
            styleMask: [.titled, .closable, .miniaturizable],
            backing: .buffered,
            defer: false
        )
        window.title = "Xprem VPN Setup"
        // The engine's version, not the app's: the engine is what talks to AWS,
        // and in a source build the two can differ.
        window.subtitle = runBinary(["version"])
        window.center()
        self.init(window: window)
        buildLayout()
        loadProfiles()
    }

    // MARK: - Layout

    private func buildLayout() {
        guard let contentView = window?.contentView else { return }

        let form = NSStackView()
        form.orientation = .vertical
        form.alignment = .leading
        form.spacing = 10
        form.translatesAutoresizingMaskIntoConstraints = false

        credentialMode.addItems(withTitles: ["Use an existing AWS profile", "Enter an access key"])
        credentialMode.target = self
        credentialMode.action = #selector(credentialModeChanged)

        let defaults = SetupDefaults()
        regionField.stringValue = defaults.region
        regionField.placeholderString = "taken from the profile when left empty"
        stackField.stringValue = defaults.stackName
        domainField.placeholderString = "updates.example.com"
        portField.stringValue = defaults.servicePort
        healthPathField.stringValue = defaults.healthCheckPath
        alarmEmailField.placeholderString = "optional"

        form.addArrangedSubview(sectionLabel("AWS credentials"))
        form.addArrangedSubview(credentialMode)
        form.addArrangedSubview(labelled("Profile", profileField))
        form.addArrangedSubview(labelled("Access key id", accessKeyField))
        form.addArrangedSubview(labelled("Secret access key", secretKeyField))
        form.addArrangedSubview(labelled("Region", regionField))

        form.addArrangedSubview(spacer())
        form.addArrangedSubview(sectionLabel("Deployment"))
        form.addArrangedSubview(labelled("Stack name", stackField))
        form.addArrangedSubview(labelled("Public hostname", domainField))
        form.addArrangedSubview(labelled("Local service port", portField))
        form.addArrangedSubview(labelled("Health check path", healthPathField))
        form.addArrangedSubview(labelled("Notify on failure", alarmEmailField))
        form.addArrangedSubview(labelled("", superviseCheckbox))

        let costNote = NSTextField(wrappingLabelWithString:
            "Deploys AWS resources into your own account that cost roughly USD 26/month. "
            + "The VPC, subnets and Route53 zone are found automatically. You will be "
            + "asked to authorise one privileged step, which writes the tunnel "
            + "configuration, the sudoers rule and — if the box above is ticked — the "
            + "supervisor.")
        costNote.font = .systemFont(ofSize: 11)
        costNote.textColor = .secondaryLabelColor

        logView.isEditable = false
        logView.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
        let logScroll = NSScrollView()
        logScroll.documentView = logView
        logScroll.hasVerticalScroller = true
        logScroll.borderType = .bezelBorder
        logScroll.translatesAutoresizingMaskIntoConstraints = false

        progress.style = .spinning
        progress.controlSize = .small
        progress.isDisplayedWhenStopped = false
        progress.translatesAutoresizingMaskIntoConstraints = false

        installButton.target = self
        installButton.action = #selector(startInstall)
        installButton.keyEquivalent = "\r"

        let footer = NSStackView(views: [progress, statusLabel, NSView(), installButton])
        footer.orientation = .horizontal
        footer.spacing = 8
        footer.translatesAutoresizingMaskIntoConstraints = false

        let stack = NSStackView(views: [form, costNote, logScroll, footer])
        stack.orientation = .vertical
        stack.alignment = .leading
        stack.spacing = 14
        stack.edgeInsets = NSEdgeInsets(top: 20, left: 20, bottom: 20, right: 20)
        stack.translatesAutoresizingMaskIntoConstraints = false
        contentView.addSubview(stack)

        NSLayoutConstraint.activate([
            stack.topAnchor.constraint(equalTo: contentView.topAnchor),
            stack.leadingAnchor.constraint(equalTo: contentView.leadingAnchor),
            stack.trailingAnchor.constraint(equalTo: contentView.trailingAnchor),
            stack.bottomAnchor.constraint(equalTo: contentView.bottomAnchor),
            logScroll.heightAnchor.constraint(equalToConstant: 200),
            logScroll.widthAnchor.constraint(equalTo: stack.widthAnchor, constant: -40),
            footer.widthAnchor.constraint(equalTo: stack.widthAnchor, constant: -40),
        ])

        credentialModeChanged()
    }

    private func sectionLabel(_ text: String) -> NSTextField {
        let label = NSTextField(labelWithString: text)
        label.font = .boldSystemFont(ofSize: 13)
        return label
    }

    private func spacer() -> NSView {
        let view = NSView()
        view.translatesAutoresizingMaskIntoConstraints = false
        view.heightAnchor.constraint(equalToConstant: 4).isActive = true
        return view
    }

    private func labelled(_ title: String, _ control: NSView) -> NSStackView {
        let label = NSTextField(labelWithString: title)
        label.alignment = .right
        label.translatesAutoresizingMaskIntoConstraints = false
        label.widthAnchor.constraint(equalToConstant: 140).isActive = true

        control.translatesAutoresizingMaskIntoConstraints = false
        control.widthAnchor.constraint(equalToConstant: 420).isActive = true

        let row = NSStackView(views: [label, control])
        row.orientation = .horizontal
        row.spacing = 10
        return row
    }

    private func loadProfiles() {
        let listed = runBinary(["profiles"]).split(separator: "\n").map(String.init)
        profileField.removeAllItems()
        if listed.isEmpty {
            profileField.addItem(withTitle: "No profiles found")
            profileField.isEnabled = false
            credentialMode.selectItem(at: 1)
        } else {
            profileField.addItems(withTitles: listed)
        }
        credentialModeChanged()
    }

    @objc private func credentialModeChanged() {
        let usingProfile = credentialMode.indexOfSelectedItem == 0
        profileField.isHidden = !usingProfile
        accessKeyField.isHidden = usingProfile
        secretKeyField.isHidden = usingProfile
    }

    // MARK: - Running

    private func append(_ line: String) {
        logView.string += line + "\n"
        logView.scrollToEndOfDocument(nil)
    }

    @objc private func startInstall() {
        guard !isRunning else { return }

        // Caught here rather than by CloudFormation five minutes in. The engine
        // validates the same thing; this only saves the round trip.
        let domain = domainField.stringValue.trimmingCharacters(in: .whitespaces)
        guard domain.contains("."), !domain.hasPrefix("."), !domain.hasSuffix(".") else {
            let alert = NSAlert()
            alert.messageText = "A public hostname is needed"
            alert.informativeText =
                "Enter the fully qualified name this deployment should serve, such as "
                + "updates.example.com. A Route53 hosted zone in this account has to be "
                + "authoritative for it."
            alert.runModal()
            return
        }

        isRunning = true
        installButton.isEnabled = false
        progress.startAnimation(nil)
        statusLabel.stringValue = "Deploying…"
        logView.string = ""

        var arguments = [
            "install", "--json", "--non-interactive",
            "--settings", settingsPath,
            "--stack", stackField.stringValue,
            "--domain", domain,
            "--port", portField.stringValue,
            "--health-path", healthPathField.stringValue,
        ]

        // Left out entirely when empty, so the engine can fall back to the
        // region the profile already names rather than to a guess.
        if !regionField.stringValue.trimmingCharacters(in: .whitespaces).isEmpty {
            arguments += ["--region", regionField.stringValue]
        }
        if !alarmEmailField.stringValue.trimmingCharacters(in: .whitespaces).isEmpty {
            arguments += ["--alarm-email", alarmEmailField.stringValue]
        }
        if superviseCheckbox.state == .on {
            arguments += ["--supervise"]
        }

        if credentialMode.indexOfSelectedItem == 0 {
            arguments += ["--profile", profileField.titleOfSelectedItem ?? "default"]
        } else {
            arguments += [
                "--access-key-id", accessKeyField.stringValue,
                "--secret-access-key", secretKeyField.stringValue,
            ]
        }

        // Stage one: everything that needs AWS credentials, as this user.
        stream(arguments + ["--stage", "deploy"]) { [weak self] success in
            guard let self else { return }
            guard success else { return self.finish(success: false, message: "Deployment failed") }
            self.runRootStage()
        }
    }

    /// Stage two. `do shell script … with administrator privileges` is the one
    /// place a password is asked for, and it covers only the three root-owned
    /// writes: the private key, the tunnel config and the sudoers rule.
    private func runRootStage() {
        statusLabel.stringValue = "Waiting for authorisation…"
        append("▸ Authorising the privileged step")

        let binary = SetupEngine.binaryPath
        let command = "\(shellQuote(binary)) install --stage root --settings \(shellQuote(settingsPath))"
        let script = "do shell script \(appleScriptQuote(command)) with administrator privileges"

        DispatchQueue.global(qos: .userInitiated).async {
            let result = runCommand("/usr/bin/osascript", ["-e", script])
            DispatchQueue.main.async {
                guard result.status == 0 else {
                    self.append(result.output)
                    self.finish(success: false, message: "Authorisation failed")
                    return
                }
                self.append("  ✓ Tunnel configuration and sudoers rule written")
                self.runFinishStage()
            }
        }
    }

    private func runFinishStage() {
        statusLabel.stringValue = "Verifying…"
        var arguments = [
            "install", "--json", "--non-interactive", "--stage", "finish",
            "--settings", settingsPath, "--login-item",
        ]
        if superviseCheckbox.state == .on {
            arguments += ["--supervise"]
        }
        if credentialMode.indexOfSelectedItem == 0 {
            arguments += ["--profile", profileField.titleOfSelectedItem ?? "default"]
        }

        stream(arguments) { [weak self] success in
            self?.finish(success: success, message: success ? "Installed" : "Verification failed")
        }
    }

    private func finish(success: Bool, message: String) {
        isRunning = false
        installButton.isEnabled = true
        progress.stopAnimation(nil)
        statusLabel.stringValue = message

        guard success else { return }
        // The app reads its config once at launch, so it has to be restarted to
        // pick up a deployment that did not exist when it started.
        let alert = NSAlert()
        alert.messageText = "Setup complete"
        alert.informativeText =
            "The padlock shield in the menu bar now toggles \(domainField.stringValue). "
            + "The app will restart to load the new configuration."
        alert.runModal()

        let relaunch = Process()
        relaunch.executableURL = URL(fileURLWithPath: "/usr/bin/open")
        relaunch.arguments = ["-n", Bundle.main.bundlePath]
        try? relaunch.run()
        NSApp.terminate(nil)
    }

    /// Runs the binary and turns each NDJSON line into a log entry as it
    /// arrives, so a five-minute deploy shows progress rather than a frozen
    /// window.
    private func stream(_ arguments: [String], completion: @escaping (Bool) -> Void) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: SetupEngine.binaryPath)
        process.arguments = arguments

        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe

        var pending = ""
        pipe.fileHandleForReading.readabilityHandler = { handle in
            let chunk = handle.availableData
            guard !chunk.isEmpty, let text = String(data: chunk, encoding: .utf8) else { return }
            DispatchQueue.main.async {
                pending += text
                while let newline = pending.firstIndex(of: "\n") {
                    let line = String(pending[pending.startIndex..<newline])
                    pending = String(pending[pending.index(after: newline)...])
                    self.appendEvent(line)
                }
            }
        }

        process.terminationHandler = { finished in
            DispatchQueue.main.async {
                pipe.fileHandleForReading.readabilityHandler = nil
                completion(finished.terminationStatus == 0)
            }
        }

        do {
            try process.run()
        } catch {
            append("Could not run \(SetupEngine.binaryPath): \(error.localizedDescription)")
            completion(false)
        }
    }

    private func appendEvent(_ line: String) {
        guard let data = line.data(using: .utf8),
              let object = try? JSONSerialization.jsonObject(with: data) as? [String: Any],
              let kind = object["kind"] as? String
        else {
            // Anything that is not an event is still worth showing: it is
            // usually a Go panic or a message from a tool further down.
            if !line.isEmpty { append(line) }
            return
        }

        let message = object["message"] as? String ?? ""
        switch kind {
        case "step": append("▸ \(message)")
        case "done": append("  ✓ \(message)")
        case "warn": append("  ! \(message)")
        case "fail": append("  ✗ \(message)")
        case "result": break
        default: append("  \(message)")
        }
    }

}

/// Where the engine binary lives. Deliberately outside the window controller so
/// it is reachable from the background work too, not only from the main actor.
enum SetupEngine {
    /// Prefers the copy inside the bundle, so the app is self-contained, and
    /// falls back to the one the package puts on the path.
    static var binaryPath: String {
        if let bundled = Bundle.main.url(forResource: "wiregard-mini-vpn", withExtension: nil),
           FileManager.default.isExecutableFile(atPath: bundled.path) {
            return bundled.path
        }
        return "/usr/local/bin/wiregard-mini-vpn"
    }
}

// MARK: - Helpers

/// Only the answers that are the same for everyone. A region or a hostname
/// default would be one particular deployment's, and accepting it would claim
/// a name in somebody else's zone.
struct SetupDefaults {
    let region = ""
    let stackName = "xprem-onprem-vpn"
    let servicePort = "3000"
    let healthCheckPath = "/hc"
}

func runCommand(_ path: String, _ arguments: [String]) -> (status: Int32, output: String) {
    let process = Process()
    process.executableURL = URL(fileURLWithPath: path)
    process.arguments = arguments

    let pipe = Pipe()
    process.standardOutput = pipe
    process.standardError = pipe

    do {
        try process.run()
    } catch {
        return (-1, error.localizedDescription)
    }

    let data = pipe.fileHandleForReading.readDataToEndOfFile()
    process.waitUntilExit()
    return (process.terminationStatus, String(data: data, encoding: .utf8) ?? "")
}

func runBinary(_ arguments: [String]) -> String {
    runCommand(SetupEngine.binaryPath, arguments).output
        .trimmingCharacters(in: .whitespacesAndNewlines)
}

func shellQuote(_ value: String) -> String {
    "'" + value.replacingOccurrences(of: "'", with: "'\\''") + "'"
}

func appleScriptQuote(_ value: String) -> String {
    "\"" + value.replacingOccurrences(of: "\\", with: "\\\\")
        .replacingOccurrences(of: "\"", with: "\\\"") + "\""
}

/// NSPushButton is spelled NSButton everywhere else; this alias keeps the
/// layout code above reading as a list of controls.
typealias NSPushButton = NSButton
