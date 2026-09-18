// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	ec2types "github.com/aws/aws-sdk-go-v2/service/ec2/types"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	r53types "github.com/aws/aws-sdk-go-v2/service/route53/types"
)

// Network is everything the template needs to know about the account it is
// being deployed into. It used to be a set of defaults naming one particular
// account's vpc-, subnet- and rtb- ids, which is fine for one deployment and
// useless for a product: the customer owns the account, and nobody is going to
// look these up by hand before running an installer.
type Network struct {
	VpcID          string
	VpcCidr        string
	PublicSubnets  []string
	GatewaySubnets []string
	RouteTableIDs  []string
}

// DiscoverNetwork picks the VPC and the public subnets to deploy into.
//
// "Public" is decided by following each subnet to the route table that governs
// it and looking for a default route at an internet gateway, rather than by
// trusting the MapPublicIpOnLaunch flag: that flag says what happens to an
// instance's addressing, not whether packets can leave.
func (c *Client) DiscoverNetwork(ctx context.Context, vpcID string) (Network, error) {
	vpc, err := c.resolveVPC(ctx, vpcID)
	if err != nil {
		return Network{}, err
	}

	network := Network{
		VpcID:   aws.ToString(vpc.VpcId),
		VpcCidr: aws.ToString(vpc.CidrBlock),
	}

	tables, err := c.EC2.DescribeRouteTables(ctx, &ec2.DescribeRouteTablesInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{network.VpcID}}},
	})
	if err != nil {
		return Network{}, fmt.Errorf("listing route tables: %w", err)
	}

	// A subnet with no explicit association is governed by the VPC's main
	// table, so that one has to be resolved before any subnet can be judged.
	var mainTable *ec2types.RouteTable
	governing := map[string]*ec2types.RouteTable{}
	for index := range tables.RouteTables {
		table := &tables.RouteTables[index]
		for _, association := range table.Associations {
			if aws.ToBool(association.Main) {
				mainTable = table
			}
			if subnet := aws.ToString(association.SubnetId); subnet != "" {
				governing[subnet] = table
			}
		}
	}

	subnets, err := c.EC2.DescribeSubnets(ctx, &ec2.DescribeSubnetsInput{
		Filters: []ec2types.Filter{{Name: aws.String("vpc-id"), Values: []string{network.VpcID}}},
	})
	if err != nil {
		return Network{}, fmt.Errorf("listing subnets: %w", err)
	}

	// One subnet per availability zone. The NLB rejects two subnets in the same
	// zone, and a second one there would buy nothing anyway.
	perZone := map[string]ec2types.Subnet{}
	tableForZone := map[string]string{}
	for _, subnet := range subnets.Subnets {
		table := governing[aws.ToString(subnet.SubnetId)]
		if table == nil {
			table = mainTable
		}
		if table == nil || !reachesInternet(table) {
			continue
		}
		zone := aws.ToString(subnet.AvailabilityZone)
		if existing, taken := perZone[zone]; taken {
			// Deterministic rather than whatever the API returned first, so two
			// runs of the installer produce the same stack parameters and the
			// second one is a no-op.
			if aws.ToString(existing.SubnetId) < aws.ToString(subnet.SubnetId) {
				continue
			}
		}
		perZone[zone] = subnet
		tableForZone[zone] = aws.ToString(table.RouteTableId)
	}

	if len(perZone) == 0 {
		return Network{}, fmt.Errorf("VPC %s has no subnet with a route to an internet gateway; "+
			"the load balancer and the gateway both need one", network.VpcID)
	}

	zones := make([]string, 0, len(perZone))
	for zone := range perZone {
		zones = append(zones, zone)
	}
	sort.Strings(zones)

	seenTable := map[string]bool{}
	for _, zone := range zones {
		network.PublicSubnets = append(network.PublicSubnets, aws.ToString(perZone[zone].SubnetId))
		if table := tableForZone[zone]; !seenTable[table] {
			seenTable[table] = true
			network.RouteTableIDs = append(network.RouteTableIDs, table)
		}
	}

	// The gateway may be rebuilt into any of them. It is still one instance at
	// a time; this only means a dead availability zone is survivable.
	network.GatewaySubnets = network.PublicSubnets

	return network, nil
}

