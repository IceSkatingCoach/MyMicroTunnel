// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	elbtypes "github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2/types"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// Peer is one workstation: the public half of its WireGuard key, the tunnel
// address it answers on, and a name for whoever has to read the list later.
type Peer struct {
	PublicKey string `json:"publicKey"`
	Address   string `json:"address"`
	Label     string `json:"label,omitempty"`
}

// Peers reads the registry the gateway reconciles against. A missing parameter
// is an empty list rather than an error: that is what a stack looks like
// between being deployed and having its first workstation registered.
func (c *Client) Peers(ctx context.Context, parameter string) ([]Peer, error) {
	out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(parameter)})
	if err != nil {
		if strings.Contains(err.Error(), "ParameterNotFound") {
			return nil, nil
		}
		return nil, err
	}

	var peers []Peer
	raw := aws.ToString(out.Parameter.Value)
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	if err := json.Unmarshal([]byte(raw), &peers); err != nil {
		return nil, fmt.Errorf("%s does not hold a peer list: %w", parameter, err)
	}
	return peers, nil
}

// PutPeers replaces the registry wholesale. The gateway's timer applies it
// within a minute, so no instance is replaced and no stack is updated.
func (c *Client) PutPeers(ctx context.Context, parameter string, peers []Peer) error {
	sort.Slice(peers, func(left, right int) bool { return peers[left].Address < peers[right].Address })

	encoded, err := json.Marshal(peers)
	if err != nil {
		return err
	}
	_, err = c.SSM.PutParameter(ctx, &ssm.PutParameterInput{
		Name:        aws.String(parameter),
		Type:        "String",
		Value:       aws.String(string(encoded)),
		Overwrite:   aws.Bool(true),
		Description: aws.String("WireGuard peers reconciled by the gateway. Managed by wiregard-mini-vpn."),
	})
	return err
}

// UpsertPeer adds or updates one workstation and returns the whole list.
//
// A tunnel address identifies a workstation, not a key: re-running the
// installer after regenerating a key has to replace that machine's entry rather
// than leave a second peer holding the same address, which the gateway would
// resolve by handing the address to whichever of them handshook last.
func (c *Client) UpsertPeer(ctx context.Context, parameter string, peer Peer) ([]Peer, error) {
	existing, err := c.Peers(ctx, parameter)
	if err != nil {
		return nil, err
	}

	updated := MergePeer(existing, peer)
	if err := c.PutPeers(ctx, parameter, updated); err != nil {
		return nil, err
	}
	return updated, nil
}

// MergePeer is the rule UpsertPeer applies, kept separate from the API call so
// it can be reasoned about — and tested — without an AWS account.
//
// Both the address and the key identify a workstation. Matching on the address
// alone would leave a stale key behind after a machine regenerated one;
// matching on the key alone would leave two peers claiming one address, which
// the gateway resolves in favour of whichever handshook last.
func MergePeer(existing []Peer, peer Peer) []Peer {
	updated := make([]Peer, 0, len(existing)+1)
	for _, candidate := range existing {
		if candidate.Address == peer.Address || candidate.PublicKey == peer.PublicKey {
			continue
		}
		updated = append(updated, candidate)
	}
	return append(updated, peer)
}

// RemovePeer takes one workstation out of the registry, leaving the rest of the
// deployment serving.
func (c *Client) RemovePeer(ctx context.Context, parameter, address string) error {
	existing, err := c.Peers(ctx, parameter)
	if err != nil {
		return err
	}

	kept := WithoutPeer(existing, address)
	if len(kept) == len(existing) {
		return nil
	}
	return c.PutPeers(ctx, parameter, kept)
}

// WithoutPeer drops the workstation at one tunnel address and keeps the rest.
func WithoutPeer(existing []Peer, address string) []Peer {
	kept := make([]Peer, 0, len(existing))
	for _, candidate := range existing {
		if candidate.Address != address {
			kept = append(kept, candidate)
		}
	}
	return kept
}

// --- target registration ---------------------------------------------------

// RegisterTarget puts one workstation behind the load balancer.
//
// AvailabilityZone "all" is what marks a target as living outside the VPC,
// reached over the VPN route rather than by sitting in one of the subnets. The
// registration is an API call rather than a template property because the
// number of workstations is not known when the stack is written.
func (c *Client) RegisterTarget(ctx context.Context, targetGroupARN, address string, port int32) error {
	_, err := c.ELB.RegisterTargets(ctx, &elasticloadbalancingv2.RegisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               aws.String(address),
			Port:             aws.Int32(port),
			AvailabilityZone: aws.String("all"),
		}},
	})
	return err
}

func (c *Client) DeregisterTarget(ctx context.Context, targetGroupARN, address string, port int32) error {
	_, err := c.ELB.DeregisterTargets(ctx, &elasticloadbalancingv2.DeregisterTargetsInput{
		TargetGroupArn: aws.String(targetGroupARN),
		Targets: []elbtypes.TargetDescription{{
			Id:               aws.String(address),
			Port:             aws.Int32(port),
			AvailabilityZone: aws.String("all"),
		}},
	})
	if err != nil && strings.Contains(err.Error(), "InvalidTarget") {
		return nil
	}
	return err
}

// TargetHealthOf reports on one address rather than on whichever target the API
// happened to list first, which with several workstations registered would be a
// coin toss.
func (c *Client) TargetHealthOf(ctx context.Context, targetGroupARN, address string) (string, error) {
	out, err := c.ELB.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(targetGroupARN),
	})
	if err != nil {
		return "", err
	}
	for _, description := range out.TargetHealthDescriptions {
		if description.Target == nil || aws.ToString(description.Target.Id) != address {
			continue
		}
		if description.TargetHealth == nil {
			return "unknown", nil
		}
		state := string(description.TargetHealth.State)
		if reason := aws.ToString(description.TargetHealth.Description); reason != "" && state != "healthy" {
			return state + " (" + reason + ")", nil
		}
		return state, nil
	}
	return "not registered", nil
}

// --- cleanup ---------------------------------------------------------------

// DeleteParametersByPath removes the parameters the gateway writes for itself.
//
// They are not CloudFormation resources — the instance creates them, which is
// the only way the server key can outlive the instance that generated it — so
// deleting the stack leaves them behind. An uninstall that claims to have
// removed the deployment has to remove these too.
func (c *Client) DeleteParametersByPath(ctx context.Context, path string) error {
	var names []string
	var next *string
	for {
		out, err := c.SSM.GetParametersByPath(ctx, &ssm.GetParametersByPathInput{
			Path:      aws.String(path),
			Recursive: aws.Bool(true),
			NextToken: next,
		})
		if err != nil {
			if strings.Contains(err.Error(), "ParameterNotFound") {
				return nil
			}
			return err
		}
		for _, parameter := range out.Parameters {
			names = append(names, aws.ToString(parameter.Name))
		}
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}

	// DeleteParameters takes ten at a time.
	for start := 0; start < len(names); start += 10 {
		end := start + 10
		if end > len(names) {
			end = len(names)
		}
		if _, err := c.SSM.DeleteParameters(ctx, &ssm.DeleteParametersInput{Names: names[start:end]}); err != nil {
			return err
		}
	}
	return nil
}
