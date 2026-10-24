package workspacestorage

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

const (
	workspaceExecutionRolePath       = "/hako/workspaces/"
	workspaceExecutionRolePolicyName = "hako-workspace-efs"
)

var accountIDPattern = regexp.MustCompile(`^[0-9]{12}$`)

type IAMAPI interface {
	CreateRole(context.Context, *iam.CreateRoleInput, ...func(*iam.Options)) (*iam.CreateRoleOutput, error)
	GetRole(context.Context, *iam.GetRoleInput, ...func(*iam.Options)) (*iam.GetRoleOutput, error)
	PutRolePolicy(context.Context, *iam.PutRolePolicyInput, ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error)
	GetRolePolicy(context.Context, *iam.GetRolePolicyInput, ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error)
	ListRolePolicies(context.Context, *iam.ListRolePoliciesInput, ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error)
	ListAttachedRolePolicies(context.Context, *iam.ListAttachedRolePoliciesInput, ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error)
	DeleteRolePolicy(context.Context, *iam.DeleteRolePolicyInput, ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error)
	DeleteRole(context.Context, *iam.DeleteRoleInput, ...func(*iam.Options)) (*iam.DeleteRoleOutput, error)
}

type WorkspaceExecutionRole struct {
	Name string
	ARN  string
}

type ExecutionRoleProvider struct {
	api       IAMAPI
	partition string
	accountID string
}

func NewExecutionRoleProvider(api IAMAPI, partition, accountID string) (*ExecutionRoleProvider, error) {
	if api == nil || (partition != "aws" && partition != "aws-us-gov" && partition != "aws-cn") || !accountIDPattern.MatchString(accountID) {
		return nil, errors.New("IAM API, supported AWS partition, and account ID are required")
	}
	return &ExecutionRoleProvider{api: api, partition: partition, accountID: accountID}, nil
}

// Ensure creates or verifies the deterministic, Workspace-scoped Lambda
// MicroVM execution role. It never repairs an existing foreign or drifted role
// by broadening permissions; those cases fail closed for operator review.
func (p *ExecutionRoleProvider) Ensure(ctx context.Context, identity Identity, binding Binding) (WorkspaceExecutionRole, error) {
	if p == nil || p.api == nil || ctx == nil || ctx.Err() != nil || binding.ValidateFor(identity) != nil {
		return WorkspaceExecutionRole{}, errors.New("Workspace execution role identity or Access Point binding is invalid")
	}
	name, err := ExecutionRoleName(identity)
	if err != nil {
		return WorkspaceExecutionRole{}, err
	}
	fileSystemARN := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:file-system/%s", p.partition, binding.Region, p.accountID, binding.FileSystemID)
	accessPointARN := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:access-point/%s", p.partition, binding.Region, p.accountID, binding.AccessPointID)
	permissions, err := ExecutionRolePermissionsPolicy(fileSystemARN, accessPointARN)
	if err != nil {
		return WorkspaceExecutionRole{}, err
	}
	trust, err := ExecutionRoleTrustPolicy()
	if err != nil {
		return WorkspaceExecutionRole{}, err
	}
	tags := expectedWorkspaceRoleTags(identity)
	role, created, err := p.getOrCreate(ctx, name, trust, tags)
	if err != nil {
		return WorkspaceExecutionRole{}, err
	}
	if err := p.verifyRole(ctx, role, name, trust, tags, permissions, false); err != nil {
		if created {
			if cleanupErr := p.compensateNewRole(ctx, name); cleanupErr != nil {
				return WorkspaceExecutionRole{}, fmt.Errorf("Workspace role verification failed and new-role compensation failed: %w", cleanupErr)
			}
		}
		return WorkspaceExecutionRole{}, err
	}
	if created {
		if _, err := p.api.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(workspaceExecutionRolePolicyName), PolicyDocument: aws.String(permissions)}); err != nil {
			if cleanupErr := p.compensateNewRole(ctx, name); cleanupErr != nil {
				return WorkspaceExecutionRole{}, fmt.Errorf("attach Workspace role policy failed and role compensation failed: %w", cleanupErr)
			}
			return WorkspaceExecutionRole{}, errors.New("attach Workspace execution role policy")
		}
	} else if err := p.ensureExactPolicy(ctx, name, permissions); err != nil {
		return WorkspaceExecutionRole{}, err
	}
	roleARN := aws.ToString(role.Arn)
	if roleARN != p.roleARN(name) {
		return WorkspaceExecutionRole{}, errors.New("IAM returned an unexpected Workspace execution role ARN")
	}
	return WorkspaceExecutionRole{Name: name, ARN: roleARN}, nil
}

func (p *ExecutionRoleProvider) compensateNewRole(ctx context.Context, name string) error {
	if _, err := p.api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(workspaceExecutionRolePolicyName)}); err != nil && !isIAMCode(err, "NoSuchEntity") {
		return err
	}
	_, err := p.api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)})
	if err != nil && !isIAMCode(err, "NoSuchEntity") {
		return err
	}
	return nil
}