// reachesInternet reports whether a route table sends unmatched traffic at an
// internet gateway. A NAT gateway does not count: the load balancer's nodes
// have to be addressable from outside, not merely able to call out.
func reachesInternet(table *ec2types.RouteTable) bool {
	for _, route := range table.Routes {
		if aws.ToString(route.DestinationCidrBlock) != "0.0.0.0/0" {
			continue
		}
		if strings.HasPrefix(aws.ToString(route.GatewayId), "igw-") {
			return true
		}
	}
	return false
}

func (c *Client) resolveVPC(ctx context.Context, vpcID string) (ec2types.Vpc, error) {
	if vpcID != "" {
		out, err := c.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{VpcIds: []string{vpcID}})
		if err != nil {
			return ec2types.Vpc{}, fmt.Errorf("reading VPC %s: %w", vpcID, err)
		}
		if len(out.Vpcs) == 0 {
			return ec2types.Vpc{}, fmt.Errorf("no VPC %s in %s", vpcID, c.Region)
		}
		return out.Vpcs[0], nil
	}

	out, err := c.EC2.DescribeVpcs(ctx, &ec2.DescribeVpcsInput{})
	if err != nil {
		return ec2types.Vpc{}, fmt.Errorf("listing VPCs: %w", err)
	}
	if len(out.Vpcs) == 0 {
		return ec2types.Vpc{}, fmt.Errorf("no VPC in %s", c.Region)
	}
	for _, vpc := range out.Vpcs {
		if aws.ToBool(vpc.IsDefault) {
			return vpc, nil
		}
	}
	if len(out.Vpcs) == 1 {
		return out.Vpcs[0], nil
	}

	// Ambiguous on purpose rather than guessed: picking the wrong VPC produces
	// a stack that deploys cleanly and serves nothing.
	var names []string
	for _, vpc := range out.Vpcs {
		names = append(names, fmt.Sprintf("%s (%s)", aws.ToString(vpc.VpcId), aws.ToString(vpc.CidrBlock)))
	}
	sort.Strings(names)
	return ec2types.Vpc{}, fmt.Errorf(
		"%s has no default VPC and more than one to choose from; pass --vpc with one of: %s",
		c.Region, strings.Join(names, ", "))
}

// FindHostedZone returns the public zone that is authoritative for a hostname.
//
// The most specific match wins, because delegating a subdomain to its own zone
// is normal: with zones for example.com and for vpn.example.com, a record for
// a.vpn.example.com belongs in the second one, and putting it in the first is a
// record that never resolves.
func (c *Client) FindHostedZone(ctx context.Context, domain string) (string, error) {
	wanted := strings.TrimSuffix(strings.ToLower(domain), ".") + "."

	var (
		best       string
		bestLength int
		marker     *string
	)
	for {
		out, err := c.Route53.ListHostedZones(ctx, &route53.ListHostedZonesInput{Marker: marker})
		if err != nil {
			return "", fmt.Errorf("listing hosted zones: %w", err)
		}
		for _, zone := range out.HostedZones {
			if isPrivate(zone) {
				continue
			}
			name := strings.ToLower(aws.ToString(zone.Name))
			if !strings.HasSuffix(wanted, "."+name) && wanted != name {
				continue
			}
			if len(name) > bestLength {
				bestLength = len(name)
				// The API returns "/hostedzone/Z123"; the template wants "Z123".
				best = strings.TrimPrefix(aws.ToString(zone.Id), "/hostedzone/")
			}
		}
		if !out.IsTruncated {
			break
		}
		marker = out.NextMarker
	}

	if best == "" {
		return "", fmt.Errorf("no public Route53 hosted zone in this account is authoritative for %s; "+
			"create one, or point the hostname at a zone that exists", domain)
	}
	return best, nil
}

func isPrivate(zone r53types.HostedZone) bool {
	return zone.Config != nil && zone.Config.PrivateZone
}
