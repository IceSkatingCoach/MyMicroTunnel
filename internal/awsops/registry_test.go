// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

func parameterValue(value string) awsReply {
	return jsonOK(map[string]any{"Parameter": map[string]any{"Name": "/p", "Value": value, "Type": "String"}})
}

func sentPeers(t *testing.T, call awsCall) []Peer {
	t.Helper()
	var peers []Peer
	if err := json.Unmarshal([]byte(call.JSON["Value"].(string)), &peers); err != nil {
		t.Fatalf("PutParameter sent something other than a peer list: %v", call.JSON)
	}
	return peers
}

func TestPeersReadsTheRegistry(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", parameterValue(`[{"publicKey":"aaa=","address":"10.100.0.2","label":"laptop"}]`))

	peers, err := fake.client().Peers(context.Background(), "/mmt/peers")
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0] != (Peer{PublicKey: "aaa=", Address: "10.100.0.2", Label: "laptop"}) {
		t.Errorf("got %v", peers)
	}
	if name := fake.made("ssm GetParameter")[0].JSON["Name"]; name != "/mmt/peers" {
		t.Errorf("read %v, want /mmt/peers", name)
	}
}

// Between deploying a stack and registering its first workstation there is
// no registry yet, or an empty one; neither is an error.
func TestPeersOfAFreshStackIsAnEmptyList(t *testing.T) {
	for name, reply := range map[string]awsReply{
		"missing": jsonError("ParameterNotFound", ""),
		"blank":   parameterValue("  "),
	} {
		fake := newFakeAWS(t)
		fake.reply("ssm GetParameter", reply)
		if peers, err := fake.client().Peers(context.Background(), "/p"); err != nil || peers != nil {
			t.Errorf("%s: got %v, %v", name, peers, err)
		}
	}
}

func TestPeersRejectsSomethingThatIsNotAPeerList(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", parameterValue("not json"))
	if _, err := fake.client().Peers(context.Background(), "/p"); err == nil || !strings.Contains(err.Error(), "/p does not hold a peer list") {
		t.Errorf("got %v", err)
	}

	fake.reply("ssm GetParameter", jsonError("AccessDeniedException", "denied"))
	if _, err := fake.client().Peers(context.Background(), "/p"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("got %v", err)
	}
}

