// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/version"
)

// Diagnose collects, in one place, everything somebody would otherwise have to
// ask for over three rounds of email.
//
// It is written to be pasted. Every line is safe to send to a stranger: the
// AWS account is reduced to its last four digits, no credential is read, and
// the only keys shown are public ones. The private key's *existence* is
// reported, never its contents.
//
// It never fails. A section that cannot be gathered says so and the next one
// runs, because the machine that most needs diagnosing is the one where half of
// this is broken.
type Report struct {
	lines []string
}

func (r *Report) section(title string) {
	r.lines = append(r.lines, "", "── "+title+" "+strings.Repeat("─", max(0, 58-len(title))))
}

func (r *Report) addf(format string, args ...any) {
	r.lines = append(r.lines, fmt.Sprintf(format, args...))
}

func (r *Report) field(name string, format string, args ...any) {
	r.addf("  %-22s %s", name, fmt.Sprintf(format, args...))
}

func (r *Report) String() string { return strings.Join(r.lines, "\n") + "\n" }

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// DiagnoseOptions says how much to gather. The AWS half needs credentials and
// is skipped without them rather than refused.
type DiagnoseOptions struct {
	// ProfileName is which VPN profile to report on. Every profile is listed
	// whatever this says; the detailed sections describe this one, because a
	// report that interleaved three tunnels would be unreadable.
	ProfileName string

	StackName string
	Profile   string
	Region    string
	SkipAWS   bool
}

func Diagnose(ctx context.Context, options DiagnoseOptions) string {
	report := &Report{}
	report.addf("microtunnel vpn diagnostics — %s", time.Now().Format(time.RFC3339))
	report.addf("paste this whole thing to mymicrotunnel@maragato.ca")

	if options.ProfileName == "" {
		options.ProfileName = DefaultProfileName
	}
	config := installedTunnel(options.ProfileName)
	settings, _ := LoadProfileSettings(options.ProfileName)
	if options.StackName == "" {
		options.StackName = settings.StackName
	}

	diagnoseMachine(report)
	diagnoseProfiles(report, options.ProfileName)
	diagnoseInstall(report, config, settings)
	diagnoseTunnel(report, config)
	diagnoseService(report, config)
	if !options.SkipAWS {
		diagnoseAWS(ctx, report, options)
	}
	diagnoseLocalNetworks(report, config)
	diagnoseSupervisor(report, options.ProfileName)

	report.section("end")
	return report.String()
}

func diagnoseMachine(report *Report) {
	report.section("this machine")
	report.field("tool version", "%s", version.String())

	if product := sys.Run("/usr/bin/sw_vers", "-productVersion"); product.OK() {
		report.field("macOS", "%s", product.Output)
	}
	if arch := sys.Run("/usr/bin/uname", "-m"); arch.OK() {
		report.field("architecture", "%s", arch.Output)
	}
	if name, err := os.Hostname(); err == nil {
		report.field("hostname", "%s", name)
	}
	report.field("running as", "uid %d", os.Geteuid())
	if os.Geteuid() != 0 {
		report.field("", "%s", "(the handshake and the tunnel's own view need sudo)")
	}
}

// diagnoseProfiles is the first question to ask of a machine that holds more
// than one deployment: which of them are there at all, and which one is this
// report about.
func diagnoseProfiles(report *Report, current string) {
	report.section("VPN profiles")

	names := ListProfiles()
	if len(names) == 0 {
		report.field("profiles", "none installed")
		return
	}
	for _, name := range names {
		config := installedTunnel(name)
		marker := " "
		if name == current {
			marker = "*"
		}
		report.field(marker+" "+name, "%s  %s  %s",
			config.InterfaceName, orNone(config.ClientAddress), orNone(config.StackName))
	}
	report.field("", "%s", "(* is the profile the rest of this report describes)")
}

