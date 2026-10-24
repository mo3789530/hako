package workspacestorage

import (
	"context"
	"errors"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/iam"
	"github.com/aws/aws-sdk-go-v2/service/iam/types"
)

type iamFakeError string

func (e iamFakeError) Error() string     { return string(e) }
func (e iamFakeError) ErrorCode() string { return string(e) }

type iamFake struct {
	roles         map[string]*types.Role
	policies      map[string]map[string]string
	createCalls   int
	putCalls      int
	deleteCalls   int
	deletePolicy  int
	putErr        error
	policyEncoded bool
}

func newIAMFake() *iamFake {
	return &iamFake{roles: map[string]*types.Role{}, policies: map[string]map[string]string{}}
}

func (f *iamFake) CreateRole(_ context.Context, input *iam.CreateRoleInput, _ ...func(*iam.Options)) (*iam.CreateRoleOutput, error) {
	f.createCalls++
	name := aws.ToString(input.RoleName)
	if _, exists := f.roles[name]; exists {
		return nil, iamFakeError("EntityAlreadyExists")
	}
	role := &types.Role{RoleName: input.RoleName, Path: input.Path, AssumeRolePolicyDocument: input.AssumeRolePolicyDocument,
		Arn: aws.String("arn:aws:iam::123456789012:role" + aws.ToString(input.Path) + name), Tags: append([]types.Tag(nil), input.Tags...),
		RoleId: aws.String("AROATEST"), CreateDate: aws.Time(time.Now())}
	f.roles[name] = role
	f.policies[name] = map[string]string{}
	return &iam.CreateRoleOutput{Role: role}, nil
}

func (f *iamFake) GetRole(_ context.Context, input *iam.GetRoleInput, _ ...func(*iam.Options)) (*iam.GetRoleOutput, error) {
	role := f.roles[aws.ToString(input.RoleName)]
	if role == nil {
		return nil, iamFakeError("NoSuchEntity")
	}
	copyRole := *role
	copyRole.Tags = append([]types.Tag(nil), role.Tags...)
	return &iam.GetRoleOutput{Role: &copyRole}, nil
}

func (f *iamFake) PutRolePolicy(_ context.Context, input *iam.PutRolePolicyInput, _ ...func(*iam.Options)) (*iam.PutRolePolicyOutput, error) {
	f.putCalls++
	if f.putErr != nil {
		return nil, f.putErr
	}
	name := aws.ToString(input.RoleName)
	if f.roles[name] == nil {
		return nil, iamFakeError("NoSuchEntity")
	}
	document := aws.ToString(input.PolicyDocument)
	if f.policyEncoded {
		document = url.QueryEscape(document)
	}
	f.policies[name][aws.ToString(input.PolicyName)] = document
	return &iam.PutRolePolicyOutput{}, nil
}

func (f *iamFake) GetRolePolicy(_ context.Context, input *iam.GetRolePolicyInput, _ ...func(*iam.Options)) (*iam.GetRolePolicyOutput, error) {
	name := aws.ToString(input.RoleName)
	document, ok := f.policies[name][aws.ToString(input.PolicyName)]
	if !ok {
		return nil, iamFakeError("NoSuchEntity")
	}
	return &iam.GetRolePolicyOutput{RoleName: input.RoleName, PolicyName: input.PolicyName, PolicyDocument: aws.String(document)}, nil
}

func (f *iamFake) ListRolePolicies(_ context.Context, input *iam.ListRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListRolePoliciesOutput, error) {
	var names []string
	for name := range f.policies[aws.ToString(input.RoleName)] {
		names = append(names, name)
	}
	return &iam.ListRolePoliciesOutput{PolicyNames: names}, nil
}

func (f *iamFake) ListAttachedRolePolicies(_ context.Context, _ *iam.ListAttachedRolePoliciesInput, _ ...func(*iam.Options)) (*iam.ListAttachedRolePoliciesOutput, error) {
	return &iam.ListAttachedRolePoliciesOutput{}, nil
}

func (f *iamFake) DeleteRolePolicy(_ context.Context, input *iam.DeleteRolePolicyInput, _ ...func(*iam.Options)) (*iam.DeleteRolePolicyOutput, error) {
	f.deletePolicy++
	name := aws.ToString(input.RoleName)
	if f.roles[name] == nil {
		return nil, iamFakeError("NoSuchEntity")
	}
	if _, ok := f.policies[name][aws.ToString(input.PolicyName)]; !ok {
		return nil, iamFakeError("NoSuchEntity")
	}
	delete(f.policies[name], aws.ToString(input.PolicyName))
	return &iam.DeleteRolePolicyOutput{}, nil
}

func (f *iamFake) DeleteRole(_ context.Context, input *iam.DeleteRoleInput, _ ...func(*iam.Options)) (*iam.DeleteRoleOutput, error) {
	f.deleteCalls++
	name := aws.ToString(input.RoleName)
	if f.roles[name] == nil {
		return nil, iamFakeError("NoSuchEntity")
	}
	if len(f.policies[name]) > 0 {
		return nil, errors.New("role still has inline policies")
	}
	delete(f.roles, name)
	delete(f.policies, name)
	return &iam.DeleteRoleOutput{}, nil
}