func TestUpsertPeerWritesTheMergedListSortedByAddress(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", parameterValue(`[{"publicKey":"bbb=","address":"10.100.0.3"},{"publicKey":"old=","address":"10.100.0.9"}]`))
	fake.reply("ssm PutParameter", jsonOK(map[string]any{"Version": 2}))

	updated, err := fake.client().UpsertPeer(context.Background(), "/p", Peer{PublicKey: "old=", Address: "10.100.0.2", Label: "laptop"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(addresses(updated), []string{"10.100.0.2", "10.100.0.3"}) {
		t.Errorf("returned %v", addresses(updated))
	}

	put := fake.made("ssm PutParameter")
	if len(put) != 1 {
		t.Fatalf("got %d writes, want 1", len(put))
	}
	if put[0].JSON["Name"] != "/p" || put[0].JSON["Type"] != "String" || put[0].JSON["Overwrite"] != true {
		t.Errorf("unexpected PutParameter: %v", put[0].JSON)
	}
	if sent := sentPeers(t, put[0]); !slices.Equal(addresses(sent), []string{"10.100.0.2", "10.100.0.3"}) {
		t.Errorf("wrote %v", sent)
	}
}

func TestUpsertPeerSurfacesFailures(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", jsonError("InternalServerError", "read failed"))
	if _, err := fake.client().UpsertPeer(context.Background(), "/p", Peer{Address: "10.100.0.2"}); err == nil || !strings.Contains(err.Error(), "read failed") {
		t.Errorf("got %v", err)
	}
	if len(fake.made("ssm PutParameter")) != 0 {
		t.Error("wrote a registry it could not read")
	}

	fake.reply("ssm GetParameter", jsonError("ParameterNotFound", ""))
	fake.reply("ssm PutParameter", jsonError("InternalServerError", "write failed"))
	if _, err := fake.client().UpsertPeer(context.Background(), "/p", Peer{Address: "10.100.0.2"}); err == nil || !strings.Contains(err.Error(), "write failed") {
		t.Errorf("got %v", err)
	}
}

func TestRemovePeerWritesOnlyWhenSomethingChanged(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", parameterValue(`[{"publicKey":"aaa=","address":"10.100.0.2"},{"publicKey":"bbb=","address":"10.100.0.3"}]`))
	fake.reply("ssm PutParameter", jsonOK(map[string]any{"Version": 3}))
	client := fake.client()

	if err := client.RemovePeer(context.Background(), "/p", "10.100.0.9"); err != nil {
		t.Fatal(err)
	}
	if len(fake.made("ssm PutParameter")) != 0 {
		t.Error("removing an unknown address rewrote the registry")
	}

	if err := client.RemovePeer(context.Background(), "/p", "10.100.0.2"); err != nil {
		t.Fatal(err)
	}
	put := fake.made("ssm PutParameter")
	if len(put) != 1 || !slices.Equal(addresses(sentPeers(t, put[0])), []string{"10.100.0.3"}) {
		t.Errorf("unexpected writes: %v", put)
	}

	fake.reply("ssm GetParameter", jsonError("InternalServerError", "read failed"))
	if err := client.RemovePeer(context.Background(), "/p", "10.100.0.3"); err == nil {
		t.Error("an unreadable registry was treated as already clean")
	}
}

func TestWaitForParameterReturnsOnceTheGatewayPublishes(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.sequence("ssm GetParameter", jsonError("ParameterNotFound", ""), parameterValue("serverkey="))

	value, err := fake.client().WaitForParameter(context.Background(), "/mmt/server-key", time.Minute)
	if err != nil || value != "serverkey=" {
		t.Errorf("got %q, %v", value, err)
	}
	if polls := len(fake.made("ssm GetParameter")); polls != 2 {
		t.Errorf("polled %d times, want 2", polls)
	}
}

func TestWaitForParameterGivesUpAtTheDeadline(t *testing.T) {
	fastPolling(t)
	fake := newFakeAWS(t)
	fake.reply("ssm GetParameter", jsonError("ParameterNotFound", ""))

	_, err := fake.client().WaitForParameter(context.Background(), "/mmt/server-key", 20*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "the gateway never published /mmt/server-key") {
		t.Errorf("got %v", err)
	}
}

func TestDeleteParameterIgnoresOneAlreadyGone(t *testing.T) {
	fake := newFakeAWS(t)
	fake.sequence("ssm DeleteParameter", jsonOK(map[string]any{}), jsonError("ParameterNotFound", ""), jsonError("AccessDeniedException", "denied"))
	client := fake.client()

	if err := client.DeleteParameter(context.Background(), "/p"); err != nil {
		t.Errorf("first delete: %v", err)
	}
	if err := client.DeleteParameter(context.Background(), "/p"); err != nil {
		t.Errorf("an already-deleted parameter: %v", err)
	}
	if err := client.DeleteParameter(context.Background(), "/p"); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Errorf("got %v", err)
	}
	if name := fake.made("ssm DeleteParameter")[0].JSON["Name"]; name != "/p" {
		t.Errorf("deleted %v", name)
	}
}

func TestDeleteParametersByPathPagesAndDeletesInTens(t *testing.T) {
	page := func(from, to int, next string) awsReply {
		var parameters []map[string]any
		for index := from; index < to; index++ {
			parameters = append(parameters, map[string]any{"Name": fmt.Sprintf("/mmt/p%02d", index)})
		}
		body := map[string]any{"Parameters": parameters}
		if next != "" {
			body["NextToken"] = next
		}
		return jsonOK(body)
	}
	fake := newFakeAWS(t)
	fake.sequence("ssm GetParametersByPath", page(0, 7, "more"), page(7, 12, ""))
	fake.reply("ssm DeleteParameters", jsonOK(map[string]any{}))

	if err := fake.client().DeleteParametersByPath(context.Background(), "/mmt"); err != nil {
		t.Fatal(err)
	}

	listed := fake.made("ssm GetParametersByPath")
	if len(listed) != 2 || listed[0].JSON["Path"] != "/mmt" || listed[0].JSON["Recursive"] != true || listed[1].JSON["NextToken"] != "more" {
		t.Errorf("unexpected listing: %v", listed)
	}
	deleted := fake.made("ssm DeleteParameters")
	if len(deleted) != 2 {
		t.Fatalf("got %d deletions, want 2", len(deleted))
	}
	first, second := deleted[0].JSON["Names"].([]any), deleted[1].JSON["Names"].([]any)
	if len(first) != 10 || len(second) != 2 || first[0] != "/mmt/p00" || second[1] != "/mmt/p11" {
		t.Errorf("deleted %v then %v", first, second)
	}
}

func TestDeleteParametersByPathSurfacesFailures(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ssm GetParametersByPath", jsonError("ParameterNotFound", ""))
	client := fake.client()
	if err := client.DeleteParametersByPath(context.Background(), "/mmt"); err != nil {
		t.Errorf("a path with nothing under it: %v", err)
	}

	fake.reply("ssm GetParametersByPath", jsonError("AccessDeniedException", "cannot list"))
	if err := client.DeleteParametersByPath(context.Background(), "/mmt"); err == nil || !strings.Contains(err.Error(), "cannot list") {
		t.Errorf("got %v", err)
	}

	fake.reply("ssm GetParametersByPath", jsonOK(map[string]any{"Parameters": []map[string]any{{"Name": "/mmt/a"}}}))
	fake.reply("ssm DeleteParameters", jsonError("AccessDeniedException", "cannot delete"))
	if err := client.DeleteParametersByPath(context.Background(), "/mmt"); err == nil || !strings.Contains(err.Error(), "cannot delete") {
		t.Errorf("got %v", err)
	}
}

func TestRegisterAndDeregisterTargetOutsideTheVPC(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("elasticloadbalancing RegisterTargets", queryOK("RegisterTargets", ""))
	fake.sequence("elasticloadbalancing DeregisterTargets",
		queryOK("DeregisterTargets", ""),
		queryError(http.StatusBadRequest, "InvalidTarget", "not registered"),
		queryError(http.StatusBadRequest, "TargetGroupNotFound", "no group"))
	client := fake.client()

	if err := client.RegisterTarget(context.Background(), "arn:tg", "10.100.0.2", 22); err != nil {
		t.Fatal(err)
	}
	form := fake.made("elasticloadbalancing RegisterTargets")[0].Form
	// "all" is what lets a target outside every subnet be registered at all.
	if form.Get("TargetGroupArn") != "arn:tg" || form.Get("Targets.member.1.Id") != "10.100.0.2" ||
		form.Get("Targets.member.1.Port") != "22" || form.Get("Targets.member.1.AvailabilityZone") != "all" {
		t.Errorf("unexpected registration: %v", form)
	}

	if err := client.DeregisterTarget(context.Background(), "arn:tg", "10.100.0.2", 22); err != nil {
		t.Fatal(err)
	}
	form = fake.made("elasticloadbalancing DeregisterTargets")[0].Form
	if form.Get("Targets.member.1.Id") != "10.100.0.2" || form.Get("Targets.member.1.AvailabilityZone") != "all" {
		t.Errorf("unexpected deregistration: %v", form)
	}
	if err := client.DeregisterTarget(context.Background(), "arn:tg", "10.100.0.2", 22); err != nil {
		t.Errorf("a target already gone: %v", err)
	}
	if err := client.DeregisterTarget(context.Background(), "arn:tg", "10.100.0.2", 22); err == nil || !strings.Contains(err.Error(), "no group") {
		t.Errorf("got %v", err)
	}
}

func TestTargetHealthOfReportsOnTheAskedAddressOnly(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("elasticloadbalancing DescribeTargetHealth", queryOK("DescribeTargetHealth", `<TargetHealthDescriptions>
		<member><Target><Id>10.100.0.9</Id><Port>22</Port></Target><TargetHealth><State>healthy</State></TargetHealth></member>
		<member><Target><Id>10.100.0.2</Id><Port>22</Port></Target><TargetHealth><State>unhealthy</State><Description>Health checks failed</Description></TargetHealth></member>
		<member><Target><Id>10.100.0.3</Id><Port>22</Port></Target><TargetHealth><State>healthy</State><Description>ignored when healthy</Description></TargetHealth></member>
		<member><Target><Id>10.100.0.4</Id><Port>22</Port></Target></member>
		<member><HealthCheckPort>22</HealthCheckPort></member>
	</TargetHealthDescriptions>`))
	client := fake.client()

	for address, want := range map[string]string{
		"10.100.0.2": "unhealthy (Health checks failed)",
		"10.100.0.3": "healthy",
		"10.100.0.4": "unknown",
		"10.100.0.5": "not registered",
	} {
		if got, err := client.TargetHealthOf(context.Background(), "arn:tg", address); err != nil || got != want {
			t.Errorf("%s: got %q, %v, want %q", address, got, err, want)
		}
	}
	if arn := fake.made("elasticloadbalancing DescribeTargetHealth")[0].Form.Get("TargetGroupArn"); arn != "arn:tg" {
		t.Errorf("asked about %q", arn)
	}

	fake.reply("elasticloadbalancing DescribeTargetHealth", queryError(http.StatusBadRequest, "TargetGroupNotFound", "no group"))
	if _, err := client.TargetHealthOf(context.Background(), "arn:tg", "10.100.0.2"); err == nil {
		t.Error("a missing target group was not reported")
	}
}