func diagnoseInstall(report *Report, config appConfig, settings Settings) {
	report.section("what is installed")

	for _, path := range []string{
		InstalledAppPath,
		CommandPath,
		HelperPath,
		EmbeddedEngineDir + "/wireguard-go",
	} {
		info, err := os.Stat(path)
		if err != nil {
			report.field(shortName(path), "MISSING")
			continue
		}
		report.field(shortName(path), "%s  %s  %s",
			info.Mode().Perm(), info.ModTime().Format("2006-01-02 15:04"), owner(path))
	}

	if bundled := appVersionOf(InstalledAppPath); bundled != "" {
		report.field("app version", "%s", bundled)
	}
	// A helper that disagrees with the app is the shape an interrupted update
	// leaves behind, and it is invisible from the menu bar.
	if helper := sys.Run(HelperPath, "version"); helper.OK() {
		report.field("helper version", "%s", helper.Output)
	}

	report.section("configuration")
	report.field("profile", "%s", config.ProfileName)
	report.field("config file", "%s", ProfileConfigPath(config.ProfileName))
	report.field("interface", "%s", config.InterfaceName)
	report.field("this machine", "%s", config.ClientAddress)
	report.field("service port", "%s", orNone(config.ServicePort))
	report.field("health check", "%s", orNone(config.HealthCheckURL))
	report.field("helper", "%s", config.HelperPath)
	report.field("supervised", "%t", config.Supervised)
	if len(config.TcpPorts) > 0 {
		report.field("exposed TCP ports", "%s", strings.Join(config.TcpPorts, ", "))
	}
	if config.IdleTimeoutMinutes > 0 {
		report.field("idle timeout", "%d minutes, then the gateway is switched off", config.IdleTimeoutMinutes)
		report.field("wake role", "%s", orNone(settings.WakeRoleARN))
	}

	// Contents never shown. Its presence and mode are the whole question —
	// and "cannot look" is a third answer, distinct from "not there".
	keyPath := Settings{InterfaceName: config.InterfaceName}.ClientKeyPath()
	info, err := os.Stat(keyPath)
	switch {
	case err == nil:
		report.field("private key", "present, mode %s, %s", info.Mode().Perm(), owner(keyPath))
		if info.Mode().Perm() != 0o600 {
			report.field("", "%s", "WARNING: expected mode 600")
		}
	case os.IsPermission(err):
		report.field("private key", "cannot check without sudo (/etc/wireguard is root-only)")
	default:
		report.field("private key", "MISSING at %s", keyPath)
	}

	if content, err := os.ReadFile(PublicKeyPath(config.ProfileName)); err == nil {
		report.field("public key", "%s", strings.TrimSpace(string(content)))
	}

	report.field("sudoers rule", "%s", sudoersState())
}

func diagnoseTunnel(report *Report, config appConfig) {
	report.section("tunnel")

	device := tunnel.Device(config.InterfaceName)
	present := tunnel.AddressPresent(config.ClientAddress)

	switch {
	case device != "":
		report.field("state", "up on %s", device)
	case present:
		report.field("state", "up (%s is assigned); run with sudo for detail", config.ClientAddress)
	default:
		report.field("state", "down")
	}

	if device == "" {
		return
	}

	status, err := tunnel.Report(config.InterfaceName)
	if err != nil {
		report.field("handshake", "could not read: %v", err)
		return
	}
	if len(status.Peers) == 0 {
		report.field("peers", "none configured — the tunnel exists but has nobody to talk to")
		return
	}
	for _, peer := range status.Peers {
		when := "never"
		if !peer.LastHandshake.IsZero() {
			when = time.Since(peer.LastHandshake).Round(time.Second).String() + " ago"
		}
		report.field("peer", "%s", peer.PublicKey)
		report.field("  endpoint", "%s", orNone(peer.Endpoint))
		report.field("  handshake", "%s", when)
		report.field("  traffic", "rx %d  tx %d", peer.ReceivedBytes, peer.SentBytes)
	}
	// An interface can outlive the far end by hours. This is the distinction
	// that matters and the one a screenshot of the menu bar cannot show.
	report.field("carrying traffic", "%t", status.Connected(time.Now()))
}

