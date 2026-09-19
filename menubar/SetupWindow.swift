// SPDX-License-Identifier: GPL-3.0-or-later
// First-run setup, as a window rather than a terminal session.
//
// It does not reimplement the deployment: it drives the mymicrotunnel
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
    // A picker, not a field: a mistyped region is only discovered after
    // authentication, as a confusing credentials error, and the list is worth
    // seeing anyway — the region decides latency and price. The built-in list
    // is replaced by what the account can actually reach as soon as there are
    // credentials to ask with, because a region that has to be opted into is
    // not deployable until it has been.
    private let regionPicker = NSPopUpButton(frame: .zero, pullsDown: false)
    // Which deployment on this Mac is being set up. The picker lists the ones
    // that exist and offers a new one; the name field is only for the latter,
    // because renaming an installed profile from here would leave its tunnel,
    // its key and its sudoers line under the old name.
    private let vpnProfilePicker = NSPopUpButton(frame: .zero, pullsDown: false)
    private let vpnProfileField = NSTextField()
    private let stackField = NSTextField()
    private let domainField = NSTextField()
    private let portField = NSTextField()
    private let tcpPortsField = NSTextField()
    private let healthPathField = NSTextField()
    private let vpnCidrField = NSTextField()
    private let idleTimeoutField = NSTextField()
    private let alarmEmailField = NSTextField()
    private let superviseCheckbox = NSButton(
        checkboxWithTitle: "Reconnect this profile at login and after a reboot",
        target: nil, action: nil)

    private let logView = NSTextView()
    private let logScroll = NSScrollView()
    private let progress = NSProgressIndicator()
    private let installButton = NSPushButton(title: "Deploy CloudFormation", target: nil, action: nil)

    /// Held so the log can be folded away to nothing rather than merely
    /// hidden: an NSView that is hidden still owns its constraints, and the
    /// window would keep a 160-point hole where the log used to be.
    private var logHeight: NSLayoutConstraint?
    private let statusLabel = NSTextField(labelWithString: "")

    private var isRunning = false
    private let settingsPath = NSTemporaryDirectory() + "microtunnel-setup.json"

    /// Where a person with no AWS identity for this starts. The console link
    /// creates the two IAM users this product uses — the one that installs,
    /// and the narrower one the app runs as afterwards — in whichever account
    /// they are signed into.
    private static let deployIdentityURL = URL(string:
        "https://console.aws.amazon.com/cloudformation/home?region=us-east-1#/stacks/create/review"
        + "?templateURL=https%3A%2F%2Fmymicrotunnel-site-985658740042.s3.amazonaws.com%2Flaunch%2Fdeploy-role.yaml"
        + "&stackName=mymicrotunnel-deploy-role")!

    private static let guideURL = URL(string: "https://mymicrotunnel.maragato.ca/#install")!

    /// Title of the picker entry that means "not one of the installed ones".
    private static let newProfileTitle = "New VPN Profile…"

    /// Entry that leaves the region unset, so the engine takes whatever the
    /// AWS profile already names — which is right far more often than any
    /// guess this window could make.
    private static let profileRegionTitle = "From the AWS profile"

    /// Shown before there are credentials to ask the account with. Commercial
    /// regions only: GovCloud and the China partitions need their own
    /// credentials and would not work if picked here by accident.
    private static let knownRegions = [
        "us-east-1", "us-east-2", "us-west-1", "us-west-2",
        "ca-central-1", "ca-west-1", "sa-east-1",
        "eu-west-1", "eu-west-2", "eu-west-3", "eu-central-1", "eu-central-2",
        "eu-north-1", "eu-south-1", "eu-south-2",
        "af-south-1", "il-central-1", "me-central-1", "me-south-1",
        "ap-south-1", "ap-south-2", "ap-southeast-1", "ap-southeast-2",
        "ap-southeast-3", "ap-southeast-4", "ap-northeast-1", "ap-northeast-2",
        "ap-northeast-3", "ap-east-1",
    ]

    convenience init() {
        // Resizable, and never taller than the screen it opens on.
        //
        // The form grew — a VPN profile, the exposed ports, the tunnel subnet,
        // an idle timeout — and a fixed 620-point window on a laptop display
        // put the Install button below the bottom edge, where it could not be
        // reached or even seen. A window that cannot be resized has no way out
        // of that. The form scrolls now and the button is pinned outside the
        // scrolling area, so no amount of further growth can hide it again.
        let visible = NSScreen.main?.visibleFrame.height ?? 900
        let height = min(660, max(420, visible - 80))
        let window = NSWindow(
            contentRect: NSRect(x: 0, y: 0, width: 660, height: height),
            styleMask: [.titled, .closable, .miniaturizable, .resizable],
            backing: .buffered,
            defer: false
        )
        window.minSize = NSSize(width: 620, height: 420)
        window.title = "MyMicroTunnel Setup"
        // The engine's version, not the app's: the engine is what talks to AWS,
        // and in a source build the two can differ.
        window.subtitle = runBinary(["version"])
        window.center()
        self.init(window: window)
        buildLayout()
        loadProfiles()
        loadVpnProfiles(select: nil)
        refreshRegions()
    }

    /// Opens the window ready to create another deployment rather than to
    /// re-run an existing one. The menu bar's "New VPN Profile…" lands here.
    func startNewProfile() {
        loadVpnProfiles(select: Self.newProfileTitle)
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
        vpnProfilePicker.target = self
        vpnProfilePicker.action = #selector(vpnProfileChanged)
        regionPicker.target = self
        regionPicker.action = #selector(regionChanged)
        regionPicker.addItem(withTitle: Self.profileRegionTitle)
        regionPicker.menu?.addItem(.separator())
        regionPicker.addItems(withTitles: Self.knownRegions)
        vpnProfileField.stringValue = defaults.vpnProfile
        vpnProfileField.placeholderString = "a name for this deployment, e.g. lab"
        stackField.placeholderString = "mymicrotunnel-<account-id>-<region>"
        domainField.placeholderString = "updates.example.com"
        portField.stringValue = defaults.servicePort
        portField.placeholderString = "3000, or 3000:8443 to publish it elsewhere"
        tcpPortsField.placeholderString = "optional, up to 10: 5432, 3000:8080"
        healthPathField.stringValue = defaults.healthCheckPath
        vpnCidrField.stringValue = defaults.vpnCidr
        idleTimeoutField.stringValue = defaults.idleTimeout
        alarmEmailField.placeholderString = "optional"

        // Shown only when nothing is deployed. Somebody re-running setup for
        // an existing profile knows all of this, and a wall of instructions
        // above the form they came to edit is in the way.
        if Tunnel.installed().isEmpty {
            form.addArrangedSubview(sectionLabel("First time here"))
            form.addArrangedSubview(firstRunNote())
            form.addArrangedSubview(firstRunButtons())
            form.addArrangedSubview(spacer())
        }

        form.addArrangedSubview(sectionLabel("AWS credentials"))
        form.addArrangedSubview(credentialMode)
        form.addArrangedSubview(labelled("Profile", profileField))
        form.addArrangedSubview(labelled("Access key id", accessKeyField))
        form.addArrangedSubview(labelled("Secret access key", secretKeyField))
        form.addArrangedSubview(labelled("Region", regionPicker))

        form.addArrangedSubview(spacer())
        form.addArrangedSubview(sectionLabel("Deployment"))
        form.addArrangedSubview(labelled("VPN profile", vpnProfilePicker))
        form.addArrangedSubview(labelled("Name", vpnProfileField))
        form.addArrangedSubview(labelled("Stack name", stackField))
        form.addArrangedSubview(labelled("Public hostname", domainField))
        form.addArrangedSubview(labelled("Service port", portField))
        form.addArrangedSubview(labelled("Also publish", tcpPortsField))
        form.addArrangedSubview(labelled("", portHint()))
        form.addArrangedSubview(labelled("Health check path", healthPathField))
        form.addArrangedSubview(labelled("Tunnel subnet", vpnCidrField))
        form.addArrangedSubview(labelled("Idle timeout (min)", idleTimeoutField))
        form.addArrangedSubview(labelled("Notify on failure", alarmEmailField))
        form.addArrangedSubview(labelled("", superviseCheckbox))

        let costNote = NSTextField(wrappingLabelWithString:
            "Deploys AWS resources into your own account that cost roughly USD 26/month. "
            + "The VPC, subnets and Route53 zone are found automatically. You will be "
            + "asked to authorise one privileged step, which writes the tunnel "
            + "configuration, the sudoers rule and — if the box above is ticked — the "
            + "supervisor.\n\n"
            + "A second VPN profile is a second deployment on this Mac: give it its own "
            + "name and its own tunnel subnet, and it gets its own interface, its own "
            + "stack and its own switch in the menu. An idle timeout above 0 switches the "
            + "gateway off after that many minutes with no traffic, and the app wakes it "
            + "again when you connect — cheaper, at the cost of about two minutes on the "
            + "first connection of the day.")
        costNote.font = .systemFont(ofSize: 11)
        costNote.textColor = .secondaryLabelColor

        // Empty until there is something to say. A console taking up a third
        // of the window before anything has run reads as an error region, and
        // people asked what was wrong with it.
        logScroll.isHidden = true
        logView.isEditable = false
        logView.font = .monospacedSystemFont(ofSize: 11, weight: .regular)
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

        // Only the questions scroll. The log and the Install button are
        // pinned to the bottom of the window, so the button is reachable on
        // any screen and the log is visible while the install runs — which is
        // the whole point of showing it.
        let questions = NSStackView(views: [form, costNote])
        questions.orientation = .vertical
        questions.alignment = .leading
        questions.spacing = 14
        questions.edgeInsets = NSEdgeInsets(top: 20, left: 20, bottom: 10, right: 20)
        questions.translatesAutoresizingMaskIntoConstraints = false

        let formScroll = NSScrollView()
        formScroll.documentView = questions
        formScroll.hasVerticalScroller = true
        formScroll.drawsBackground = false
        formScroll.borderType = .noBorder
        formScroll.translatesAutoresizingMaskIntoConstraints = false

        contentView.addSubview(formScroll)
        contentView.addSubview(logScroll)
        contentView.addSubview(footer)

        let clip = formScroll.contentView
        NSLayoutConstraint.activate([
            // The document is as wide as the clip view, so the form lays out
            // horizontally and scrolls only vertically.
            questions.topAnchor.constraint(equalTo: clip.topAnchor),
            questions.leadingAnchor.constraint(equalTo: clip.leadingAnchor),
            questions.trailingAnchor.constraint(equalTo: clip.trailingAnchor),

            formScroll.topAnchor.constraint(equalTo: contentView.topAnchor),
            formScroll.leadingAnchor.constraint(equalTo: contentView.leadingAnchor),
            formScroll.trailingAnchor.constraint(equalTo: contentView.trailingAnchor),
            formScroll.bottomAnchor.constraint(equalTo: logScroll.topAnchor, constant: -12),

            logScroll.leadingAnchor.constraint(equalTo: contentView.leadingAnchor, constant: 20),
            logScroll.trailingAnchor.constraint(equalTo: contentView.trailingAnchor, constant: -20),
            logHeightConstraint(),
            logScroll.bottomAnchor.constraint(equalTo: footer.topAnchor, constant: -12),

            footer.leadingAnchor.constraint(equalTo: contentView.leadingAnchor, constant: 20),
            footer.trailingAnchor.constraint(equalTo: contentView.trailingAnchor, constant: -20),
            footer.bottomAnchor.constraint(equalTo: contentView.bottomAnchor, constant: -20),
        ])

        // The questions must not be squeezed to make room; the scroll view is
        // what gives when the window is short.
        formScroll.setContentCompressionResistancePriority(.defaultLow, for: .vertical)

        credentialModeChanged()
    }

    private func firstRunNote() -> NSTextField {
        let note = NSTextField(wrappingLabelWithString: """
            1.  Create the AWS identity this installs with. The button below opens             CloudFormation in your own account and creates two IAM users: one that             installs, and a narrower one the app runs as afterwards. Then open that             user in IAM, create an access key, and paste the two values below — or             pick an AWS profile you already have.

            2.  You need a public Route53 hosted zone for the domain you will serve             from: example.com if the hostname will be updates.example.com. This never             creates or deletes a zone. The hostname needs three labels, and it is             repointed rather than refused if it already exists.

            3.  Fill in the form. Region is a list, and the stack name fills itself in             from your account and region once both are known.

            4.  Press Install. macOS asks for your password once, for the tunnel             configuration, the private key and the narrow sudoers rule that lets the             menu bar switch the tunnel without asking again.

            5.  It finishes by proving the path: tunnel up, gateway answers, load             balancer healthy, hostname returns 200. If a step fails it says which.

            The credentials you type are used once, to mint the app's own key, which             is kept in your login Keychain. Roughly USD 26/month of AWS, in your             account, and the switch in the menu bar decides whether the world can             reach you.
            """)
        note.font = .systemFont(ofSize: 11)
        note.textColor = .secondaryLabelColor
        note.translatesAutoresizingMaskIntoConstraints = false
        note.widthAnchor.constraint(equalToConstant: 570).isActive = true
        return note
    }

    private func firstRunButtons() -> NSStackView {
        let create = NSPushButton(title: "Create the deploy identity", target: self,
                                  action: #selector(openDeployIdentity))
        let guide = NSPushButton(title: "Open the guide", target: self,
                                 action: #selector(openGuide))

        let row = NSStackView(views: [create, guide])
        row.orientation = .horizontal
        row.spacing = 10
        return row
    }

    @objc private func openDeployIdentity() {
        NSWorkspace.shared.open(Self.deployIdentityURL)
    }

    @objc private func openGuide() {
        NSWorkspace.shared.open(Self.guideURL)
    }

    /// Says what a port field takes, next to the port fields.
    ///
    /// The placeholders showed the syntax and not the meaning, and "3000:8080"
    /// is ambiguous until somebody tells you which end is which — the two
    /// readings differ by whether your service moves or the hostname does.
    private func portHint() -> NSTextField {
        let hint = NSTextField(wrappingLabelWithString: """
            Ports are written local:published — the port here first, the port the             hostname answers on second. 5432 publishes 5432 under its own name.             3000:8080 reaches port 3000 on this Mac and answers as 8080 on the hostname.             The service above follows the same rule: 3000 is HTTPS on 443, and 3000:8443             publishes it on 8443 instead.
            """)
        hint.font = .systemFont(ofSize: 11)
        hint.textColor = .secondaryLabelColor
        return hint
    }

    private func logHeightConstraint() -> NSLayoutConstraint {
        let constraint = logScroll.heightAnchor.constraint(equalToConstant: 0)
        logHeight = constraint
        return constraint
    }

    /// Unfolds the log, once, when the deploy starts.
    private func revealLog() {
        guard logScroll.isHidden else { return }
        logScroll.isHidden = false
        logHeight?.constant = 160
        window?.contentView?.layoutSubtreeIfNeeded()
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

    /// Trimmed text, or a fallback when the field was left empty. Every one of
    /// these used to be an inline trimmingCharacters call, and two of them
    /// disagreed about whether an all-spaces field counted as empty.
    /// The region the user picked, or "" when they left it to the profile.
    private var selectedRegion: String {
        let title = regionPicker.titleOfSelectedItem ?? Self.profileRegionTitle
        return title == Self.profileRegionTitle ? "" : title
    }

    private func selectRegion(_ region: String) {
        if region.isEmpty || regionPicker.item(withTitle: region) == nil {
            regionPicker.selectItem(withTitle: Self.profileRegionTitle)
            return
        }
        regionPicker.selectItem(withTitle: region)
    }

    private func trimmed(_ field: NSTextField, or fallback: String) -> String {
        let value = field.stringValue.trimmingCharacters(in: .whitespaces)
        return value.isEmpty ? fallback : value
    }

    // MARK: - VPN profiles

    /// Fills the picker and selects something sensible: the profile asked for,
    /// otherwise the first installed one, otherwise a new one — which is what
    /// a first run is.
    private func loadVpnProfiles(select wanted: String?) {
        let installed = Tunnel.installed().map(\.profileName)

        vpnProfilePicker.removeAllItems()
        vpnProfilePicker.addItems(withTitles: installed)
        if !installed.isEmpty {
            vpnProfilePicker.menu?.addItem(.separator())
        }
        vpnProfilePicker.addItem(withTitle: Self.newProfileTitle)

        if let wanted, installed.contains(wanted) {
            vpnProfilePicker.selectItem(withTitle: wanted)
        } else if wanted != nil || installed.isEmpty {
            vpnProfilePicker.selectItem(withTitle: Self.newProfileTitle)
        } else {
            vpnProfilePicker.selectItem(at: 0)
        }
        vpnProfileChanged()
    }

    @objc private func vpnProfileChanged() {
        let selected = vpnProfilePicker.titleOfSelectedItem ?? Self.newProfileTitle
        let isNew = selected == Self.newProfileTitle

        vpnProfileField.isEnabled = isNew
        if isNew {
            // Suggested, not imposed: a second deployment usually wants its
            // own tunnel subnet as well, and defaulting both together is what
            // stops the install being refused for overlapping the first.
            let installed = Tunnel.installed()
            if vpnProfileField.stringValue.isEmpty || !installed.isEmpty {
                vpnProfileField.stringValue = suggestedProfileName(installed.map(\.profileName))
            }
            if installed.count > 0 {
                vpnCidrField.stringValue = suggestedVpnCidr(installed.count)
                domainField.stringValue = ""
            }
            // A new deployment is named for the account and the region, and
            // the account is only knowable by asking AWS. Shown rather than
            // left blank so the name is visible before the deploy, and
            // editable so it can be overridden.
            stackField.stringValue = ""
            showDerivedStackName()
            return
        }

        vpnProfileField.stringValue = selected
        loadSettings(of: selected)
    }

    private func suggestedProfileName(_ taken: [String]) -> String {
        if !taken.contains("default") { return "default" }
        for index in 2... {
            let candidate = "profile\(index)"
            if !taken.contains(candidate) { return candidate }
        }
        return "profile"
    }

    /// 10.100, 10.110, 10.120 … one /24 per profile, far enough apart that a
    /// deployment can grow into it and still not meet the next one.
    private func suggestedVpnCidr(_ existing: Int) -> String {
        "10.\(100 + existing * 10).0.0/24"
    }

    /// Reads a profile's recorded deployment back into the form, so re-running
    /// setup for an installed profile does not mean retyping every answer.
    private func loadSettings(of profileName: String) {
        let path = Tunnel.profilesDirectory
            .appendingPathComponent(profileName)
            .appendingPathComponent("settings.json")
        guard let data = try? Data(contentsOf: path),
              let stored = try? JSONSerialization.jsonObject(with: data) as? [String: Any]
        else {
            return
        }

        func text(_ key: String) -> String { stored[key] as? String ?? "" }
        selectRegion(text("region"))
        stackField.stringValue = text("stackName")
        domainField.stringValue = text("domainName")
        // Shown the way it was typed: a bare number when the two halves
        // agree, local:published when they do not.
        let local = text("servicePort")
        let published = text("publishedPort")
        portField.stringValue = (published.isEmpty || published == "443" || published == local)
            ? local : "\(local):\(published)"
        healthPathField.stringValue = text("healthCheckPath")
        vpnCidrField.stringValue = text("vpnCidr")
        alarmEmailField.stringValue = text("alarmEmail")
        tcpPortsField.stringValue = (stored["tcpPorts"] as? [String] ?? []).joined(separator: ", ")
        idleTimeoutField.stringValue = String(stored["idleTimeoutMinutes"] as? Int ?? 0)
        superviseCheckbox.state = (stored["supervise"] as? Bool ?? false) ? .on : .off

        if let awsProfile = stored["profile"] as? String, !awsProfile.isEmpty {
            credentialMode.selectItem(at: 0)
            profileField.selectItem(withTitle: awsProfile)
            credentialModeChanged()
        }
    }

    /// Replaces the built-in list with the regions this account can actually
    /// reach. Keeps the current selection if it survives the swap.
    private func refreshRegions() {
        let awsProfile = credentialMode.indexOfSelectedItem == 0
            ? (profileField.titleOfSelectedItem ?? "default")
            : "default"

        DispatchQueue.global(qos: .utility).async {
            let listed = runBinary(["regions", "--profile", awsProfile])
                .split(separator: "\n")
                .map(String.init)
                .filter { !$0.isEmpty }
            guard listed.count > 1 else { return }

            DispatchQueue.main.async {
                let chosen = self.selectedRegion
                self.regionPicker.removeAllItems()
                self.regionPicker.addItem(withTitle: Self.profileRegionTitle)
                self.regionPicker.menu?.addItem(.separator())
                self.regionPicker.addItems(withTitles: listed)
                self.selectRegion(chosen)
            }
        }
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

    @objc private func regionChanged() {
        if vpnProfilePicker.titleOfSelectedItem == Self.newProfileTitle {
            showDerivedStackName()
        }
    }

    /// Asks the engine what a deploy would call this stack, and fills the
    /// field with it.
    ///
    /// Off the main thread: it authenticates and calls STS, which on a cold
    /// SSO session is not instant, and a setup window that freezes while the
    /// user picks a region is worse than one that fills a field a moment
    /// late. Whatever has been typed in the meantime wins.
    private func showDerivedStackName() {
        let awsProfile = credentialMode.indexOfSelectedItem == 0
            ? (profileField.titleOfSelectedItem ?? "default")
            : "default"
        let region = selectedRegion

        DispatchQueue.global(qos: .userInitiated).async {
            var arguments = ["default-stack", "--profile", awsProfile]
            if !region.isEmpty {
                arguments += ["--region", region]
            }
            let derived = runBinary(arguments).trimmingCharacters(in: .whitespacesAndNewlines)

            DispatchQueue.main.async {
                guard !derived.isEmpty, self.stackField.stringValue.isEmpty else { return }
                self.stackField.stringValue = derived
            }
        }
    }

    @objc private func credentialModeChanged() {
        let usingProfile = credentialMode.indexOfSelectedItem == 0
        profileField.isHidden = !usingProfile
        accessKeyField.isHidden = usingProfile
        secretKeyField.isHidden = usingProfile

        if vpnProfilePicker.numberOfItems > 0,
           vpnProfilePicker.titleOfSelectedItem == Self.newProfileTitle {
            showDerivedStackName()
        }
        refreshRegions()
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
        revealLog()

        var arguments = [
            "install", "--json", "--non-interactive",
            "--settings", settingsPath,
            "--vpn-profile", trimmed(vpnProfileField, or: "default"),
            "--domain", domain,
            "--port", portField.stringValue,
            "--health-path", healthPathField.stringValue,
            "--idle-timeout", trimmed(idleTimeoutField, or: "0"),
        ]

        // Left out when empty so the engine derives it from the account and
        // the region, which is what makes a second deployment in one account
        // not collide with the first.
        if !trimmed(stackField, or: "").isEmpty {
            arguments += ["--stack", stackField.stringValue]
        }
        if !trimmed(tcpPortsField, or: "").isEmpty {
            arguments += ["--tcp-ports", tcpPortsField.stringValue]
        }
        if !trimmed(vpnCidrField, or: "").isEmpty {
            arguments += ["--vpn-cidr", vpnCidrField.stringValue]
        }

        // Left out entirely when empty, so the engine can fall back to the
        // region the profile already names rather than to a guess.
        if !selectedRegion.isEmpty {
            arguments += ["--region", selectedRegion]
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
            "--vpn-profile", trimmed(vpnProfileField, or: "default"),
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
    ///
    /// The concurrency here is fussier than it looks. A pipe's readability
    /// handler runs on an arbitrary queue, and Process calls its termination
    /// handler on another one; both are `@Sendable` closures. Anything they
    /// touch has to be safe to touch from there, which rules out the obvious
    /// spelling — a captured `var` for the partial line, and a plain closure
    /// for the completion. Swift 5 calls those warnings; Swift 6 calls them
    /// errors.
    ///
    /// `completion` is spelled `@Sendable @MainActor` rather than just
    /// `@MainActor`. Swift 6 infers that a main-actor-isolated closure is safe
    /// to hand across domains; Swift 5 with complete checking does not, and
    /// says so. Writing both keeps every mode quiet about the same code.
    private func stream(_ arguments: [String], completion: @escaping @Sendable @MainActor (Bool) -> Void) {
        let process = Process()
        process.executableURL = URL(fileURLWithPath: SetupEngine.binaryPath)
        process.arguments = arguments

        let pipe = Pipe()
        process.standardOutput = pipe
        process.standardError = pipe

        let buffer = LineBuffer()
        pipe.fileHandleForReading.readabilityHandler = { handle in
            let chunk = handle.availableData
            if chunk.isEmpty {
                // EOF. Cleared here, from the handle the handler was given,
                // rather than from the termination handler — which would mean
                // carrying the pipe into a second concurrency domain to do it.
                handle.readabilityHandler = nil
                return
            }
            guard let text = String(data: chunk, encoding: .utf8) else { return }
            Task { @MainActor in
                for line in buffer.take(text) {
                    self.appendEvent(line)
                }
            }
        }

        process.terminationHandler = { finished in
            // Read out here and passed on as a Bool: Process is not Sendable,
            // so it must not cross into the task below.
            let succeeded = finished.terminationStatus == 0
            Task { @MainActor in
                completion(succeeded)
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

/// Accumulates the partial line left over between reads.
///
/// A reference type on the main actor rather than a captured `var`, because the
/// pipe's handler runs on an arbitrary queue: a captured var mutated from there
/// is a data race, and one that happens to work until the day a deploy is
/// chatty enough to split a line across two reads under load.
@MainActor
private final class LineBuffer {
    private var pending = ""

    /// Adds a chunk and returns whatever complete lines that produced, keeping
    /// any trailing partial line for next time.
    func take(_ text: String) -> [String] {
        pending += text

        var lines: [String] = []
        while let newline = pending.firstIndex(of: "\n") {
            lines.append(String(pending[pending.startIndex..<newline]))
            pending = String(pending[pending.index(after: newline)...])
        }
        return lines
    }
}

/// Where the engine binary lives. Deliberately outside the window controller so
/// it is reachable from the background work too, not only from the main actor.
enum SetupEngine {
    /// Prefers the copy inside the bundle, so the app is self-contained, and
    /// falls back to the one the package puts on the path.
    static var binaryPath: String {
        if let bundled = Bundle.main.url(forResource: "mymicrotunnel", withExtension: nil),
           FileManager.default.isExecutableFile(atPath: bundled.path) {
            return bundled.path
        }
        return "/usr/local/bin/mymicrotunnel"
    }
}

// MARK: - Helpers

/// Only the answers that are the same for everyone. A region or a hostname
/// default would be one particular deployment's, and accepting it would claim
/// a name in somebody else's zone.
struct SetupDefaults {
    let vpnProfile = "default"
    let servicePort = "3000"  // published on 443 unless written local:published
    let healthCheckPath = "/hc"
    let vpnCidr = "10.100.0.0/24"
    /// 0 keeps the gateway running. It is the default because switching a
    /// deployment off is a decision about money and latency that belongs to
    /// whoever is paying, not to this installer.
    let idleTimeout = "0"
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
