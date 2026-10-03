// SPDX-License-Identifier: GPL-3.0-or-later
package setup

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/IceSkatingCoach/MyMicroTunnel/internal/awsops"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/sys"
	"github.com/IceSkatingCoach/MyMicroTunnel/internal/tunnel"
)

// fakeMachine stands in for everything this package does to the computer:
// commands, root-owned writes, the tunnel itself and the machine-wide paths,
// which live under root instead of /.
type fakeMachine struct {
	root string
	euid int

	mu       sync.Mutex
	commands []string
	// replies answer a command by the longest prefix of its command line.
	replies map[string]sys.Result
	// exits answer an interactive command the same way.
	exits map[string]int
	// writeErrors refuses a root-owned write to one path.
	writeErrors map[string]error

	devices     map[string]string
	present     map[string]bool
	status      map[string]tunnel.Status
	statusErr   error
	networks    []tunnel.Network
	networksErr error
	upErr       error
	downErr     error
	raised      []tunnel.Options
	dropped     []string
	engine      string
	engineErr   error
}

// failure is what fail panics with under test, so a step that would end the
// run stops the test's call instead of the test binary.
type failure string

func stubMachine(t *testing.T) *fakeMachine {
	t.Helper()
	fake := &fakeMachine{
		root:        t.TempDir(),
		euid:        501,
		replies:     map[string]sys.Result{},
		exits:       map[string]int{},
		writeErrors: map[string]error{},
		devices:     map[string]string{},
		present:     map[string]bool{},
		status:      map[string]tunnel.Status{},
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("SUDO_USER", "")

	saved := []func(){}
	keep := func(restore func()) { saved = append(saved, restore) }
	{
		old := run
		keep(func() { run = old })
		run = fake.run
	}
	{
		old := runInteractive
		keep(func() { runInteractive = old })
		runInteractive = fake.runInteractive
	}
	{
		old := writeAsRoot
		keep(func() { writeAsRoot = old })
		writeAsRoot = func(content, destination, mode string) error {
			parsed, err := strconv.ParseUint(mode, 8, 32)
			if err != nil {
				return err
			}
			return fake.write("sudo install", content, destination, os.FileMode(parsed))
		}
	}
	{
		old := writeAsRootNonInteractive
		keep(func() { writeAsRootNonInteractive = old })
		writeAsRootNonInteractive = func(content, destination string, mode os.FileMode) error {
			return fake.write("write", content, destination, mode)
		}
	}
	{
		old := geteuid
		keep(func() { geteuid = old })
		geteuid = func() int { return fake.euid }
	}
	{
		old := sleep
		keep(func() { sleep = old })
		sleep = func(time.Duration) {}
	}
	{
		old := fail
		keep(func() { fail = old })
		fail = func(format string, args ...any) { panic(failure(fmt.Sprintf(format, args...))) }
	}
	{
		old := tunnelEngine
		keep(func() { tunnelEngine = old })
		tunnelEngine = func(string) (string, error) { return fake.engine, fake.engineErr }
	}
	{
		oldDevice, oldIsUp, oldUp, oldDown := tunnelDevice, tunnelIsUp, tunnelUp, tunnelDown
		oldReport, oldPresent, oldNetworks := tunnelReport, tunnelAddressPresent, localNetworks
		keep(func() {
			tunnelDevice, tunnelIsUp, tunnelUp, tunnelDown = oldDevice, oldIsUp, oldUp, oldDown
			tunnelReport, tunnelAddressPresent, localNetworks = oldReport, oldPresent, oldNetworks
		})
		tunnelDevice = func(name string) string {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			return fake.devices[name]
		}
		tunnelIsUp = func(name string) bool { return tunnelDevice(name) != "" }
		tunnelUp = func(options tunnel.Options) error {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.raised = append(fake.raised, options)
			return fake.upErr
		}
		tunnelDown = func(name string) error {
			fake.mu.Lock()
			defer fake.mu.Unlock()
			fake.dropped = append(fake.dropped, name)
			return fake.downErr
		}
		tunnelReport = func(name string) (tunnel.Status, error) { return fake.status[name], fake.statusErr }
		tunnelAddressPresent = func(address string) bool { return fake.present[address] }
		localNetworks = func(exclude []string) ([]tunnel.Network, error) {
			skip := map[string]bool{}
			for _, name := range exclude {
				skip[name] = true
			}
			var kept []tunnel.Network
			for _, network := range fake.networks {
				if !skip[network.Interface] {
					kept = append(kept, network)
				}
			}
			return kept, fake.networksErr
		}
	}
	{
		oldLoad, oldForget := loadAppCredentials, forgetAppCredentials
		keep(func() { loadAppCredentials, forgetAppCredentials = oldLoad, oldForget })
		loadAppCredentials = func(string) (*awsops.AppCredentials, error) { return nil, nil }
		forgetAppCredentials = func(string) error { return nil }
	}
	{
		oldRoot, oldTransport := rootDir, probeTransport
		keep(func() { rootDir, probeTransport = oldRoot, oldTransport })
		rootDir = fake.root
	}
	{
		oldOverride := profilesDirOverride
		keep(func() { profilesDirOverride = oldOverride })
		profilesDirOverride = ""
	}

	t.Cleanup(func() {
		for index := len(saved) - 1; index >= 0; index-- {
			saved[index]()
		}
	})
	return fake
}

func (fake *fakeMachine) commandLine(name string, args []string) string {
	return strings.TrimSpace(name + " " + strings.Join(args, " "))
}

func (fake *fakeMachine) run(name string, args ...string) sys.Result {
	line := fake.commandLine(name, args)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.commands = append(fake.commands, line)
	best, found := "", false
	for prefix := range fake.replies {
		if strings.HasPrefix(line, prefix) && len(prefix) >= len(best) {
			best, found = prefix, true
		}
	}
	if !found {
		return sys.Result{ExitCode: 1, Output: "no such command here"}
	}
	return fake.replies[best]
}

func (fake *fakeMachine) runInteractive(name string, args ...string) int {
	line := fake.commandLine(name, args)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.commands = append(fake.commands, line)
	best, code := "", 0
	for prefix, exit := range fake.exits {
		if strings.HasPrefix(line, prefix) && len(prefix) >= len(best) {
			best, code = prefix, exit
		}
	}
	return code
}

func (fake *fakeMachine) write(how, content, destination string, mode os.FileMode) error {
	fake.mu.Lock()
	fake.commands = append(fake.commands, fmt.Sprintf("%s %s %o", how, destination, mode))
	err := fake.writeErrors[destination]
	fake.mu.Unlock()
	if err != nil {
		return err
	}
	path := onDisk(destination)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(path, []byte(content), mode|0o200); err != nil {
		return err
	}
	return os.Chmod(path, mode)
}

// reply makes every command line starting with prefix succeed with output.
func (fake *fakeMachine) reply(prefix, output string) {
	fake.replies[prefix] = sys.Result{Output: output}
}

func (fake *fakeMachine) refuse(prefix, output string) {
	fake.replies[prefix] = sys.Result{ExitCode: 1, Output: output}
}

func (fake *fakeMachine) ran(prefix string) bool {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, line := range fake.commands {
		if strings.HasPrefix(line, prefix) {
			return true
		}
	}
	return false
}

// place writes a file at one of the machine-wide paths.
func (fake *fakeMachine) place(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	full := onDisk(path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(full, mode); err != nil {
		t.Fatal(err)
	}
}

func (fake *fakeMachine) read(t *testing.T, path string) string {
	t.Helper()
	content, err := os.ReadFile(onDisk(path))
	if err != nil {
		t.Fatalf("%s was not written: %v", path, err)
	}
	return string(content)
}

// failureOf runs a step and returns what it failed with, or "" when it ran to
// the end.
func failureOf(step func()) (message string) {
	defer func() {
		if recovered := recover(); recovered != nil {
			reason, ok := recovered.(failure)
			if !ok {
				panic(recovered)
			}
			message = string(reason)
		}
	}()
	step()
	return ""
}

// captured is everything a step printed.
func captured(t *testing.T, step func()) string {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	original := os.Stdout
	os.Stdout = writer
	done := make(chan string, 1)
	go func() {
		content, _ := io.ReadAll(reader)
		done <- string(content)
	}()
	func() {
		defer func() {
			os.Stdout = original
			writer.Close()
		}()
		step()
	}()
	return <-done
}

func network(t *testing.T, device, cidr string) tunnel.Network {
	t.Helper()
	_, parsed, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatal(err)
	}
	return tunnel.Network{Interface: device, Net: parsed}
}

// probesAnswer serves the HTTP checks of the published service from a table of
// host to status, and refuses any host not in it.
type probesAnswer map[string]int

func (answers probesAnswer) RoundTrip(request *http.Request) (*http.Response, error) {
	status, found := answers[request.URL.Host]
	if !found {
		return nil, fmt.Errorf("dial tcp %s: connection refused", request.URL.Host)
	}
	return &http.Response{
		StatusCode: status,
		Status:     fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    request,
	}, nil
}

// --- AWS -------------------------------------------------------------------

// awsCall is one request the code made, decoded enough to answer it.
type awsCall struct {
	Service string
	Action  string
	Form    url.Values
	Body    string
}

type awsReply struct {
	Status int
	Body   string
	JSON   bool
}

// fakeAWS answers for every AWS endpoint at once. The SDK finds it through
// AWS_ENDPOINT_URL, the same way it would find a local emulator, so the code
// under test builds its clients exactly as it does for a customer.
type fakeAWS struct {
	t        *testing.T
	mu       sync.Mutex
	calls    []awsCall
	handlers map[string]func(awsCall) awsReply
}

var signingService = regexp.MustCompile(`Credential=[^/]+/[^/]+/[^/]+/([^/]+)/aws4_request`)

func stubAWS(t *testing.T) *fakeAWS {
	t.Helper()
	fake := &fakeAWS{t: t, handlers: map[string]func(awsCall) awsReply{}}
	server := httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(server.Close)

	empty := t.TempDir()
	t.Setenv("AWS_ENDPOINT_URL", server.URL)
	t.Setenv("AWS_ACCESS_KEY_ID", "AKIDTEST")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "SECRET")
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_REGION", "eu-west-1")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(empty, "config"))
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(empty, "credentials"))
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_MAX_ATTEMPTS", "1")
	return fake
}

