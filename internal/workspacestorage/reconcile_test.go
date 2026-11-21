package workspacestorage

import (
	"context"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/aws/aws-sdk-go-v2/service/efs/types"
)

type reconciliationEFSStub struct {
	points []types.AccessPointDescription
}

func (s reconciliationEFSStub) CreateAccessPoint(context.Context, *efs.CreateAccessPointInput, ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error) {
	panic("unexpected CreateAccessPoint")
}

func (s reconciliationEFSStub) DescribeAccessPoints(context.Context, *efs.DescribeAccessPointsInput, ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error) {
	return &efs.DescribeAccessPointsOutput{AccessPoints: s.points}, nil
}

func (s reconciliationEFSStub) DeleteAccessPoint(context.Context, *efs.DeleteAccessPointInput, ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error) {
	panic("Reconcile must be read-only")
}

func TestReconcileClassifiesJournalAndAccessPointDriftWithoutMutation(t *testing.T) {
	bound := testIdentity()
	stale := bound
	stale.WorkspaceID = "ws-stale"
	unbound := bound
	unbound.WorkspaceID = "ws-unbound"
	missing := bound
	missing.WorkspaceID = "ws-missing"
	duplicate := bound
	duplicate.WorkspaceID = "ws-duplicate"
	owner := bound
	owner.WorkspaceID = "ws-owner"
	foreign := owner
	foreign.WorkspaceID = "ws-foreign"

	boundAP := testBinding(bound, "fsap-0123456789abcdef0")
	staleJournalAP := testBinding(stale, "fsap-1123456789abcdef0")
	missingAP := testBinding(missing, "fsap-2123456789abcdef0")
	duplicateAP1 := testBinding(duplicate, "fsap-3123456789abcdef0")
	duplicateAP2 := testBinding(duplicate, "fsap-4123456789abcdef0")
	sharedAP := testBinding(owner, "fsap-7123456789abcdef0")
	client, err := NewEFS(reconciliationEFSStub{points: []types.AccessPointDescription{
		describeBinding(bound, boundAP),
		describeBinding(stale, testBinding(stale, "fsap-5123456789abcdef0")),
		describeBinding(unbound, testBinding(unbound, "fsap-6123456789abcdef0")),
		describeBinding(duplicate, duplicateAP1),
		describeBinding(duplicate, duplicateAP2),
		describeBinding(foreign, testBinding(foreign, sharedAP.AccessPointID)),
	}}, "fs-0123456789abcdef0", "us-east-1")
	if err != nil {
		t.Fatal(err)
	}

	entries, err := client.Reconcile(context.Background(), []WorkspaceBinding{
		{Identity: bound, Binding: boundAP},
		{Identity: stale, Binding: staleJournalAP},
		{Identity: missing, Binding: missingAP},
		{Identity: duplicate, Binding: duplicateAP1},
		{Identity: owner, Binding: sharedAP},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int{}
	for _, entry := range entries {
		got[string(entry.Status)]++
	}
	want := map[string]int{
		string(ReconciliationReferenced):       1,
		string(ReconciliationStaleCandidate):   1,
		string(ReconciliationUnboundCandidate): 1,
		string(ReconciliationDuplicate):        2,
		string(ReconciliationIdentityMismatch): 1,
		string(ReconciliationMissing):          3,
	}
	if len(entries) != 9 || len(got) != len(want) {
		t.Fatalf("unexpected reconciliation report: %+v", entries)
	}
	for status, count := range want {
		if got[status] != count {
			t.Errorf("status %s count = %d, want %d; report: %+v", status, got[status], count, entries)
		}
	}
	for i := 1; i < len(entries); i++ {
		if entries[i-1].Identity.WorkspaceID > entries[i].Identity.WorkspaceID {
			t.Fatalf("report is not deterministic: %+v", entries)
		}
	}
}

func TestReconcileRejectsInvalidOrDuplicateJournalBindings(t *testing.T) {
	identity := testIdentity()
	binding := testBinding(identity, "fsap-0123456789abcdef0")
	client, _ := NewEFS(reconciliationEFSStub{}, "fs-0123456789abcdef0", "us-east-1")
	if _, err := client.Reconcile(context.Background(), []WorkspaceBinding{{Identity: identity, Binding: binding}, {Identity: identity, Binding: binding}}); err == nil {
		t.Fatal("accepted duplicate journal identity")
	}
	binding.Region = "us-west-2"
	if _, err := client.Reconcile(context.Background(), []WorkspaceBinding{{Identity: identity, Binding: binding}}); err == nil {
		t.Fatal("accepted binding from another Resource Plane Region")
	}
}

func testBinding(identity Identity, accessPointID string) Binding {
	uid := stablePOSIXID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	return Binding{
		FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: accessPointID,
		RootPath: "/hako/" + shortHash(string(identity.ResourcePlaneID)) + "/" + shortHash(string(identity.TenantID)) + "/" + shortHash(string(identity.WorkspaceID)),
		UID:      uid, GID: uid,
	}
}

func describeBinding(identity Identity, binding Binding) types.AccessPointDescription {
	return types.AccessPointDescription{
		AccessPointId: aws.String(binding.AccessPointID), FileSystemId: aws.String(binding.FileSystemID),
		PosixUser: &types.PosixUser{Uid: aws.Int64(int64(binding.UID)), Gid: aws.Int64(int64(binding.GID))},
		RootDirectory: &types.RootDirectory{Path: aws.String(binding.RootPath), CreationInfo: &types.CreationInfo{
			OwnerUid: aws.Int64(int64(binding.UID)), OwnerGid: aws.Int64(int64(binding.GID)), Permissions: aws.String("0700"),
		}},
		Tags: []types.Tag{
			{Key: aws.String("hako:managed-by"), Value: aws.String("hako")},
			{Key: aws.String("hako:tenant-id"), Value: aws.String(string(identity.TenantID))},
			{Key: aws.String("hako:workspace-id"), Value: aws.String(string(identity.WorkspaceID))},
			{Key: aws.String("hako:resource-plane-id"), Value: aws.String(string(identity.ResourcePlaneID))},
		},
	}
}