func diagnoseService(report *Report, config appConfig) {
	report.section("the service being published")

	if config.ServicePort == "" {
		report.field("skipped", "no port recorded")
		return
	}

	client := &http.Client{Timeout: 5 * time.Second}

	// Any HTTP response means reachable. A 404 is the service answering, which
	// is the question being asked here; whether the path exists is a different
	// one and not this function's business.
	reachable := func(url string) (string, bool) {
		response, err := client.Get(url)
		if err != nil {
			return "unreachable (" + firstLine(err.Error()) + ")", false
		}
		defer response.Body.Close()
		return response.Status, true
	}

	loopbackText, onLoopback := reachable("http://127.0.0.1:" + config.ServicePort + "/")
	tunnelText, onTunnel := reachable("http://" + config.ClientAddress + ":" + config.ServicePort + "/")
	report.field("on loopback", "%s", loopbackText)
	report.field("on the tunnel", "%s", tunnelText)

	// Said only when it is actually true. An unconditional explanation reads as
	// a finding, and sends whoever is diagnosing after a problem that is not
	// there.
	switch {
	case !tunnel.AddressPresent(config.ClientAddress):
		report.field("note", "the tunnel is down, so the second probe proves nothing")
	case onLoopback && !onTunnel:
		report.field("PROBLEM", "%s", "the service answers on 127.0.0.1 but not on "+
			config.ClientAddress+": it is bound to loopback, and the load balancer "+
			"cannot reach it. Republish it on all interfaces.")
	case !onLoopback && !onTunnel:
		report.field("PROBLEM", "the service is not answering on port %s at all", config.ServicePort)
	}

	if config.HealthCheckURL != "" {
		response, err := client.Get(config.HealthCheckURL)
		if err != nil {
			report.field("public hostname", "unreachable (%s)", firstLine(err.Error()))
			return
		}
		response.Body.Close()
		report.field("public hostname", "%s", response.Status)
	}
}

func diagnoseAWS(ctx context.Context, report *Report, options DiagnoseOptions) {
	report.section("aws")

	region := options.Region
	if region == "" {
		region = awsops.ProfileRegion(ctx, options.Profile)
	}
	report.field("profile", "%s", options.Profile)
	report.field("region", "%s", orNone(region))
	report.field("stack", "%s", options.StackName)

	client, err := awsops.LoadProfile(ctx, options.Profile, region)
	if err != nil {
		report.field("credentials", "could not load: %v", err)
		return
	}
	identity, err := client.Identity(ctx)
	if err != nil {
		report.field("credentials", "do not work: %v", firstLine(err.Error()))
		return
	}
	report.field("identity", "%s", maskAccount(identity))

	outputs, err := client.StackOutputs(ctx, options.StackName)
	if err != nil {
		report.field("stack", "could not read: %v", firstLine(err.Error()))
		return
	}
	report.field("gateway address", "%s", orNone(outputs["GatewayPublicIp"]))
	report.field("service url", "%s", orNone(outputs["ServiceUrl"]))

	settings := Settings{StackName: options.StackName}
	peers, err := client.Peers(ctx, settings.PeersParameter())
	if err != nil {
		report.field("peer registry", "could not read: %v", firstLine(err.Error()))
	} else if len(peers) == 0 {
		report.field("peer registry", "empty — no workstation is registered")
	} else {
		for _, peer := range peers {
			report.field("registered", "%-16s %-22s %s", peer.Address, orNone(peer.Label), peer.PublicKey)
		}
	}

	if targetGroup := outputs["TargetGroupArn"]; targetGroup != "" {
		config := installedTunnel(options.ProfileName)
		state, err := client.TargetHealthOf(ctx, targetGroup, config.ClientAddress)
		if err != nil {
			report.field("target health", "could not read: %v", firstLine(err.Error()))
		} else {
			report.field("target health", "%s", state)
		}
	}
}

