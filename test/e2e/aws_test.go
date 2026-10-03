// SPDX-License-Identifier: GPL-3.0-or-later

//go:build e2e

// Package e2e drives the built command against an emulated AWS account: the
// real CloudFormation template deployed by the real installer, with moto
// standing in for AWS (see moto_server.py).
//
//	MMT_E2E_PYTHON=/path/to/python-with-moto go test -tags e2e ./test/e2e/
//
// It sits between the hermetic tests, which never reach AWS at all, and the
// nightly integration run, which deploys into a real account and costs money.
// What it catches is what the installer and the template hand each other:
// parameters the template no longer declares, outputs the installer reads
// that the template stopped writing, the peer list and the load balancer
// targets a workstation is registered in, and what uninstall leaves behind.
//
// Uninstall also takes down this machine's tunnel and sudoers rule through
// sudo, so that part runs only where MMT_E2E_DISPOSABLE=1 says the machine is
// a throwaway one, such as a CI runner.
package e2e

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

const (
	region = "us-east-1"
	zone   = "example.test"
	stack  = "e2e"
	domain = "app." + zone
)

// account is one emulated AWS account, and the clients the test inspects it with.
type account struct {
	endpoint string
	cfg      aws.Config
}

func startMoto(t *testing.T) account {
	t.Helper()
	python := os.Getenv("MMT_E2E_PYTHON")
	if python == "" {
		t.Skip("MMT_E2E_PYTHON is not set: point it at a Python with moto[server] installed")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	server := exec.Command(python, "moto_server.py", fmt.Sprint(port))
	logFile, err := os.Create(filepath.Join(t.TempDir(), "moto.log"))
	if err != nil {
		t.Fatal(err)
	}
	server.Stdout, server.Stderr = logFile, logFile
	if err := server.Start(); err != nil {
		t.Fatalf("starting moto: %v", err)
	}
	t.Cleanup(func() {
		server.Process.Kill()
		server.Wait()
		if t.Failed() {
			log, _ := os.ReadFile(logFile.Name())
			t.Logf("moto's log:\n%s", log)
		}
	})

	endpoint := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitFor(t, 30*time.Second, "moto to listen", func() bool {
		response, err := http.Get(endpoint + "/moto-api/")
		if err != nil {
			return false
		}
		response.Body.Close()
		return true
	})

	return account{
		endpoint: endpoint,
		cfg: aws.Config{
			Region:       region,
			BaseEndpoint: aws.String(endpoint),
			Credentials:  credentials.NewStaticCredentialsProvider("test", "test", ""),
		},
	}
}

func waitFor(t *testing.T, timeout time.Duration, what string, done func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !done() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// seed gives the account what a customer's already has: a VPC whose subnets
// reach an internet gateway, and a public hosted zone for the domain.
func (a account) seed(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	compute := ec2.NewFromConfig(a.cfg)

	vpcs, err := compute.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		t.Fatal(err)
	}
	var vpcID string
	for _, vpc := range vpcs.Vpcs {
		if aws.ToBool(vpc.IsDefault) {
			vpcID = aws.ToString(vpc.VpcId)
		}
	}
	gateway, err := compute.CreateInternetGateway(ctx, &ec2.CreateInternetGatewayInput{})
	if err != nil {
		t.Fatal(err)
	}
	gatewayID := gateway.InternetGateway.InternetGatewayId
	if _, err := compute.AttachInternetGateway(ctx, &ec2.AttachInternetGatewayInput{InternetGatewayId: gatewayID, VpcId: &vpcID}); err != nil {
		t.Fatal(err)
	}
	tables, err := compute.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{vpcID}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, table := range tables.RouteTables {
		if _, err := compute.CreateRoute(ctx, &ec2.CreateRouteInput{
			RouteTableId: table.RouteTableId, DestinationCidrBlock: aws.String("0.0.0.0/0"), GatewayId: gatewayID,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := route53.NewFromConfig(a.cfg).CreateHostedZone(ctx, &route53.CreateHostedZoneInput{
		Name: aws.String(zone + "."), CallerReference: aws.String("e2e"),
	}); err != nil {
		t.Fatal(err)
	}
}

// actAsGateway publishes the gateway's WireGuard key once the stack exists,
// which is what the instance's boot script does on real AWS. Nothing boots in
// moto, so without this the installer would wait for a key that never comes.
func (a account) actAsGateway(t *testing.T, done <-chan struct{}) string {
	t.Helper()
	key := newKey(t)
	go func() {
		ctx := context.Background()
		formation := cloudformation.NewFromConfig(a.cfg)
		for {
			select {
			case <-done:
				return
			case <-time.After(200 * time.Millisecond):
			}
			stacks, err := formation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stack)})
			if err != nil || len(stacks.Stacks) == 0 || !strings.HasSuffix(string(stacks.Stacks[0].StackStatus), "_COMPLETE") {
				continue
			}
			ssm.NewFromConfig(a.cfg).PutParameter(ctx, &ssm.PutParameterInput{
				Name: aws.String("/" + stack + "/wireguard/server-public-key"), Value: aws.String(key),
				Type: "String", Overwrite: aws.Bool(true),
			})
			return
		}
	}()
	return key
}

func newKey(t *testing.T) string {
	t.Helper()
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(key)
}

func build(t *testing.T) string {
	t.Helper()
	binary := filepath.Join(t.TempDir(), "mymicrotunnel")
	if output, err := exec.Command("go", "build", "-o", binary, "../../cmd/mymicrotunnel").CombinedOutput(); err != nil {
		t.Fatalf("building the command: %v\n%s", err, output)
	}
	return binary
}

// machine is one workstation: a home directory of its own, so two of them
// can join the same deployment as two Macs would.
type machine struct {
	t       *testing.T
	binary  string
	home    string
	account account
}

func (m machine) run(args ...string) (string, int) {
	m.t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	command := exec.CommandContext(ctx, m.binary, args...)
	command.Env = append(os.Environ(),
		"HOME="+m.home,
		"AWS_ENDPOINT_URL="+m.account.endpoint,
		"AWS_REGION="+region,
		"AWS_EC2_METADATA_DISABLED=true",
	)
	output, err := command.CombinedOutput()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return string(output), exit.ExitCode()
	}
	if err != nil {
		m.t.Fatalf("running %v: %v\n%s", args, err, output)
	}
	return string(output), 0
}