// DeleteOwned is idempotent and removes only a role whose path, ownership
// tags, Lambda trust, inline policy set, and exact EFS policy still match this
// Workspace binding. It refuses unexpected attachments or drift.
func (p *ExecutionRoleProvider) DeleteOwned(ctx context.Context, identity Identity, binding Binding) error {
	if p == nil || p.api == nil || ctx == nil || ctx.Err() != nil || binding.ValidateFor(identity) != nil {
		return errors.New("Workspace execution role deletion identity or binding is invalid")
	}
	name, err := ExecutionRoleName(identity)
	if err != nil {
		return err
	}
	role, err := p.getRole(ctx, name)
	if isIAMCode(err, "NoSuchEntity") {
		return nil
	}
	if err != nil {
		return errors.New("load Workspace execution role for deletion")
	}
	trust, _ := ExecutionRoleTrustPolicy()
	fsARN := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:file-system/%s", p.partition, binding.Region, p.accountID, binding.FileSystemID)
	apARN := fmt.Sprintf("arn:%s:elasticfilesystem:%s:%s:access-point/%s", p.partition, binding.Region, p.accountID, binding.AccessPointID)
	permissions, err := ExecutionRolePermissionsPolicy(fsARN, apARN)
	if err != nil {
		return err
	}
	if err := p.verifyRole(ctx, role, name, trust, expectedWorkspaceRoleTags(identity), permissions, true); err != nil {
		return err
	}
	if _, err := p.api.DeleteRolePolicy(ctx, &iam.DeleteRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(workspaceExecutionRolePolicyName)}); err != nil && !isIAMCode(err, "NoSuchEntity") {
		return errors.New("delete Workspace execution role inline policy")
	}
	if _, err := p.api.DeleteRole(ctx, &iam.DeleteRoleInput{RoleName: aws.String(name)}); err != nil && !isIAMCode(err, "NoSuchEntity") {
		return errors.New("delete Workspace execution role")
	}
	return nil
}

func (p *ExecutionRoleProvider) getOrCreate(ctx context.Context, name, trust string, tags []types.Tag) (*types.Role, bool, error) {
	role, err := p.getRole(ctx, name)
	if err == nil {
		return role, false, nil
	}
	if !isIAMCode(err, "NoSuchEntity") {
		return nil, false, errors.New("load Workspace execution role")
	}
	output, createErr := p.api.CreateRole(ctx, &iam.CreateRoleInput{
		RoleName: aws.String(name), Path: aws.String(workspaceExecutionRolePath),
		AssumeRolePolicyDocument: aws.String(trust), Description: aws.String("Hako Workspace-scoped Lambda MicroVM EFS access"),
		MaxSessionDuration: aws.Int32(3600), Tags: tags,
	})
	if createErr != nil {
		// IAM role names are globally unique per account. A concurrent ensure can
		// win CreateRole; read it back and apply the same ownership checks.
		if isIAMCode(createErr, "EntityAlreadyExists") {
			role, err = p.getRole(ctx, name)
			if err == nil {
				return role, false, nil
			}
		}
		return nil, false, errors.New("create Workspace execution role")
	}
	if output == nil || output.Role == nil {
		return nil, true, errors.New("IAM returned no Workspace execution role")
	}
	return output.Role, true, nil
}

func (p *ExecutionRoleProvider) getRole(ctx context.Context, name string) (*types.Role, error) {
	output, err := p.api.GetRole(ctx, &iam.GetRoleInput{RoleName: aws.String(name)})
	if err != nil {
		return nil, err
	}
	if output == nil || output.Role == nil {
		return nil, errors.New("IAM returned an empty Workspace execution role")
	}
	return output.Role, nil
}

func (p *ExecutionRoleProvider) verifyRole(ctx context.Context, role *types.Role, name, trust string, tags []types.Tag, permissions string, allowMissingPolicy bool) error {
	if role == nil || aws.ToString(role.RoleName) != name || aws.ToString(role.Path) != workspaceExecutionRolePath ||
		aws.ToString(role.Arn) != p.roleARN(name) || !hasExpectedRoleTags(role.Tags, tags) {
		return errors.New("Workspace execution role is not owned by this Hako Workspace")
	}
	actualTrust, err := normalizedPolicy(aws.ToString(role.AssumeRolePolicyDocument))
	if err != nil {
		return errors.New("Workspace execution role trust policy is invalid")
	}
	expectedTrust, _ := normalizedPolicy(trust)
	if actualTrust != expectedTrust {
		return errors.New("Workspace execution role trust policy drifted")
	}
	attached, err := p.listAttachedPolicies(ctx, name)
	if err != nil {
		return errors.New("inspect Workspace execution role managed policies")
	}
	if len(attached) != 0 {
		return errors.New("Workspace execution role has unexpected managed policy attachments")
	}
	inline, err := p.listInlinePolicies(ctx, name)
	if err != nil {
		return errors.New("inspect Workspace execution role inline policies")
	}
	if len(inline) == 0 && allowMissingPolicy {
		return nil
	}
	if len(inline) == 0 {
		return nil
	}
	if len(inline) != 1 || inline[0] != workspaceExecutionRolePolicyName {
		return errors.New("Workspace execution role has unexpected inline policies")
	}
	return p.ensureExactPolicy(ctx, name, permissions)
}

