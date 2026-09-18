// SPDX-License-Identifier: GPL-3.0-or-later
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
	"github.com/aws/aws-sdk-go-v2/service/cloudfront"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/elasticloadbalancingv2"
	"github.com/aws/aws-sdk-go-v2/service/route53"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	"github.com/aws/aws-sdk-go-v2/service/sts"
)

type Client struct {
	Region  string
	CFN     *cloudformation.Client
	SSM     *ssm.Client
	ELB     *elasticloadbalancingv2.Client
	STS     *sts.Client
	EC2     *ec2.Client
	Route53 *route53.Client

	// Only the publishing side uses these; a customer install never touches
	// either service.
	S3         *s3.Client
	CloudFront *cloudfront.Client
}

func newClient(cfg aws.Config) *Client {
	return &Client{
		Region: cfg.Region,
		CFN:    cloudformation.NewFromConfig(cfg),
		SSM:    ssm.NewFromConfig(cfg),
		ELB:    elasticloadbalancingv2.NewFromConfig(cfg),
		STS:    sts.NewFromConfig(cfg),
		EC2:    ec2.NewFromConfig(cfg),
		// Route53 is global; its endpoint lives in us-east-1 whatever region
		// the rest of the deployment is in.
		Route53: route53.NewFromConfig(cfg, func(options *route53.Options) {
			options.Region = "us-east-1"
		}),
		S3: s3.NewFromConfig(cfg),
		// CloudFront is global, like Route53.
		CloudFront: cloudfront.NewFromConfig(cfg, func(options *cloudfront.Options) {
			options.Region = "us-east-1"
		}),
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

// IsSSOProfile reports whether a profile authenticates through IAM Identity
// Center rather than a stored key pair.
//
// The SDK handles both without being told which is which. The difference
// matters only for what to say when it fails: a static key that stops working
// has been deleted or disabled, while an SSO session simply expires — routinely,
// every few hours — and the fix is one command rather than a new credential.
func IsSSOProfile(profile string) bool {
	home, err := os.UserHomeDir()
	if err != nil {
		return false
	}
	content, err := os.ReadFile(filepath.Join(home, ".aws", "config"))
	if err != nil {
		return false
	}

	inProfile := false
	for _, line := range strings.Split(string(content), "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			name := strings.TrimSuffix(strings.TrimPrefix(trimmed, "["), "]")
			inProfile = strings.TrimPrefix(name, "profile ") == profile
			continue
		}
		if !inProfile {
			continue
		}
		// Either spelling: the legacy sso_ keys or a reference to an
		// [sso-session] block.
		if strings.HasPrefix(trimmed, "sso_") || strings.HasPrefix(trimmed, "sso-session") {
			return true
		}
	}
	return false
}

// ExplainCredentialFailure turns an SDK error into something actionable.
//
// "operation error STS: GetCallerIdentity, get identity: get credentials" tells
// somebody who already knows the answer what they already knew. The two cases
// worth distinguishing are an expired SSO session, which is normal and fixed in
// one command, and a key that no longer works, which is not.
func ExplainCredentialFailure(profile string, err error) string {
	message := err.Error()

	expired := strings.Contains(message, "expired") ||
		strings.Contains(message, "InvalidGrantException") ||
		strings.Contains(message, "the SSO session has expired") ||
		strings.Contains(message, "ForbiddenException")

	if IsSSOProfile(profile) {
		if expired {
			return fmt.Sprintf("the IAM Identity Center session for %q has expired.\n\n"+
				"  Sign in again and re-run this:\n\n"+
				"      aws sso login --profile %s", profile, profile)
		}
		return fmt.Sprintf("%q is an IAM Identity Center profile and it did not work: %v\n\n"+
			"  Try `aws sso login --profile %s` first.", profile, err, profile)
	}

	if expired {
		return fmt.Sprintf("the credentials for %q have expired: %v", profile, err)
	}
	return fmt.Sprintf("the credentials for %q do not work: %v\n\n"+
		"  Check the key is still active in IAM, and that it belongs to the account\n"+
		"  holding the hosted zone for this hostname.", profile, err)
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

	// Only parameters this template declares may be sent. An older stack can
	// hold parameters a newer template has dropped, and CloudFormation rejects
	// the whole change set for one of those rather than ignoring it.
	declared, err := c.declaredParameters(ctx, templateBody)
	if err != nil {
		return err
	}

	var input []cfntypes.Parameter
	for key, value := range parameters {
		if !declared[key] {
			continue
		}
		input = append(input, cfntypes.Parameter{
			ParameterKey:   aws.String(key),
			ParameterValue: aws.String(value),
		})
	}
	for key := range carried {
		if _, overridden := parameters[key]; overridden {
			continue
		}
		if !declared[key] {
			continue
		}
		input = append(input, cfntypes.Parameter{
			ParameterKey:     aws.String(key),
			UsePreviousValue: aws.Bool(true),
		})
	}
	// Sorted so two runs with the same inputs produce the same change set, and
	// "no changes" means what it says.
	sort.Slice(input, func(left, right int) bool {
		return aws.ToString(input[left].ParameterKey) < aws.ToString(input[right].ParameterKey)
	})

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

// declaredParameters asks CloudFormation what the template accepts, rather than
// parsing the YAML here: the answer has to match what the service will accept,
// not what a second parser thinks the file says.
func (c *Client) declaredParameters(ctx context.Context, templateBody string) (map[string]bool, error) {
	out, err := c.CFN.GetTemplateSummary(ctx, &cloudformation.GetTemplateSummaryInput{
		TemplateBody: aws.String(templateBody),
	})
	if err != nil {
		return nil, fmt.Errorf("reading the template's parameters: %w", err)
	}
	declared := map[string]bool{}
	for _, parameter := range out.Parameters {
		declared[aws.ToString(parameter.ParameterKey)] = true
	}
	return declared, nil
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

// StackOutputs reads every output at once. The installer needs five of them,
// and five DescribeStacks calls to answer one question is five chances to be
// throttled.
func (c *Client) StackOutputs(ctx context.Context, stackName string) (map[string]string, error) {
	out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return nil, err
	}
	outputs := map[string]string{}
	for _, stack := range out.Stacks {
		for _, output := range stack.Outputs {
			outputs[aws.ToString(output.OutputKey)] = aws.ToString(output.OutputValue)
		}
	}
	return outputs, nil
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

// StackParameter reads one parameter off the deployed stack, so a caller can
// tell whether it is about to change it.
func (c *Client) StackParameter(ctx context.Context, stackName, key string) (string, bool) {
	out, err := c.CFN.DescribeStacks(ctx, &cloudformation.DescribeStacksInput{StackName: aws.String(stackName)})
	if err != nil {
		return "", false
	}
	for _, stack := range out.Stacks {
		for _, parameter := range stack.Parameters {
			if aws.ToString(parameter.ParameterKey) == key {
				return aws.ToString(parameter.ParameterValue), true
			}
		}
	}
	return "", false
}

func (c *Client) DeleteParameter(ctx context.Context, name string) error {
	_, err := c.SSM.DeleteParameter(ctx, &ssm.DeleteParameterInput{Name: aws.String(name)})
	if err != nil && strings.Contains(err.Error(), "ParameterNotFound") {
		return nil
	}
	return err
}

// ProfileRegion reads the region a profile already names, so the installer does
// not have to ask for something the user has configured once already. Empty
// when the profile sets none.
func ProfileRegion(ctx context.Context, profile string) string {
	cfg, err := config.LoadDefaultConfig(ctx, config.WithSharedConfigProfile(profile))
	if err != nil {
		return ""
	}
	return cfg.Region
}