func TestWorkspaceExecutionRoleProviderEnsuresExactRoleIdempotently(t *testing.T) {
	fake := newIAMFake()
	fake.policyEncoded = true
	provider, err := NewExecutionRoleProvider(fake, "aws", "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	binding := testRoleBinding(identity)
	first, err := provider.Ensure(context.Background(), identity, binding)
	if err != nil {
		t.Fatal(err)
	}
	second, err := provider.Ensure(context.Background(), identity, binding)
	if err != nil || first != second {
		t.Fatalf("repeat ensure changed role identity: first=%+v second=%+v err=%v", first, second, err)
	}
	if fake.createCalls != 1 || fake.putCalls != 1 {
		t.Fatalf("idempotent ensure calls = create:%d put:%d", fake.createCalls, fake.putCalls)
	}
	role := fake.roles[first.Name]
	if role == nil || aws.ToString(role.Path) != workspaceExecutionRolePath || aws.ToString(role.Arn) != first.ARN ||
		!hasExpectedRoleTags(role.Tags, expectedWorkspaceRoleTags(identity)) {
		t.Fatalf("Workspace role lacks path, ARN, or ownership tags: %+v", role)
	}
	if policy := fake.policies[first.Name][workspaceExecutionRolePolicyName]; policy == "" {
		t.Fatal("exact Workspace EFS policy was not attached")
	}
}

func TestWorkspaceExecutionRoleProviderRejectsForeignRoleAndPolicyDrift(t *testing.T) {
	identity := testIdentity()
	binding := testRoleBinding(identity)
	t.Run("foreign ownership", func(t *testing.T) {
		fake := newIAMFake()
		provider, _ := NewExecutionRoleProvider(fake, "aws", "123456789012")
		name, _ := ExecutionRoleName(identity)
		trust, _ := ExecutionRoleTrustPolicy()
		if _, err := fake.CreateRole(context.Background(), &iam.CreateRoleInput{RoleName: aws.String(name), Path: aws.String(workspaceExecutionRolePath), AssumeRolePolicyDocument: aws.String(trust), Tags: expectedWorkspaceRoleTags(identity)}); err != nil {
			t.Fatal(err)
		}
		fake.roles[name].Tags[1].Value = aws.String("tenant-foreign")
		if _, err := provider.Ensure(context.Background(), identity, binding); err == nil {
			t.Fatal("adopted a role with foreign ownership tags")
		}
		if fake.putCalls != 0 {
			t.Fatal("modified a foreign role policy")
		}
	})

	t.Run("policy drift", func(t *testing.T) {
		fake := newIAMFake()
		provider, _ := NewExecutionRoleProvider(fake, "aws", "123456789012")
		role, err := provider.Ensure(context.Background(), identity, binding)
		if err != nil {
			t.Fatal(err)
		}
		fake.policies[role.Name][workspaceExecutionRolePolicyName] = `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":"*","Resource":"*"}]}`
		if _, err := provider.Ensure(context.Background(), identity, binding); err == nil {
			t.Fatal("silently repaired a broadened Workspace role")
		}
		if fake.putCalls != 1 {
			t.Fatal("overwrote a drifted policy instead of failing closed")
		}
	})
}

func TestWorkspaceExecutionRoleProviderDeleteOwnedIsFencedAndIdempotent(t *testing.T) {
	fake := newIAMFake()
	provider, err := NewExecutionRoleProvider(fake, "aws", "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	identity := testIdentity()
	binding := testRoleBinding(identity)
	role, err := provider.Ensure(context.Background(), identity, binding)
	if err != nil {
		t.Fatal(err)
	}
	other := identity
	other.WorkspaceID = "ws-other"
	if err := provider.DeleteOwned(context.Background(), other, binding); err == nil {
		t.Fatal("deleted a different Workspace role")
	}
	if fake.deleteCalls != 0 {
		t.Fatal("issued delete before binding identity validation")
	}
	if err := provider.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatal(err)
	}
	if fake.roles[role.Name] != nil || fake.deletePolicy != 1 || fake.deleteCalls != 1 {
		t.Fatalf("owned Workspace role was not completely deleted: role=%v policy=%d roleDelete=%d", fake.roles[role.Name], fake.deletePolicy, fake.deleteCalls)
	}
	if err := provider.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatalf("repeat role deletion was not idempotent: %v", err)
	}
}

func TestWorkspaceExecutionRoleProviderCompensatesFailedPolicyAttach(t *testing.T) {
	fake := newIAMFake()
	fake.putErr = errors.New("synthetic attach failure")
	provider, _ := NewExecutionRoleProvider(fake, "aws", "123456789012")
	if _, err := provider.Ensure(context.Background(), testIdentity(), testRoleBinding(testIdentity())); err == nil {
		t.Fatal("ignored inline policy attach failure")
	}
	if len(fake.roles) != 0 || len(fake.policies) != 0 || fake.deleteCalls != 1 {
		t.Fatalf("failed role creation left an unconfigured role: roles=%d deletes=%d", len(fake.roles), fake.deleteCalls)
	}
}

func testRoleBinding(identity Identity) Binding {
	uid := stablePOSIXID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	return Binding{FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/" + shortHash(string(identity.ResourcePlaneID)) + "/" + shortHash(string(identity.TenantID)) + "/" + shortHash(string(identity.WorkspaceID)), UID: uid, GID: uid}
}
