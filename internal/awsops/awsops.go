// Package awsops is every AWS call the installer makes. It uses the SDK
// directly rather than shelling out to the AWS CLI, so the installed product
// has no dependency the user has to install first.
package awsops

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/cloudformation"
	cfntypes "github.com/aws/aws-sdk-go-v2/service/cloudformation/types"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type Client struct {
	Region string
	CFN    *cloudformation.Client
	SSM    *ssm.Client
	ELB    *elasticloadbalancingv2.Client
	STS    *sts.Client
}

func newClient(cfg aws.Config) *Client {
	return &Client{
		Region: cfg.Region,
		CFN:    cloudformation.NewFromConfig(cfg),
		SSM:    ssm.NewFromConfig(cfg),
		ELB:    elasticloadbalancingv2.NewFromConfig(cfg),
		STS:    sts.NewFromConfig(cfg),
	}
}

func LoadProfile(ctx context.Context, profile, region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return nil, err
	}
	return newClient(cfg), nil
}

func LoadStatic(ctx context.Context, accessKeyID, secretAccessKey, region string) (*Client, error) {
	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion(region),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(accessKeyID, secretAccessKey, ""),
		),
	)
	if err != nil {
		return nil, err
	}
	return newClient(cfg), nil
}

func (c *Client) Identity(ctx context.Context) (string, error) {
	out, err := c.STS.GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", err
	}
	return aws.ToString(out.Arn), nil
}

// --- shared config ---------------------------------------------------------

// Profiles reads the names out of both shared files. The SDK has no public API
// for listing them, and parsing two ini files is cheaper than depending on one.
func Profiles() []string {
	home, err := os.UserHomeDir()
	if err != nil {
		return nil
	}

	seen := map[string]bool{}
	for _, path := range []string{
		filepath.Join(home, ".aws", "credentials"),
		filepath.Join(home, ".aws", "config"),
	} {
		content, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(content), "\n") {
			line = strings.TrimSpace(line)
			if !strings.HasPrefix(line, "[") || !strings.HasSuffix(line, "]") {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(line, "["), "]")
			// ~/.aws/config spells them "profile NAME", except for "default".
			name = strings.TrimPrefix(name, "profile ")
			if name != "" {
				seen[name] = true
			}
		}
	}

	names := make([]string, 0, len(seen))
	for name := range seen {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// WriteProfile adds or replaces one profile in ~/.aws/credentials, leaving
// every other section untouched.
func WriteProfile(profile, accessKeyID, secretAccessKey, region string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	directory := filepath.Join(home, ".aws")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return err
	}
	path := filepath.Join(directory, "credentials")

	existing, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	var kept []string
	inTarget := false
	for _, line := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			inTarget = trimmed == "["+profile+"]"
		}
		if !inTarget {
			kept = append(kept, line)
		}
	}

	body := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if body != "" {
		body += "\n\n"
	}
	body += fmt.Sprintf(
		"[%s]\naws_access_key_id = %s\naws_secret_access_key = %s\nregion = %s\n",
		profile, accessKeyID, secretAccessKey, region,
	)

	// 0600: this file holds a long-lived secret.
	return os.WriteFile(path, []byte(body), 0o600)
}

// --- CloudFormation --------------------------------------------------------

func (c *Client) stackExists(ctx context.Context, name string) (bool, error) {
	out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		if strings.Contains(err.Error(), "does not exist") {
			return false, nil
		}
		return false, err
	}
	for _, stack := range out.Stacks {
		// A stack left in REVIEW_IN_PROGRESS has a change set but was never
		// executed, so it still needs the CREATE path.
		if stack.StackStatus == cfntypes.StackStatusReviewInProgress {
			return false, nil
		}
	}
	return true, nil
}

// existingParameterKeys is what makes an update safe to re-run. Parameters the
// caller does not set are carried over with UsePreviousValue instead of falling
// back to the template default — otherwise the SSM-resolved AMI parameter would
// pick up a newer image and replace the gateway on every deploy.
func (c *Client) existingParameterKeys(ctx context.Context, name string) (map[string]bool, error) {
	out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
	if err != nil {
		return nil, err
	}
	keys := map[string]bool{}
	for _, stack := range out.Stacks {
		for _, parameter := range stack.Parameters {
			keys[aws.ToString(parameter.ParameterKey)] = true
		}
	}
	return keys, nil
}

