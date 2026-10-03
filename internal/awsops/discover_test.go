// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
)

func vpcsXML(vpcs ...string) awsReply {
	return ec2OK("DescribeVpcs", "<vpcSet>"+strings.Join(vpcs, "")+"</vpcSet>")
}

func vpcXML(id, cidr string, isDefault bool) string {
	flag := "false"
	if isDefault {
		flag = "true"
	}
	return "<item><vpcId>" + id + "</vpcId><cidrBlock>" + cidr + "</cidrBlock><isDefault>" + flag + "</isDefault></item>"
}

// routeTableXML takes the subnets the table is explicitly associated with;
// main marks the VPC's main table. internet is the default route's target.
func routeTableXML(id string, main bool, internet string, subnets ...string) string {
	var builder strings.Builder
	builder.WriteString("<item><routeTableId>" + id + "</routeTableId><routeSet>")
	builder.WriteString("<item><destinationCidrBlock>172.31.0.0/16</destinationCidrBlock><gatewayId>local</gatewayId></item>")
	if internet != "" {
		target := "<gatewayId>" + internet + "</gatewayId>"
		if strings.HasPrefix(internet, "nat-") {
			target = "<natGatewayId>" + internet + "</natGatewayId>"
		}
		builder.WriteString("<item><destinationCidrBlock>0.0.0.0/0</destinationCidrBlock>" + target + "</item>")
	}
	builder.WriteString("</routeSet><associationSet>")
	if main {
		builder.WriteString("<item><main>true</main></item>")
	}
	for _, subnet := range subnets {
		builder.WriteString("<item><main>false</main><subnetId>" + subnet + "</subnetId></item>")
	}
	builder.WriteString("</associationSet></item>")
	return builder.String()
}

func subnetXML(id, zone string) string {
	return "<item><subnetId>" + id + "</subnetId><availabilityZone>" + zone + "</availabilityZone></item>"
}

