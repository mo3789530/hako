package workspacestorage

import (
	"context"
	"errors"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
	"github.com/mo3789530/hako/internal/domain"
)

type efsStub struct {
	createInput   *efs.CreateAccessPointInput
	point         types.AccessPointDescription
	createErr     error
	describeErr   error
	deleteErr     error
	deleted       int
	preservePoint bool
	noPoint       bool
}

func (s *efsStub) CreateAccessPoint(_ context.Context, input *efs.CreateAccessPointInput, _ ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error) {
	s.createInput = input
	if s.createErr != nil {
		return nil, s.createErr
	}
	if !s.preservePoint && (s.point.AccessPointId == nil || aws.ToString(s.point.ClientToken) != aws.ToString(input.ClientToken)) {
		id := "fsap-0123456789abcdef0"
		if s.point.AccessPointId != nil {
			id = "fsap-1123456789abcdef0"
		}
		s.point = pointFromCreate(input, id)
	}
	return &efs.CreateAccessPointOutput{
		AccessPointId: s.point.AccessPointId, ClientToken: s.point.ClientToken,
		FileSystemId: s.point.FileSystemId, PosixUser: s.point.PosixUser,
		RootDirectory: s.point.RootDirectory, Tags: s.point.Tags,
	}, nil
}

func (s *efsStub) DescribeAccessPoints(_ context.Context, _ *efs.DescribeAccessPointsInput, _ ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error) {
	if s.describeErr != nil {
		return nil, s.describeErr
	}
	if s.noPoint {
		return &efs.DescribeAccessPointsOutput{}, nil
	}
	return &efs.DescribeAccessPointsOutput{AccessPoints: []types.AccessPointDescription{s.point}}, nil
}

func (s *efsStub) DeleteAccessPoint(_ context.Context, _ *efs.DeleteAccessPointInput, _ ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error) {
	s.deleted++
	return &efs.DeleteAccessPointOutput{}, s.deleteErr
}

func TestEnsureAccessPointIsWorkspaceScopedAndIdempotent(t *testing.T) {
	identity := testIdentity()
	stub := &efsStub{}
	client, err := NewEFS(stub, "fs-0123456789abcdef0")
	if err != nil {
		t.Fatal(err)
	}
	first, err := client.Ensure(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if len(stub.createInput.Tags) != 4 || aws.ToString(stub.createInput.FileSystemId) != first.FileSystemID ||
		aws.ToInt64(stub.createInput.PosixUser.Uid) != int64(first.UID) ||
		aws.ToString(stub.createInput.RootDirectory.Path) != first.RootPath ||
		aws.ToString(stub.createInput.RootDirectory.CreationInfo.Permissions) != "0700" {
		t.Fatalf("Access Point request lacks Workspace confinement: %+v", stub.createInput)
	}
	firstToken := aws.ToString(stub.createInput.ClientToken)
	second, err := client.Ensure(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || aws.ToString(stub.createInput.ClientToken) != aws.ToString(stub.point.ClientToken) {
		t.Fatalf("retry did not preserve the same Access Point identity: first=%+v second=%+v", first, second)
	}

	other := identity
	other.WorkspaceID = "ws-other"
	third, err := client.Ensure(context.Background(), other)
	if err != nil {
		t.Fatal(err)
	}
	if third.RootPath == first.RootPath || third.UID == first.UID || aws.ToString(stub.createInput.ClientToken) == firstToken {
		t.Fatal("different Workspace reused storage isolation identity")
	}
}

func TestEnsureRejectsMismatchedEFSResponse(t *testing.T) {
	stub := &efsStub{point: types.AccessPointDescription{
		AccessPointId: aws.String("fsap-0123456789abcdef0"), FileSystemId: aws.String("fs-fffffffffffffffff"),
	}, preservePoint: true}
	client, _ := NewEFS(stub, "fs-0123456789abcdef0")
	if _, err := client.Ensure(context.Background(), testIdentity()); err == nil {
		t.Fatal("accepted an Access Point created on a different filesystem")
	}
	stub.createErr = errors.New("denied")
	if _, err := client.Ensure(context.Background(), testIdentity()); err == nil {
		t.Fatal("ignored Access Point create failure")
	}
}

func TestDeleteOwnedRequiresExactWorkspaceOwnership(t *testing.T) {
	identity := testIdentity()
	stub := &efsStub{}
	client, _ := NewEFS(stub, "fs-0123456789abcdef0")
	binding, err := client.Ensure(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	stub.point = pointFromCreate(stub.createInput, binding.AccessPointID)
	if err := client.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatal(err)
	}
	if stub.deleted != 1 {
		t.Fatalf("expected one AP deletion, got %d", stub.deleted)
	}
	stub.deleted = 0
	other := identity
	other.TenantID = "tenant-other"
	if err := client.DeleteOwned(context.Background(), other, binding); err == nil {
		t.Fatal("deleted an AP belonging to another Tenant")
	}
	if stub.deleted != 0 {
		t.Fatal("issued delete before verifying ownership")
	}
}

func TestDeleteOwnedIsIdempotentWhenAlreadyAbsent(t *testing.T) {
	identity := testIdentity()
	stub := &efsStub{}
	client, _ := NewEFS(stub, "fs-0123456789abcdef0")
	binding, _ := client.Ensure(context.Background(), identity)
	stub.noPoint = true
	if err := client.DeleteOwned(context.Background(), identity, binding); err != nil {
		t.Fatalf("missing AP should be an idempotent delete: %v", err)
	}
	if stub.deleted != 0 {
		t.Fatal("issued delete for already absent AP")
	}
}

func TestDiscoverManagedListsAndValidatesTaggedWorkspaces(t *testing.T) {
	identity := testIdentity()
	stub := &efsStub{}
	client, _ := NewEFS(stub, "fs-0123456789abcdef0")
	binding, err := client.Ensure(context.Background(), identity)
	if err != nil {
		t.Fatal(err)
	}
	stub.point = pointFromCreate(stub.createInput, binding.AccessPointID)
	found, err := client.DiscoverManaged(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(found) != 1 || found[0].Identity != identity || found[0].Binding != binding {
		t.Fatalf("discovery did not return exact Workspace storage: %+v", found)
	}
	stub.point.Tags = stub.point.Tags[:3]
	if _, err := client.DiscoverManaged(context.Background()); err == nil {
		t.Fatal("accepted incomplete Hako ownership tags")
	}
	stub.point = pointFromCreate(stub.createInput, binding.AccessPointID)
	stub.point.Tags = append(stub.point.Tags, types.Tag{Key: aws.String("hako:tenant-id"), Value: aws.String("tenant-other")})
	if _, err := client.DiscoverManaged(context.Background()); err == nil {
		t.Fatal("accepted duplicate conflicting ownership tags")
	}
}

func pointFromCreate(input *efs.CreateAccessPointInput, id string) types.AccessPointDescription {
	return types.AccessPointDescription{AccessPointId: aws.String(id), ClientToken: input.ClientToken,
		FileSystemId: input.FileSystemId, PosixUser: input.PosixUser, RootDirectory: input.RootDirectory, Tags: input.Tags}
}

func testIdentity() Identity {
	return Identity{TenantID: domain.TenantID("tenant-a"), WorkspaceID: domain.WorkspaceID("ws-one"), ResourcePlaneID: domain.ResourcePlaneID("rp-tokyo-01")}
}