// DeployStack is the equivalent of `aws cloudformation deploy`: create a change
// set, wait for it, execute it, and wait for the stack. Reports "no changes" as
// success, because a re-run of the installer is expected to be a no-op.
func (c *Client) DeployStack(ctx context.Context, name, templateBody string, parameters map[string]string, onProgress func(string)) error {
	exists, err := c.stackExists(ctx, name)
	if err != nil {
		return err
	}

	changeSetType := cfntypes.ChangeSetTypeCreate
	carried := map[string]bool{}
	if exists {
		changeSetType = cfntypes.ChangeSetTypeUpdate
		if carried, err = c.existingParameterKeys(ctx, name); err != nil {
			return err
		}
	}

	var input []cfntypes.Parameter
	for key, value := range parameters {
		input = append(input, cfntypes.Parameter{
			ParameterKey:   aws.String(key),
			ParameterValue: aws.String(value),
		})
	}
	for key := range carried {
		if _, overridden := parameters[key]; overridden {
			continue
		}
		input = append(input, cfntypes.Parameter{
			ParameterKey:     aws.String(key),
			UsePreviousValue: aws.Bool(true),
		})
	}

	changeSetName := fmt.Sprintf("wiregard-%d", time.Now().Unix())
	if _, err := c.CFN.CreateChangeSet(ctx, &cloudformation.CreateChangeSetInput{
		StackName:     aws.String(name),
		ChangeSetName: aws.String(changeSetName),
		ChangeSetType: changeSetType,
		TemplateBody:  aws.String(templateBody),
		Parameters:    input,
		Capabilities:  []cfntypes.Capability{cfntypes.CapabilityCapabilityIam},
	}); err != nil {
		return fmt.Errorf("creating the change set: %w", err)
	}

	empty, err := c.waitForChangeSet(ctx, name, changeSetName)
	if err != nil {
		return err
	}
	if empty {
		_, _ = c.CFN.DeleteChangeSet(ctx, &cloudformation.DeleteChangeSetInput{
			StackName:     aws.String(name),
			ChangeSetName: aws.String(changeSetName),
		})
		onProgress("No changes to apply")
		return nil
	}

	if _, err := c.CFN.ExecuteChangeSet(ctx, &cloudformation.ExecuteChangeSetInput{
		StackName:     aws.String(name),
		ChangeSetName: aws.String(changeSetName),
	}); err != nil {
		return fmt.Errorf("executing the change set: %w", err)
	}

	return c.waitForStack(ctx, name, onProgress)
}