func TestDiscoverNetworkPicksOnePublicSubnetPerZone(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ec2 DescribeVpcs", vpcsXML(vpcXML("vpc-other", "10.0.0.0/16", false), vpcXML("vpc-1", "172.31.0.0/16", true)))
	fake.reply("ec2 DescribeRouteTables", ec2OK("DescribeRouteTables", "<routeTableSet>"+
		routeTableXML("rtb-main", true, "igw-1")+
		routeTableXML("rtb-private", false, "nat-1", "subnet-private")+
		routeTableXML("rtb-public", false, "igw-1", "subnet-b2", "subnet-b1", "subnet-b3")+
		"</routeTableSet>"))
	fake.reply("ec2 DescribeSubnets", ec2OK("DescribeSubnets", "<subnetSet>"+
		subnetXML("subnet-b2", "eu-west-1b")+
		subnetXML("subnet-private", "eu-west-1c")+
		subnetXML("subnet-a", "eu-west-1a")+
		subnetXML("subnet-b1", "eu-west-1b")+
		subnetXML("subnet-b3", "eu-west-1b")+
		"</subnetSet>"))

	network, err := fake.client().DiscoverNetwork(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if network.VpcID != "vpc-1" || network.VpcCidr != "172.31.0.0/16" {
		t.Errorf("chose %s %s, want the default VPC", network.VpcID, network.VpcCidr)
	}
	// subnet-a has no association, so the main table governs it. In
	// eu-west-1b the lower id wins whichever order the API listed them in,
	// and the NAT-routed subnet is not public at all.
	if !slices.Equal(network.PublicSubnets, []string{"subnet-a", "subnet-b1"}) {
		t.Errorf("public subnets %v", network.PublicSubnets)
	}
	if !slices.Equal(network.GatewaySubnets, network.PublicSubnets) {
		t.Errorf("gateway subnets %v", network.GatewaySubnets)
	}
	if !slices.Equal(network.RouteTableIDs, []string{"rtb-main", "rtb-public"}) {
		t.Errorf("route tables %v", network.RouteTableIDs)
	}
	for _, operation := range []string{"ec2 DescribeRouteTables", "ec2 DescribeSubnets"} {
		form := fake.made(operation)[0].Form
		if form.Get("Filter.1.Name") != "vpc-id" || form.Get("Filter.1.Value.1") != "vpc-1" {
			t.Errorf("%s was not scoped to the VPC: %v", operation, form)
		}
	}
}

func TestDiscoverNetworkRefusesAVPCWithNoWayOut(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ec2 DescribeVpcs", vpcsXML(vpcXML("vpc-1", "10.0.0.0/16", false)))
	fake.reply("ec2 DescribeRouteTables", ec2OK("DescribeRouteTables", "<routeTableSet>"+
		routeTableXML("rtb-private", false, "nat-1", "subnet-a")+"</routeTableSet>"))
	fake.reply("ec2 DescribeSubnets", ec2OK("DescribeSubnets", "<subnetSet>"+
		subnetXML("subnet-a", "eu-west-1a")+subnetXML("subnet-orphan", "eu-west-1b")+"</subnetSet>"))

	_, err := fake.client().DiscoverNetwork(context.Background(), "vpc-1")
	if err == nil || !strings.Contains(err.Error(), "VPC vpc-1 has no subnet with a route to an internet gateway") {
		t.Errorf("got %v", err)
	}
	if ids := fake.made("ec2 DescribeVpcs")[0].Form.Get("VpcId.1"); ids != "vpc-1" {
		t.Errorf("asked for VPC %q", ids)
	}
}

func TestDiscoverNetworkSurfacesListingFailures(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ec2 DescribeVpcs", vpcsXML(vpcXML("vpc-1", "10.0.0.0/16", true)))
	fake.reply("ec2 DescribeRouteTables", ec2Error("UnauthorizedOperation", "no tables"))
	client := fake.client()

	if _, err := client.DiscoverNetwork(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "listing route tables") {
		t.Errorf("got %v", err)
	}

	fake.reply("ec2 DescribeRouteTables", ec2OK("DescribeRouteTables", "<routeTableSet></routeTableSet>"))
	fake.reply("ec2 DescribeSubnets", ec2Error("UnauthorizedOperation", "no subnets"))
	if _, err := client.DiscoverNetwork(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "listing subnets") {
		t.Errorf("got %v", err)
	}

	fake.reply("ec2 DescribeVpcs", ec2Error("UnauthorizedOperation", "no vpcs"))
	if _, err := client.DiscoverNetwork(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "listing VPCs") {
		t.Errorf("got %v", err)
	}
}

func TestResolveVPCChoosesOnlyWhenTheChoiceIsObvious(t *testing.T) {
	fake := newFakeAWS(t)
	client := fake.client()

	fake.reply("ec2 DescribeVpcs", vpcsXML(vpcXML("vpc-only", "10.0.0.0/16", false)))
	if vpc, err := client.resolveVPC(context.Background(), ""); err != nil || *vpc.VpcId != "vpc-only" {
		t.Errorf("a single VPC: got %v, %v", vpc.VpcId, err)
	}

	// Guessing between two would deploy cleanly into the wrong one.
	fake.reply("ec2 DescribeVpcs", vpcsXML(vpcXML("vpc-b", "10.1.0.0/16", false), vpcXML("vpc-a", "10.0.0.0/16", false)))
	_, err := client.resolveVPC(context.Background(), "")
	if err == nil || !strings.Contains(err.Error(), "eu-west-1 has no default VPC and more than one to choose from; pass --vpc with one of: vpc-a (10.0.0.0/16), vpc-b (10.1.0.0/16)") {
		t.Errorf("got %v", err)
	}

	fake.reply("ec2 DescribeVpcs", vpcsXML())
	if _, err := client.resolveVPC(context.Background(), ""); err == nil || !strings.Contains(err.Error(), "no VPC in eu-west-1") {
		t.Errorf("got %v", err)
	}
	if _, err := client.resolveVPC(context.Background(), "vpc-gone"); err == nil || !strings.Contains(err.Error(), "no VPC vpc-gone in eu-west-1") {
		t.Errorf("got %v", err)
	}

	fake.reply("ec2 DescribeVpcs", ec2Error("InvalidVpcID.NotFound", "The vpc ID 'vpc-gone' does not exist"))
	if _, err := client.resolveVPC(context.Background(), "vpc-gone"); err == nil || !strings.Contains(err.Error(), "reading VPC vpc-gone") {
		t.Errorf("got %v", err)
	}
}

func hostedZonesPage(truncated bool, next string, zones ...string) awsReply {
	flag := "false"
	if truncated {
		flag = "true"
	}
	body := `<?xml version="1.0"?><ListHostedZonesResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/"><HostedZones>` +
		strings.Join(zones, "") + "</HostedZones><Marker></Marker><IsTruncated>" + flag + "</IsTruncated><MaxItems>100</MaxItems>"
	if next != "" {
		body += "<NextMarker>" + next + "</NextMarker>"
	}
	return awsReply{Body: body + "</ListHostedZonesResponse>"}
}

func zoneXML(id, name string, private bool) string {
	flag := "false"
	if private {
		flag = "true"
	}
	return "<HostedZone><Id>/hostedzone/" + id + "</Id><Name>" + name + "</Name><CallerReference>r</CallerReference>" +
		"<Config><PrivateZone>" + flag + "</PrivateZone></Config></HostedZone>"
}

func TestFindHostedZonePrefersTheMostSpecificPublicZone(t *testing.T) {
	fake := newFakeAWS(t)
	fake.sequence("route53 GET /2013-04-01/hostedzone",
		hostedZonesPage(true, "page2",
			zoneXML("ZROOT", "example.com.", false),
			zoneXML("ZPRIVATE", "a.vpn.example.com.", true)),
		hostedZonesPage(false, "",
			zoneXML("ZVPN", "vpn.example.com.", false),
			zoneXML("ZLOOKALIKE", "myvpn.example.com.", false),
			zoneXML("ZOTHER", "example.org.", false)))

	zone, err := fake.client().FindHostedZone(context.Background(), "A.VPN.Example.com.")
	if err != nil || zone != "ZVPN" {
		t.Errorf("got %q, %v, want ZVPN", zone, err)
	}
	pages := fake.made("route53 GET /2013-04-01/hostedzone?marker=page2")
	if len(pages) != 1 {
		t.Errorf("the second page was not asked for with its marker: %v", fake.operations())
	}
}

func TestFindHostedZoneMatchesTheApexItself(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("route53 GET /2013-04-01/hostedzone", hostedZonesPage(false, "", zoneXML("ZROOT", "example.com.", false)))

	if zone, err := fake.client().FindHostedZone(context.Background(), "example.com"); err != nil || zone != "ZROOT" {
		t.Errorf("got %q, %v", zone, err)
	}
}

func TestFindHostedZoneExplainsWhenNothingMatches(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("route53 GET /2013-04-01/hostedzone", hostedZonesPage(false, "", zoneXML("ZPRIVATE", "example.com.", true)))
	client := fake.client()

	if _, err := client.FindHostedZone(context.Background(), "vpn.example.com"); err == nil ||
		!strings.Contains(err.Error(), "no public Route53 hosted zone in this account is authoritative for vpn.example.com") {
		t.Errorf("got %v", err)
	}

	fake.reply("route53 GET /2013-04-01/hostedzone", queryError(http.StatusForbidden, "AccessDenied", "no zones"))
	if _, err := client.FindHostedZone(context.Background(), "vpn.example.com"); err == nil || !strings.Contains(err.Error(), "listing hosted zones") {
		t.Errorf("got %v", err)
	}
}

const changeInfo = `<?xml version="1.0"?><ChangeResourceRecordSetsResponse xmlns="https://route53.amazonaws.com/doc/2013-04-01/">` +
	`<ChangeInfo><Id>/change/C1</Id><Status>PENDING</Status><SubmittedAt>2026-01-01T00:00:00Z</SubmittedAt></ChangeInfo>` +
	`</ChangeResourceRecordSetsResponse>`

func TestUpsertAliasPointsTheHostnameAtTheLoadBalancer(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("route53 POST /2013-04-01/hostedzone/ZVPN/rrset", awsReply{Body: changeInfo})

	if err := fake.client().UpsertAlias(context.Background(), "ZVPN", "vpn.example.com", "nlb.amazonaws.com", "ZNLB"); err != nil {
		t.Fatal(err)
	}
	calls := fake.made("route53 POST /2013-04-01/hostedzone/ZVPN/rrset")
	if len(calls) != 1 {
		t.Fatalf("operations: %v", fake.operations())
	}
	for _, want := range []string{
		"<Action>UPSERT</Action>", "<Name>vpn.example.com.</Name>", "<Type>A</Type>",
		"<DNSName>nlb.amazonaws.com</DNSName>", "<HostedZoneId>ZNLB</HostedZoneId>",
		"<EvaluateTargetHealth>false</EvaluateTargetHealth>",
	} {
		if !strings.Contains(calls[0].Body, want) {
			t.Errorf("the change has no %s:\n%s", want, calls[0].Body)
		}
	}
}

func TestDeleteAliasLeavesNothingToDeleteAlone(t *testing.T) {
	fake := newFakeAWS(t)
	fake.sequence("route53 POST /2013-04-01/hostedzone/ZVPN/rrset",
		awsReply{Body: changeInfo},
		queryError(http.StatusBadRequest, "InvalidChangeBatch", "Tried to delete resource record set [name='vpn.example.com.', type='A'] but it was not found"),
		queryError(http.StatusBadRequest, "InvalidChangeBatch", "values provided do not match the current values"))
	client := fake.client()

	if err := client.DeleteAlias(context.Background(), "ZVPN", "vpn.example.com.", "nlb.amazonaws.com", "ZNLB"); err != nil {
		t.Fatal(err)
	}
	body := fake.made("route53 POST /2013-04-01/hostedzone/ZVPN/rrset")[0].Body
	// The trailing dot is not doubled when the caller already wrote one.
	if !strings.Contains(body, "<Action>DELETE</Action>") || !strings.Contains(body, "<Name>vpn.example.com.</Name>") {
		t.Errorf("unexpected change:\n%s", body)
	}
	if err := client.DeleteAlias(context.Background(), "ZVPN", "vpn.example.com", "nlb.amazonaws.com", "ZNLB"); err != nil {
		t.Errorf("a record already gone: %v", err)
	}
	if err := client.DeleteAlias(context.Background(), "ZVPN", "vpn.example.com", "nlb.amazonaws.com", "ZNLB"); err == nil {
		t.Error("a record pointed elsewhere was reported as deleted")
	}
}

func TestChangeAliasNeedsEveryPart(t *testing.T) {
	fake := newFakeAWS(t)
	client := fake.client()
	for _, parts := range [][4]string{
		{"", "vpn.example.com", "nlb", "ZNLB"},
		{"ZVPN", "", "nlb", "ZNLB"},
		{"ZVPN", "vpn.example.com", "", "ZNLB"},
		{"ZVPN", "vpn.example.com", "nlb", ""},
	} {
		if err := client.UpsertAlias(context.Background(), parts[0], parts[1], parts[2], parts[3]); err == nil {
			t.Errorf("%v: accepted", parts)
		}
	}
	if calls := fake.operations(); len(calls) != 0 {
		t.Errorf("incomplete records reached Route53: %v", calls)
	}
}

func TestRegionsListsWhatTheAccountMayUseSorted(t *testing.T) {
	fake := newFakeAWS(t)
	fake.reply("ec2 DescribeRegions", ec2OK("DescribeRegions", "<regionInfo>"+
		"<item><regionName>us-east-1</regionName></item>"+
		"<item><regionName></regionName></item>"+
		"<item><regionName>eu-west-1</regionName></item>"+
		"</regionInfo>"))
	client := fake.client()

	regions, err := client.Regions(context.Background())
	if err != nil || !slices.Equal(regions, []string{"eu-west-1", "us-east-1"}) {
		t.Errorf("got %v, %v", regions, err)
	}

	fake.reply("ec2 DescribeRegions", ec2Error("AuthFailure", "bad key"))
	if _, err := client.Regions(context.Background()); err == nil || !strings.Contains(err.Error(), "bad key") {
		t.Errorf("got %v", err)
	}
}