func (p *ExecutionRoleProvider) ensureExactPolicy(ctx context.Context, name, expected string) error {
	output, err := p.api.GetRolePolicy(ctx, &iam.GetRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(workspaceExecutionRolePolicyName)})
	if isIAMCode(err, "NoSuchEntity") {
		if _, putErr := p.api.PutRolePolicy(ctx, &iam.PutRolePolicyInput{RoleName: aws.String(name), PolicyName: aws.String(workspaceExecutionRolePolicyName), PolicyDocument: aws.String(expected)}); putErr != nil {
			return errors.New("attach Workspace execution role policy")
		}
		return nil
	}
	if err != nil || output == nil || aws.ToString(output.PolicyDocument) == "" {
		return errors.New("read Workspace execution role policy")
	}
	actual, err := normalizedPolicy(aws.ToString(output.PolicyDocument))
	want, _ := normalizedPolicy(expected)
	if err != nil || actual != want {
		return errors.New("Workspace execution role policy drifted")
	}
	return nil
}

func (p *ExecutionRoleProvider) listInlinePolicies(ctx context.Context, name string) ([]string, error) {
	var names []string
	var marker *string
	seen := map[string]bool{}
	for {
		output, err := p.api.ListRolePolicies(ctx, &iam.ListRolePoliciesInput{RoleName: aws.String(name), Marker: marker, MaxItems: aws.Int32(100)})
		if err != nil {
			return nil, err
		}
		if output == nil {
			return nil, errors.New("IAM returned an empty inline policy listing")
		}
		names = append(names, output.PolicyNames...)
		if !output.IsTruncated {
			return names, nil
		}
		if output.Marker == nil || seen[aws.ToString(output.Marker)] {
			return nil, errors.New("IAM inline policy listing pagination is invalid")
		}
		seen[aws.ToString(output.Marker)] = true
		marker = output.Marker
	}
}

func (p *ExecutionRoleProvider) listAttachedPolicies(ctx context.Context, name string) ([]types.AttachedPolicy, error) {
	var policies []types.AttachedPolicy
	var marker *string
	seen := map[string]bool{}
	for {
		output, err := p.api.ListAttachedRolePolicies(ctx, &iam.ListAttachedRolePoliciesInput{RoleName: aws.String(name), Marker: marker, MaxItems: aws.Int32(100)})
		if err != nil {
			return nil, err
		}
		if output == nil {
			return nil, errors.New("IAM returned an empty managed policy listing")
		}
		policies = append(policies, output.AttachedPolicies...)
		if !output.IsTruncated {
			return policies, nil
		}
		if output.Marker == nil || seen[aws.ToString(output.Marker)] {
			return nil, errors.New("IAM managed policy listing pagination is invalid")
		}
		seen[aws.ToString(output.Marker)] = true
		marker = output.Marker
	}
}

func (p *ExecutionRoleProvider) roleARN(name string) string {
	return fmt.Sprintf("arn:%s:iam::%s:role%s%s", p.partition, p.accountID, workspaceExecutionRolePath, name)
}

func expectedWorkspaceRoleTags(identity Identity) []types.Tag {
	return []types.Tag{
		{Key: aws.String("hako:managed-by"), Value: aws.String("hako")},
		{Key: aws.String("hako:tenant-id"), Value: aws.String(string(identity.TenantID))},
		{Key: aws.String("hako:workspace-id"), Value: aws.String(string(identity.WorkspaceID))},
		{Key: aws.String("hako:resource-plane-id"), Value: aws.String(string(identity.ResourcePlaneID))},
	}
}

func hasExpectedRoleTags(actual, expected []types.Tag) bool {
	values := map[string]string{}
	for _, tag := range actual {
		key := aws.ToString(tag.Key)
		if _, duplicate := values[key]; duplicate {
			return false
		}
		values[key] = aws.ToString(tag.Value)
	}
	for _, tag := range expected {
		if values[aws.ToString(tag.Key)] != aws.ToString(tag.Value) {
			return false
		}
	}
	return true
}

func normalizedPolicy(document string) (string, error) {
	for _, candidate := range []string{document, func() string {
		decoded, err := url.QueryUnescape(document)
		if err != nil {
			return ""
		}
		return decoded
	}()} {
		if strings.TrimSpace(candidate) == "" {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(candidate))
		decoder.UseNumber()
		var value any
		if err := decoder.Decode(&value); err != nil {
			continue
		}
		var trailing any
		if err := decoder.Decode(&trailing); err != io.EOF {
			continue
		}
		encoded, err := json.Marshal(value)
		if err == nil {
			return string(encoded), nil
		}
	}
	return "", errors.New("policy document is invalid JSON")
}

func isIAMCode(err error, code string) bool {
	var apiErr interface{ ErrorCode() string }
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == code
}
