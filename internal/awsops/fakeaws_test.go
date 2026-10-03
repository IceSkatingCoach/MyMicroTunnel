// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
)

// awsCall is one request the code under test made, decoded enough to assert
// on: which service, which operation, and what it sent.
type awsCall struct {
	Service string
	// Action is the query Action, the JSON target's operation, or "METHOD
	// path" for the REST services.
	Action string
	Form   url.Values
	JSON   map[string]any
	Body   string
	Header http.Header
}

// awsReply is what the fake answers with, already in the service's own wire
// format.
type awsReply struct {
	Status int
	Body   string
	Header map[string]string
}

// fakeAWS stands in for every AWS endpoint at once. Requests are routed by the
// signing service in the Authorization header and the operation, so one server
// can answer for CloudFormation and S3 alike without either knowing.
type fakeAWS struct {
	t      *testing.T
	server *httptest.Server

	mu       sync.Mutex
	calls    []awsCall
	handlers map[string]func(awsCall) awsReply
}

var signingService = regexp.MustCompile(`Credential=[^/]+/[^/]+/[^/]+/([^/]+)/aws4_request`)

func newFakeAWS(t *testing.T) *fakeAWS {
	t.Helper()
	fake := &fakeAWS{t: t, handlers: map[string]func(awsCall) awsReply{}}
	fake.server = httptest.NewServer(http.HandlerFunc(fake.serve))
	t.Cleanup(fake.server.Close)
	return fake
}

// client is a real Client whose every service talks to the fake.
func (fake *fakeAWS) client() *Client {
	return newClient(fake.config("AKIDTEST"))
}

func (fake *fakeAWS) config(accessKeyID string) aws.Config {
	return aws.Config{
		Region:       "eu-west-1",
		BaseEndpoint: aws.String(fake.server.URL),
		Credentials:  credentials.NewStaticCredentialsProvider(accessKeyID, "SECRET", ""),
		Retryer:      func() aws.Retryer { return aws.NopRetryer{} },
		HTTPClient:   fake.server.Client(),
		// Plain checksums rather than aws-chunked trailers, so an uploaded
		// body arrives as the bytes that were sent.
		RequestChecksumCalculation: aws.RequestChecksumCalculationWhenRequired,
	}
}

// on answers one operation, keyed "service Action": "cloudformation
// DescribeStacks", "ssm GetParameter", "s3 PUT /bucket/key".
func (fake *fakeAWS) on(operation string, handler func(awsCall) awsReply) {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	fake.handlers[operation] = handler
}

// reply answers one operation the same way every time.
func (fake *fakeAWS) reply(operation string, reply awsReply) {
	fake.on(operation, func(awsCall) awsReply { return reply })
}

// sequence answers one operation with each reply in turn, repeating the last.
func (fake *fakeAWS) sequence(operation string, replies ...awsReply) {
	var mu sync.Mutex
	next := 0
	fake.on(operation, func(awsCall) awsReply {
		mu.Lock()
		defer mu.Unlock()
		reply := replies[next]
		if next < len(replies)-1 {
			next++
		}
		return reply
	})
}

// made returns the calls to one operation, in order.
func (fake *fakeAWS) made(operation string) []awsCall {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var matching []awsCall
	for _, call := range fake.calls {
		if call.Service+" "+call.Action == operation {
			matching = append(matching, call)
		}
	}
	return matching
}

func (fake *fakeAWS) operations() []string {
	fake.mu.Lock()
	defer fake.mu.Unlock()
	names := make([]string, 0, len(fake.calls))
	for _, call := range fake.calls {
		names = append(names, call.Service+" "+call.Action)
	}
	return names
}

func (fake *fakeAWS) serve(writer http.ResponseWriter, request *http.Request) {
	body, _ := io.ReadAll(request.Body)
	call := awsCall{Body: string(body), Header: request.Header.Clone()}
	if match := signingService.FindStringSubmatch(request.Header.Get("Authorization")); match != nil {
		call.Service = match[1]
	}

	switch {
	case request.Header.Get("X-Amz-Target") != "":
		_, call.Action, _ = strings.Cut(request.Header.Get("X-Amz-Target"), ".")
		call.JSON = map[string]any{}
		_ = json.Unmarshal(body, &call.JSON)
	case strings.HasPrefix(request.Header.Get("Content-Type"), "application/x-www-form-urlencoded"):
		call.Form, _ = url.ParseQuery(string(body))
		call.Action = call.Form.Get("Action")
	default:
		call.Action = request.Method + " " + request.URL.EscapedPath()
		if request.URL.RawQuery != "" {
			call.Action += "?" + request.URL.RawQuery
		}
	}

	fake.mu.Lock()
	fake.calls = append(fake.calls, call)
	handler, found := fake.handlers[call.Service+" "+call.Action]
	if !found {
		// REST operations are also matched without their query string, which
		// carries SDK bookkeeping such as x-id that no test cares about.
		withoutQuery, _, _ := strings.Cut(call.Action, "?")
		handler, found = fake.handlers[call.Service+" "+withoutQuery]
	}
	fake.mu.Unlock()

	if !found {
		fake.t.Errorf("unexpected AWS call: %s %s", call.Service, call.Action)
		writer.WriteHeader(http.StatusNotImplemented)
		return
	}
	reply := handler(call)
	for name, value := range reply.Header {
		writer.Header().Set(name, value)
	}
	if reply.Status == 0 {
		reply.Status = http.StatusOK
	}
	writer.WriteHeader(reply.Status)
	_, _ = io.WriteString(writer, reply.Body)
}

