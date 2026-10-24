package workspacestorage

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
)

func TestWorkspaceStorageBindsAccessPointAndExecutionRole(t *testing.T) {
	identity := testIdentity()
	efsFake := &efsStub{}
	accessPoints, err := NewEFS(efsFake, "fs-0123456789abcdef0", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}
	roleFake := newIAMFake()
	roles, err := NewExecutionRoleProvider(roleFake, "aws", "123456789012")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := NewWorkspaceStorage(accessPoints, roles)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := storage.Ensure(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	name, _ := ExecutionRoleName(identity)
	if binding.AccessPointID == "" || binding.ExecutionRoleARN != "arn:aws:iam::123456789012:role/hako/workspaces/"+name {
		t.Fatalf("Workspace binding omitted its AP or dynamic execution role: %+v", binding)
	}
	retry, err := storage.Ensure(context.Background(), identity)
	if err != nil || retry != binding || roleFake.createCalls != 1 || roleFake.putCalls != 1 {
		t.Fatalf("Workspace ensure retry changed or duplicated resources: retry=%+v binding=%+v create=%d put=%d err=%v", retry, binding, roleFake.createCalls, roleFake.putCalls, err)
	}
	if err := storage.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatal(err)
	}
	if roleFake.roles[name] != nil || efsFake.deleted != 1 {
		t.Fatalf("Workspace cleanup did not remove role then AP: role=%v AP deletes=%d", roleFake.roles[name], efsFake.deleted)
	}
	if err := storage.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatalf("Workspace cleanup retry was not idempotent: %v", err)
	}
}

func TestWorkspaceStorageRefusesIncompleteRoleBindingOnDelete(t *testing.T) {
	identity := testIdentity()
	efsFake := &efsStub{noPoint: true}
	accessPoints, _ := NewEFS(efsFake, "fs-0123456789abcdef0", "us-east-1")
	roles, _ := NewExecutionRoleProvider(newIAMFake(), "aws", "123456789012")
	storage, _ := NewWorkspaceStorage(accessPoints, roles)
	binding := testRoleBinding(identity)
	if err := storage.DeleteOwned(context.Background(), identity, binding); err == nil {
		t.Fatal("deleted storage without the Workspace role binding")
	}
	if efsFake.deleted != 0 {
		t.Fatal("deleted the Access Point before validating the role binding")
	}
}

var _ EFSAPI = (*efsStub)(nil)
var _ IAMAPI = (*iamFake)(nil)

func TestWorkspaceStorageAccessPointIdentityMismatchLeavesRoleUntouched(t *testing.T) {
	identity := testIdentity()
	efsFake := &efsStub{}
	accessPoints, _ := NewEFS(efsFake, "fs-0123456789abcdef0", "us-east-1")
	roles := newIAMFake()
	provider, _ := NewExecutionRoleProvider(roles, "aws", "123456789012")
	storage, _ := NewWorkspaceStorage(accessPoints, provider)
	if _, err := storage.Ensure(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	identity.WorkspaceID = "ws-foreign"
	// Force the fake to return an AP description whose ownership tags conflict
	// with the requested Workspace before IAM role creation is attempted.
	efsFake.point.Tags = []types.Tag{{Key: aws.String("hako:managed-by"), Value: aws.String("hako")},
		{Key: aws.String("hako:tenant-id"), Value: aws.String("tenant-foreign")},
		{Key: aws.String("hako:workspace-id"), Value: aws.String("ws-foreign")},
		{Key: aws.String("hako:resource-plane-id"), Value: aws.String("rp-tokyo-01")}}
	if _, err := storage.Ensure(context.Background(), identity); err == nil {
		t.Fatal("accepted Access Point ownership mismatch")
	}
	if roles.createCalls != 1 {
		t.Fatalf("created IAM role before AP ownership was confirmed: creates=%d", roles.createCalls)
	}
}

func TestWorkspaceStorageCompensatesOnlyNewAccessPointWhenRoleProvisioningFails(t *testing.T) {
	t.Run("new Access Point is removed", func(t *testing.T) {
		identity := testIdentity()
		efsFake := &efsStub{}
		accessPoints, _ := NewEFS(efsFake, "fs-0123456789abcdef0", "us-east-1")
		roleFake := newIAMFake()
		roleFake.putErr = errors.New("synthetic policy failure")
		roles, _ := NewExecutionRoleProvider(roleFake, "aws", "123456789012")
		storage, _ := NewWorkspaceStorage(accessPoints, roles)
		if _, err := storage.Ensure(context.Background(), identity); err == nil {
			t.Fatal("expected IAM policy provisioning failure")
		}
		if efsFake.deleted != 1 || roleFake.deleteCalls != 1 {
			t.Fatalf("new resources were not compensated: AP deletes=%d IAM role deletes=%d", efsFake.deleted, roleFake.deleteCalls)
		}
	})

	t.Run("existing Access Point is preserved", func(t *testing.T) {
		identity := testIdentity()
		efsFake := &efsStub{}
		accessPoints, _ := NewEFS(efsFake, "fs-0123456789abcdef0", "us-east-1")
		if _, err := accessPoints.Ensure(context.Background(), identity); err != nil {
			t.Fatal(err)
		}
		roleFake := newIAMFake()
		roleFake.putErr = errors.New("synthetic policy failure")
		roles, _ := NewExecutionRoleProvider(roleFake, "aws", "123456789012")
		storage, _ := NewWorkspaceStorage(accessPoints, roles)
		if _, err := storage.Ensure(context.Background(), identity); err == nil {
			t.Fatal("expected IAM policy provisioning failure")
		}
		if efsFake.deleted != 0 {
			t.Fatalf("pre-existing Access Point was removed during compensation: deletes=%d", efsFake.deleted)
		}
	})
}