func (fake *fakeAWS) on(operation string, handler func(awsCall) awsReply) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.handlers[operation] = handler
}

func (fake *fakeAWS) reply(operation string, reply awsReply) {
	fake.on(operation, func(awsCall) awsReply { return reply })
}

// sequence answers one operation with each reply in turn, repeating the last.
func (fake *fakeAWS) sequence(operation string, replies ...awsReply) {
	next := 0
	fake.on(operation, func(awsCall) awsReply {
		reply := replies[next]
		if next < len(replies)-1 {
			next++
		}
		return reply
	})
}

func (fake *fakeAWS) made(operation string) []awsCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matching []awsCall
	for _, call := range fake.calls {
		name := call.Service + " " + call.Action
		if prefix, wildcard := strings.CutSuffix(operation, "*"); name == operation || wildcard && strings.HasPrefix(name, prefix) {
			matching = append(matching, call)
		}
	}
	return matching
}

func (fake *fakeAWS) serve(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	call := awsCall{Body: string(body)}
	if match := signingService.FindStringSubmatch(request.Header.Get("Authorization")); match != nil {
		call.Service = match[1]
	}
	switch {
	case request.Header.Get("X-Amz-Target") != "":
		_, call.Action, _ = strings.Cut(request.Header.Get("X-Amz-Target"), ".")
	case strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded"):
		call.Form, _ = url.ParseQuery(string(body))
		call.Action = call.Form.Get("Action")
	default:
		call.Action = request.Method + " " + request.URL.Path
	}

	fake.mu.Lock()
	fake.calls = append(fake.calls, call)
	handler, found := fake.handlers[call.Service+" "+call.Action]
	if !found {
		// A key ending in * answers every path under it, for the REST
		// services that put an id in the path.
		for key, candidate := range fake.handlers {
			if prefix, wildcard := strings.CutSuffix(key, "*"); wildcard &&
				strings.HasPrefix(call.Service+" "+call.Action, prefix) {
				handler, found = candidate, true
			}
		}
	}
	fake.mu.Unlock()
	if !found {
		fake.t.Errorf("unexpected AWS call: %s %s", call.Service, call.Action)
		writer.WriteHeader(http.StatusNotImplemented)
		return
	}
	reply := handler(call)
	if reply.JSON {
		writer.Header().Set("Content-Type", "application/x-amz-json-1.1")
	}
	if reply.Status == 0 {
		reply.Status = http.StatusOK
	}
	writer.WriteHeader(reply.Status)
	_, _ = io.WriteString(writer, reply.Body)
}