// diagnoseLocalNetworks answers the question that produces the strangest
// support mail: the tunnel came up and the rest of the network went away. It
// happens when a claimed range is a range this machine is already on, and it
// is invisible unless the two are listed side by side.
func diagnoseLocalNetworks(report *Report, config appConfig) {
	report.section("networks this machine is on")

	local, err := tunnel.LocalNetworks(ourDevices(config.InterfaceName))
	local = withoutOurTunnels(local, config.ClientAddress)
	if err != nil {
		report.field("interfaces", "could not be listed: %v", firstLine(err.Error()))
		return
	}
	for _, network := range local {
		report.field(network.Interface, "%s", network.Net)
	}

	file, err := tunnel.Load(Settings{InterfaceName: config.InterfaceName}.TunnelConfigPath())
	if err != nil {
		report.field("tunnel routes", "the configuration could not be read: %v", firstLine(err.Error()))
		return
	}
	claimed := tunnel.ClaimedBy(file)
	report.field("tunnel would route", "%s", strings.Join(claimed, ", "))

	collisions := tunnel.Collisions(claimed, local)
	if len(collisions) == 0 {
		report.field("collisions", "none")
		return
	}
	report.field("collisions", "%s", "THIS TUNNEL WILL NOT BE RAISED:")
	for _, line := range strings.Split(tunnel.Explain(collisions), "\n") {
		report.addf("%s", line)
	}
}

func diagnoseSupervisor(report *Report, profileName string) {
	report.section("supervisor")

	if !sys.Exists(SupervisorPlistPath) {
		report.field("installed", "no")
		return
	}
	report.field("installed", "%s", SupervisorPlistPath)

	state := sys.Run("/bin/launchctl", "print", "system/"+SupervisorLabel)
	if !state.OK() {
		report.field("loaded", "no")
	} else {
		for _, line := range strings.Split(state.Output, "\n") {
			if strings.Contains(line, "state =") {
				report.field("state", "%s", strings.TrimSpace(line))
				break
			}
		}
	}

	if content, err := os.ReadFile(DesiredStatePathFor(profileName)); err == nil {
		report.field("desired state", "%s", strings.TrimSpace(string(content)))
	} else {
		report.field("desired state", "not recorded")
	}

	// The last few lines are usually the whole story: the supervisor narrates
	// every decision it makes.
	if log := sys.Run("/usr/bin/tail", "-n", "12", SupervisorLogPath); log.OK() && log.Output != "" {
		report.addf("  last log lines:")
		for _, line := range strings.Split(log.Output, "\n") {
			report.addf("    %s", line)
		}
	}
}

// --- small helpers ---------------------------------------------------------

func shortName(path string) string {
	parts := strings.Split(strings.TrimSuffix(path, "/"), "/")
	return parts[len(parts)-1]
}

func owner(path string) string {
	result := sys.Run("/usr/bin/stat", "-f%Su:%Sg", path)
	if !result.OK() {
		return "unknown"
	}
	return result.Output
}

func appVersionOf(bundle string) string {
	output, err := exec.Command("/usr/libexec/PlistBuddy",
		"-c", "Print :CFBundleShortVersionString", bundle+"/Contents/Info.plist").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(output))
}

func sudoersState() string {
	if _, err := os.Stat(SudoersPath); err != nil {
		if os.IsPermission(err) {
			return "cannot check without sudo"
		}
		return "MISSING — the menu bar cannot move the tunnel"
	}

	check := sys.Run("/usr/sbin/visudo", "-c", "-f", SudoersPath)
	if check.OK() {
		return "present, visudo accepts it"
	}
	// visudo refusing to *open* the file is a permission problem, not a
	// malformed rule. Reporting the second when it is the first sends the
	// reader looking for a syntax error that is not there.
	if strings.Contains(check.Output, "unable to open") || os.Geteuid() != 0 {
		return "present; run with sudo to validate it"
	}
	return "present but visudo rejects it: " + firstLine(check.Output)
}

// maskAccount keeps enough of the ARN to be recognisable and not enough to be
// worth anything, because these reports get pasted into email.
func maskAccount(arn string) string {
	parts := strings.Split(arn, ":")
	if len(parts) < 5 || len(parts[4]) < 4 {
		return arn
	}
	parts[4] = "…" + parts[4][len(parts[4])-4:]
	return strings.Join(parts, ":")
}

func firstLine(text string) string {
	if index := strings.Index(text, "\n"); index >= 0 {
		return text[:index]
	}
	return text
}

func orNone(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none)"
	}
	return value
}