func (c *Client) waitForChangeSet(ctx context.Context, stackName, changeSetName string) (bool, error) {
	for attempt := 0; attempt < 120; attempt++ {
		out, err := c.CFN.DescribeChangeSet(ctx, &cloudformation.DescribeChangeSetInput{
			StackName:     aws.String(stackName),
			ChangeSetName: aws.String(changeSetName),
		})
		if err != nil {
			return false, err
		}

		switch out.Status {
		case cfntypes.ChangeSetStatusCreateComplete:
			return false, nil
		case cfntypes.ChangeSetStatusFailed:
			reason := aws.ToString(out.StatusReason)
			// CloudFormation reports "nothing to do" as a failure; it is not one.
			if strings.Contains(reason, "didn't contain changes") || strings.Contains(reason, "No updates are to be performed") {
				return true, nil
			}
			return false, fmt.Errorf("the change set failed: %s", reason)
		}

		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
	return false, errors.New("timed out waiting for the change set")
}

func (c *Client) waitForStack(ctx context.Context, name string, onProgress func(string)) error {
	reported := map[string]bool{}

	for attempt := 0; attempt < 360; attempt++ {
		out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
		if err != nil {
			return err
		}
		if len(out.Stacks) == 0 {
			return fmt.Errorf("stack %s disappeared", name)
		}

		status := out.Stacks[0].StackStatus
		for _, resource := range c.recentResourceStatuses(ctx, name) {
			if !reported[resource] {
				reported[resource] = true
				onProgress(resource)
			}
		}

		switch status {
		case cfntypes.StackStatusCreateComplete, cfntypes.StackStatusUpdateComplete:
			return nil
		case cfntypes.StackStatusRollbackComplete,
			cfntypes.StackStatusRollbackFailed,
			cfntypes.StackStatusCreateFailed,
			cfntypes.StackStatusUpdateRollbackComplete,
			cfntypes.StackStatusUpdateRollbackFailed,
			cfntypes.StackStatusDeleteFailed:
			return fmt.Errorf("the stack ended in %s:\n%s", status, strings.Join(c.FailureReasons(ctx, name), "\n"))
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return errors.New("timed out waiting for the stack")
}

func (c *Client) recentResourceStatuses(ctx context.Context, name string) []string {
	out, err := c.CFN.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: aws.String(name)})
	if err != nil {
		return nil
	}

	var lines []string
	for _, event := range out.StackEvents {
		if event.ResourceStatus != cfntypes.ResourceStatusCreateComplete &&
			event.ResourceStatus != cfntypes.ResourceStatusUpdateComplete {
			continue
		}
		logical := aws.ToString(event.LogicalResourceId)
		if logical == name {
			continue
		}
		lines = append(lines, logical)
	}
	// Oldest first, so progress reads in the order it happened.
	for left, right := 0, len(lines)-1; left < right; left, right = left+1, right-1 {
		lines[left], lines[right] = lines[right], lines[left]
	}
	return lines
}

func (c *Client) FailureReasons(ctx context.Context, name string) []string {
	out, err := c.CFN.DescribeStackEvents(ctx, &cloudformation.DescribeStackEventsInput{StackName: aws.String(name)})
	if err != nil {
		return []string{err.Error()}
	}

	var reasons []string
	for _, event := range out.StackEvents {
		if !strings.Contains(string(event.ResourceStatus), "FAILED") {
			continue
		}
		reasons = append(reasons, fmt.Sprintf("  %s: %s",
			aws.ToString(event.LogicalResourceId), aws.ToString(event.ResourceStatusReason)))
		if len(reasons) == 5 {
			break
		}
	}
	return reasons
}

func (c *Client) StackOutput(ctx context.Context, stackName, key string) (string, error) {
	out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return "", err
	}
	for _, stack := range out.Stacks {
		for _, output := range stack.Outputs {
			if aws.ToString(output.OutputKey) == key {
				return aws.ToString(output.OutputValue), nil
			}
		}
	}
	return "", fmt.Errorf("stack %s has no output %s", stackName, key)
}

func (c *Client) DeleteStack(ctx context.Context, name string) error {
	if _, err := c.CFN.DeleteStack(ctx, &cloudformation.DeleteStackInput{StackName: aws.String(name)}); err != nil {
		return err
	}
	for attempt := 0; attempt < 180; attempt++ {
		_, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(name)})
		if err != nil && strings.Contains(err.Error(), "does not exist") {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return errors.New("timed out waiting for the stack to delete")
}

// --- SSM and ELB -----------------------------------------------------------

// WaitForParameter polls for the public key the gateway publishes at the end of
// its boot script. Its arrival is the only signal that the instance finished
// configuring itself.
func (c *Client) WaitForParameter(ctx context.Context, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		out, err := c.SSM.GetParameter(ctx, &ssm.GetParameterInput{Name: aws.String(name)})
		if err == nil && out.Parameter != nil {
			return aws.ToString(out.Parameter.Value), nil
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(10 * time.Second):
		}
	}
	return "", fmt.Errorf("the gateway never published %s", name)
}

func (c *Client) TargetGroupARN(ctx context.Context, nameContains string) (string, error) {
	out, err := c.ELB.DescribeTargetGroups(ctx, &elasticloadbalancingv2.DescribeTargetGroupsInput{})
	if err != nil {
		return "", err
	}
	for _, group := range out.TargetGroups {
		if strings.Contains(aws.ToString(group.TargetGroupName), nameContains) {
			return aws.ToString(group.TargetGroupArn), nil
		}
	}
	return "", fmt.Errorf("no target group matching %q", nameContains)
}

func (c *Client) TargetHealth(ctx context.Context, targetGroupARN string) (string, error) {
	out, err := c.ELB.DescribeTargetHealth(ctx, &elasticloadbalancingv2.DescribeTargetHealthInput{
		TargetGroupArn: aws.String(targetGroupARN),
	})
	if err != nil {
		return "", err
	}
	if len(out.TargetHealthDescriptions) == 0 {
		return "no targets registered", nil
	}
	return string(out.TargetHealthDescriptions[0].TargetHealth.State), nil
}