func queryOK(action, result string) awsReply {
	return awsReply{Body: fmt.Sprintf(
		`<%[1]sResponse><%[1]sResult>%[2]s</%[1]sResult><ResponseMetadata><RequestId>req</RequestId></ResponseMetadata></%[1]sResponse>`,
		action, result)}
}

func queryError(code, message string) awsReply {
	return awsReply{Status: http.StatusBadRequest, Body: fmt.Sprintf(
		`<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>req</RequestId></ErrorResponse>`,
		code, message)}
}

func ec2OK(action, result string) awsReply {
	return awsReply{Body: fmt.Sprintf(`<%[1]sResponse><requestId>req</requestId>%[2]s</%[1]sResponse>`, action, result)}
}

func jsonOK(body string) awsReply { return awsReply{Body: body, JSON: true} }

func jsonError(code, message string) awsReply {
	return awsReply{Status: http.StatusBadRequest, JSON: true,
		Body: fmt.Sprintf(`{"__type":%q,"message":%q}`, code, message)}
}

func stackWithOutputs(status string, outputs map[string]string) awsReply {
	var builder strings.Builder
	builder.WriteString("<Stacks><member><StackName>stack</StackName><StackStatus>" + status + "</StackStatus>")
	builder.WriteString("<CreationTime>1970-01-01T00:00:00Z</CreationTime><Outputs>")
	for key, value := range outputs {
		fmt.Fprintf(&builder, "<member><OutputKey>%s</OutputKey><OutputValue>%s</OutputValue></member>", key, value)
	}
	builder.WriteString("</Outputs></member></Stacks>")
	return queryOK("DescribeStacks", builder.String())
}

func stackMissing() awsReply {
	return queryError("ValidationError", "Stack with id stack does not exist")
}

func targetHealth(address, state string) awsReply {
	return queryOK("DescribeTargetHealth", fmt.Sprintf(
		`<TargetHealthDescriptions><member><Target><Id>%s</Id><Port>3000</Port></Target>`+
			`<TargetHealth><State>%s</State></TargetHealth></member></TargetHealthDescriptions>`, address, state))
}