// --- the wire formats -------------------------------------------------------

// queryOK is the awsquery success envelope CloudFormation, ELBv2, STS, IAM and
// Auto Scaling share. queryError's body is also what Route53 and CloudFront
// send for a failure.
func queryOK(action, result string) awsReply {
	return awsReply{Body: fmt.Sprintf(
		`<%[1]sResponse><%[1]sResult>%[2]s</%[1]sResult><ResponseMetadata><RequestId>req</RequestId></ResponseMetadata></%[1]sResponse>`,
		action, result)}
}

func queryError(status int, code, message string) awsReply {
	return awsReply{Status: status, Body: fmt.Sprintf(
		`<ErrorResponse><Error><Type>Sender</Type><Code>%s</Code><Message>%s</Message></Error><RequestId>req</RequestId></ErrorResponse>`,
		code, message)}
}

// ec2OK is EC2's own dialect of the query protocol: no Result wrapper.
func ec2OK(action, result string) awsReply {
	return awsReply{Body: fmt.Sprintf(`<%[1]sResponse><requestId>req</requestId>%[2]s</%[1]sResponse>`, action, result)}
}

func ec2Error(code, message string) awsReply {
	return awsReply{Status: http.StatusBadRequest, Body: fmt.Sprintf(
		`<Response><Errors><Error><Code>%s</Code><Message>%s</Message></Error></Errors><RequestID>req</RequestID></Response>`,
		code, message)}
}

func jsonOK(value any) awsReply {
	encoded, _ := json.Marshal(value)
	return awsReply{Body: string(encoded), Header: map[string]string{"Content-Type": "application/x-amz-json-1.1"}}
}

func jsonError(code, message string) awsReply {
	return awsReply{
		Status: http.StatusBadRequest,
		Body:   fmt.Sprintf(`{"__type":%q,"message":%q}`, code, message),
		Header: map[string]string{"Content-Type": "application/x-amz-json-1.1"},
	}
}

// stacksXML renders a DescribeStacks result with the given status,
// parameters and outputs.
func stacksXML(status string, parameters, outputs map[string]string) string {
	var builder strings.Builder
	builder.WriteString("<Stacks><member><StackName>stack</StackName><StackStatus>" + status + "</StackStatus>")
	builder.WriteString("<CreationTime>" + time.Unix(0, 0).UTC().Format(time.RFC3339) + "</CreationTime>")
	builder.WriteString("<Parameters>")
	for key, value := range parameters {
		fmt.Fprintf(&builder, "<member><ParameterKey>%s</ParameterKey><ParameterValue>%s</ParameterValue></member>", key, value)
	}
	builder.WriteString("</Parameters><Outputs>")
	for key, value := range outputs {
		fmt.Fprintf(&builder, "<member><OutputKey>%s</OutputKey><OutputValue>%s</OutputValue></member>", key, value)
	}
	builder.WriteString("</Outputs></member></Stacks>")
	return builder.String()
}

func describeStacks(status string, parameters, outputs map[string]string) awsReply {
	return queryOK("DescribeStacks", stacksXML(status, parameters, outputs))
}

func stackMissing() awsReply {
	return queryError(http.StatusBadRequest, "ValidationError", "Stack with id stack does not exist")
}

// fastPolling makes every wait loop spin instead of sleep for one test.
func fastPolling(t *testing.T) {
	t.Helper()
	changeSet, other := changeSetPollInterval, pollInterval
	changeSetPollInterval, pollInterval = time.Millisecond, time.Millisecond
	t.Cleanup(func() { changeSetPollInterval, pollInterval = changeSet, other })
}

// slowPolling makes every wait loop sleep far longer than any test runs, so
// the only way out of the sleep is the context.
func slowPolling(t *testing.T) {
	t.Helper()
	changeSet, other := changeSetPollInterval, pollInterval
	changeSetPollInterval, pollInterval = time.Hour, time.Hour
	t.Cleanup(func() { changeSetPollInterval, pollInterval = changeSet, other })
}