// deploy runs the half of an install that needs only AWS: no root, no tunnel.
func (m machine) deploy(clientAddress string) (settings map[string]any, clientKey string) {
	m.t.Helper()
	clientKey = newKey(m.t)
	settingsPath := filepath.Join(m.home, "settings.json")
	output, code := m.run("install",
		"--stage", "deploy", "--non-interactive",
		"--access-key-id", "test", "--secret-access-key", "test",
		"--region", region, "--stack", stack, "--domain", domain,
		"--port", "3000", "--tcp-ports", "5432",
		"--client-ip", clientAddress,
		"--client-public-key", clientKey,
		"--settings", settingsPath,
	)
	if code != 0 {
		m.t.Fatalf("deploy exited %d:\n%s", code, output)
	}
	body, err := os.ReadFile(settingsPath)
	if err != nil {
		m.t.Fatalf("the deploy stage wrote no settings: %v\n%s", err, output)
	}
	if err := json.Unmarshal(body, &settings); err != nil {
		m.t.Fatal(err)
	}
	return settings, clientKey
}

func TestTwoWorkstationsShareADeploymentAndUninstallLeavesNothingBehind(t *testing.T) {
	cloud := startMoto(t)
	cloud.seed(t)
	binary := build(t)
	ctx := context.Background()

	done := make(chan struct{})
	defer close(done)
	gatewayKey := cloud.actAsGateway(t, done)

	first := machine{t: t, binary: binary, home: t.TempDir(), account: cloud}
	settings, firstKey := first.deploy("10.100.0.2")

	// What the next stages and the app read back has to have come from the
	// stack, not from a default the installer filled in.
	if settings["serverPublicKey"] != gatewayKey {
		t.Errorf("the settings carry server key %v, want the one the gateway published", settings["serverPublicKey"])
	}
	for _, field := range []string{"endpoint", "targetGroupArn", "wakeRoleArn", "gatewayGroupName", "loadBalancerDnsName"} {
		if value, _ := settings[field].(string); value == "" || strings.Contains(value, "Fn::") {
			t.Errorf("settings field %s = %q, want a value the stack reported", field, value)
		}
	}
	if settings["loadBalancerZoneId"] != "Z26RNL4JYFTOTI" {
		t.Errorf("load balancer zone = %v", settings["loadBalancerZoneId"])
	}

	formation := cloudformation.NewFromConfig(cloud.cfg)
	described, err := formation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stack)})
	if err != nil {
		t.Fatal(err)
	}
	outputs := map[string]string{}
	for _, output := range described.Stacks[0].Outputs {
		outputs[aws.ToString(output.OutputKey)] = aws.ToString(output.OutputValue)
	}
	// --port 3000 publishes on 3000 as well, so the URL names the port.
	if outputs["ServiceUrl"] != "https://"+domain+":3000" {
		t.Errorf("ServiceUrl = %q", outputs["ServiceUrl"])
	}
	tcpTarget := outputs["TcpTarget1"]
	if !strings.HasPrefix(tcpTarget, "5432:5432=arn:") {
		t.Fatalf("TcpTarget1 = %q, want the extra port and its target group", tcpTarget)
	}

	records, err := route53.NewFromConfig(cloud.cfg).ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(settings["hostedZoneId"].(string)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasAlias(records, domain, settings["loadBalancerDnsName"].(string)) {
		t.Errorf("no alias from %s to the load balancer in %+v", domain, records.ResourceRecordSets)
	}

	// A second Mac joins the same deployment, which redeploys a stack that
	// already exists and must not disturb the first one's registration.
	second := machine{t: t, binary: binary, home: t.TempDir(), account: cloud}
	_, secondKey := second.deploy("10.100.0.3")

	peers, code := first.run("peers", "--stack", stack, "--profile", "mymicrotunnel", "--region", region)
	if code != 0 || !strings.Contains(peers, firstKey) || !strings.Contains(peers, secondKey) {
		t.Fatalf("peers exited %d, want both workstations listed:\n%s", code, peers)
	}

	balancer := elbv2Client(cloud)
	assertTargets(t, balancer, settings["targetGroupArn"].(string), "10.100.0.2", "10.100.0.3")
	assertTargets(t, balancer, strings.SplitN(tcpTarget, "=", 2)[1], "10.100.0.2", "10.100.0.3")

	// Retiring the second Mac takes it out of the peer list and out from
	// behind the load balancer, and leaves the first exactly as it was.
	if output, code := first.run("peers", "--stack", stack, "--profile", "mymicrotunnel", "--region", region, "--remove", "10.100.0.3"); code != 0 {
		t.Fatalf("peers --remove exited %d:\n%s", code, output)
	}
	peers, _ = first.run("peers", "--stack", stack, "--profile", "mymicrotunnel", "--region", region)
	if !strings.Contains(peers, firstKey) || strings.Contains(peers, secondKey) {
		t.Errorf("after removing 10.100.0.3, peers:\n%s", peers)
	}
	assertTargets(t, balancer, settings["targetGroupArn"].(string), "10.100.0.2")

	if os.Getenv("MMT_E2E_DISPOSABLE") != "1" {
		t.Log("uninstall skipped: it runs sudo against this machine; set MMT_E2E_DISPOSABLE=1 on a throwaway one")
		return
	}
	output, code := first.run("uninstall", "--stack", stack, "--profile", "mymicrotunnel", "--region", region,
		"--delete-stack", "--keep-app", "--non-interactive")
	if code != 0 {
		t.Fatalf("uninstall exited %d:\n%s", code, output)
	}
	if _, err := formation.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stack)}); err == nil {
		t.Error("the stack is still standing after uninstall --delete-stack")
	}
	parameters, err := ssm.NewFromConfig(cloud.cfg).GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
		Path: aws.String("/" + stack + "/wireguard"), Recursive: aws.Bool(true),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parameters.Parameters) != 0 {
		t.Errorf("%d parameters left under /%s/wireguard", len(parameters.Parameters), stack)
	}
	records, err = route53.NewFromConfig(cloud.cfg).ListResourceRecordSets(ctx, &route53.ListResourceRecordSetsInput{
		HostedZoneId: aws.String(settings["hostedZoneId"].(string)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if hasAlias(records, domain, settings["loadBalancerDnsName"].(string)) {
		t.Errorf("the alias for %s outlived its stack", domain)
	}
}

func elbv2Client(a account) *elasticloadbalancingv2.Client {
	return elasticloadbalancingv2.NewFromConfig(a.cfg)
}

func assertTargets(t *testing.T, balancer *elasticloadbalancingv2.Client, targetGroupARN string, want ...string) {
	t.Helper()
	health, err := balancer.DescribeTargetHealth(context.Background(), &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(targetGroupARN),
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, description := range health.TargetHealthDescriptions {
		got = append(got, aws.ToString(description.Target.Id))
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("targets of %s = %v, want %v", targetGroupARN, got, want)
	}
}

func hasAlias(records *route53.ListResourceRecordSetsOutput, name, target string) bool {
	for _, record := range records.ResourceRecordSets {
		if strings.TrimSuffix(aws.ToString(record.Name), ".") == name && record.AliasTarget != nil &&
			strings.EqualFold(strings.TrimSuffix(aws.ToString(record.AliasTarget.DNSName), "."), target) {
			return true
		}
	}
	return false
}
