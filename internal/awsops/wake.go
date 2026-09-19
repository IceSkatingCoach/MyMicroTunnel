// SPDX-License-Identifier: GPL-3.0-or-later
package awsops

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials/stscreds"
	"github.com/aws/aws-sdk-go-v2/service/autoscaling"
	astypes "github.com/aws/aws-sdk-go-v2/service/autoscaling/types"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

// Waking a gateway that switched itself off.
//
// A deployment with an idle timeout spends most of its life at zero
// instances, which is the point of it. The cost is that the first connection
// after a quiet night finds nothing at the other end of the tunnel: the
// endpoint address is still allocated, the DNS record still resolves, and the
// handshake gets no reply.
//
// Nothing in AWS can fix that from the outside, because the thing that would
// notice — a request arriving at the load balancer — is exactly what has
// stopped happening. So the workstation does it: when its tunnel has been
// quiet long enough to be suspicious, it asks the group for one instance back
// and waits for it, and only then re-pins the tunnel.

// AccountID is the twelve digits the default stack name is built from.
func (c *Client) AccountID(ctx context.Context) (string, error) {
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Account), nil
}

// WakeOptions is one deployment's half of the answer. The role is optional:
// without it the caller's own credentials are used, which is what a stack
// deployed before the wake role existed has to fall back on.
type WakeOptions struct {
	RoleARN   string
	GroupName string

	// Wait bounds how long to keep asking. Zero returns as soon as the
	// desired capacity has been set, which is what a caller that only wants
	// to nudge the group — the menu bar — asks for.
	Wait time.Duration

	OnProgress func(string)
}

// WakeGateway brings the gateway back and, when asked to, waits until it is
// in service.
//
// It is safe to call on a gateway that is already running: setting the desired
// capacity to the value it already holds is not an error, and the group is
// never asked for more than one instance, so a race between the menu bar and
// the supervisor cannot produce two gateways fighting over one Elastic IP.
func (c *Client) WakeGateway(ctx context.Context, options WakeOptions) error {
	if options.GroupName == "" {
		return errors.New("no Auto Scaling group: this deployment predates the idle timeout, or its outputs were not recorded")
	}
	progress := options.OnProgress
	if progress == nil {
		progress = func(string) {}
	}

	scaling, err := c.autoScaling(ctx, options.RoleARN)
	if err != nil {
		return err
	}

	inService, desired, err := groupState(ctx, scaling, options.GroupName)
	if err != nil {
		return err
	}
	if inService > 0 {
		progress("the gateway is already running")
		return nil
	}
	if desired < 1 {
		if _, err := scaling.SetDesiredCapacity(ctx, &autoscaling.SetDesiredCapacityInput{
			AutoScalingGroupName: aws.String(options.GroupName),
			DesiredCapacity:      aws.Int32(1),
			// Honoured rather than skipped: the scale-to-zero policy has a
			// cooldown, and jumping it would let a wake race the alarm that
			// has just fired.
			HonorCooldown: aws.Bool(false),
		}); err != nil {
			return fmt.Errorf("asking for a gateway instance: %w", err)
		}
	}
	progress("asked for a gateway instance")

	if options.Wait == 0 {
		return nil
	}

	deadline := time.Now().Add(options.Wait)
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}

		inService, _, err := groupState(ctx, scaling, options.GroupName)
		if err != nil {
			return err
		}
		if inService > 0 {
			progress("the gateway is in service")
			// In service is not the same as serving: the boot script still has
			// to claim the Elastic IP, write the route and apply the peers.
			// Whoever called this re-pins the tunnel afterwards, and the
			// handshake is what actually decides.
			return nil
		}
		progress("waiting for the gateway to boot")
	}
	return fmt.Errorf("the gateway did not come back within %s", options.Wait)
}

// autoScaling builds the client the wake runs through: the narrow role when
// the stack published one, and the caller's own credentials when it did not.
//
// The role exists so a laptop that wakes a gateway several times a day is not
// carrying a credential that could also delete the deployment. Falling back is
// deliberate and is reported by the caller rather than hidden: a stack from
// before the role existed should still be wakeable.
func (c *Client) autoScaling(ctx context.Context, roleARN string) (*autoscaling.Client, error) {
	if roleARN == "" {
		return autoscaling.NewFromConfig(c.cfg), nil
	}

	assumed := c.cfg.Copy()
	assumed.Credentials = aws.NewCredentialsCache(
		stscreds.NewAssumeRoleProvider(c.STS, roleARN, func(options *stscreds.AssumeRoleOptions) {
			options.RoleSessionName = "mymicrotunnel-wake"
		}),
	)

	// Proved here rather than at the first call, so "the role cannot be
	// assumed" is distinguishable from "the group could not be scaled".
	if _, err := assumed.Credentials.Retrieve(ctx); err != nil {
		if strings.Contains(err.Error(), "AccessDenied") {
			return nil, fmt.Errorf("this AWS profile may not assume %s: %w", roleARN, err)
		}
		return nil, fmt.Errorf("assuming %s: %w", roleARN, err)
	}
	return autoscaling.NewFromConfig(assumed), nil
}

func groupState(ctx context.Context, scaling *autoscaling.Client, name string) (inService, desired int32, err error) {
	out, err := scaling.DescribeAutoScalingGroups(ctx, &autoscaling.DescribeAutoScalingGroupsInput{
		AutoScalingGroupNames: []string{name},
	})
	if err != nil {
		return 0, 0, fmt.Errorf("reading the Auto Scaling group: %w", err)
	}
	if len(out.AutoScalingGroups) == 0 {
		return 0, 0, fmt.Errorf("no Auto Scaling group called %s", name)
	}

	group := out.AutoScalingGroups[0]
	for _, instance := range group.Instances {
		if instance.LifecycleState == astypes.LifecycleStateInService {
			inService++
		}
	}
	return inService, aws.ToInt32(group.DesiredCapacity), nil
}

// TrustablePrincipal turns whatever GetCallerIdentity reported into something
// an IAM trust policy can name for longer than an hour.
//
// A profile that signs in through IAM Identity Center authenticates as
// arn:aws:sts::123456789012:assumed-role/AWSReservedSSO_Admin_abc/user@example.com.
// Putting that in a trust policy is a role nobody can assume tomorrow: the
// session name changes on every sign-in. The role behind it is stable, so that
// is what gets trusted.
//
// Anything that is not an assumed-role session — an IAM user, a role ARN — is
// already stable and is returned unchanged.
func TrustablePrincipal(callerArn string) string {
	const marker = ":assumed-role/"
	index := strings.Index(callerArn, marker)
	if index < 0 {
		return callerArn
	}

	account := strings.Split(callerArn, ":")
	if len(account) < 5 {
		return callerArn
	}
	rest := callerArn[index+len(marker):]
	role, _, found := strings.Cut(rest, "/")
	if !found || role == "" {
		return callerArn
	}
	// The partition is taken from the caller's own ARN rather than assumed to
	// be "aws": a GovCloud or China deployment reads arn:aws-us-gov: here.
	return fmt.Sprintf("arn:%s:iam::%s:role/%s", account[1], account[4], role)
}
