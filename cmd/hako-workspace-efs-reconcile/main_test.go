package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

type reconcileEFSStub struct{ deleted, created bool }

type journalListStub struct {
	bindings []workspacestorage.WorkspaceBinding
}

func (s journalListStub) ListStorageBindings(_ context.Context, limit int) ([]workspacestorage.WorkspaceBinding, error) {
	if len(s.bindings) > limit {
		return nil, errors.New("limit exceeded")
	}
	return s.bindings, nil
}

func (*reconcileEFSStub) DescribeAccessPoints(context.Context, *efs.DescribeAccessPointsInput, ...func(*efs.Options)) (*efs.DescribeAccessPointsOutput, error) {
	return &efs.DescribeAccessPointsOutput{AccessPoints: nil}, nil
}

func (s *reconcileEFSStub) CreateAccessPoint(context.Context, *efs.CreateAccessPointInput, ...func(*efs.Options)) (*efs.CreateAccessPointOutput, error) {
	s.created = true
	return nil, nil
}

func (s *reconcileEFSStub) DeleteAccessPoint(context.Context, *efs.DeleteAccessPointInput, ...func(*efs.Options)) (*efs.DeleteAccessPointOutput, error) {
	s.deleted = true
	return nil, nil
}

func TestRunListsJournalsAndProducesReadOnlyEFSReport(t *testing.T) {
	identity := workspacestorage.Identity{TenantID: domain.TenantID("tenant_a"), WorkspaceID: domain.WorkspaceID("workspace_a"), ResourcePlaneID: domain.ResourcePlaneID("rp_tokyo")}
	uid := posixID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	binding := workspacestorage.Binding{
		FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/" + hash(string(identity.ResourcePlaneID)) + "/" + hash(string(identity.TenantID)) + "/" + hash(string(identity.WorkspaceID)), UID: uid, GID: uid,
	}
	api := &reconcileEFSStub{}
	journals := journalListStub{bindings: []workspacestorage.WorkspaceBinding{{Identity: identity, Binding: binding}}}
	var output strings.Builder
	err := run(context.Background(), []string{"--region", "us-east-1", "--filesystem", binding.FileSystemID,
		"--journal-bucket", "hako-rp-journal", "--journal-prefix", "workspace-journal/rp_tokyo"}, &output,
		func(_ context.Context, region, bucket, prefix string) (workspacestorage.EFSAPI, journalBindingLister, error) {
			if region != "us-east-1" || bucket != "hako-rp-journal" || prefix != "workspace-journal/rp_tokyo" {
				t.Fatalf("unexpected read scope region=%q bucket=%q prefix=%q", region, bucket, prefix)
			}
			return api, journals, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	var report struct {
		ReadOnly bool                                   `json:"read_only"`
		Findings []workspacestorage.ReconciliationEntry `json:"findings"`
	}
	if err := json.Unmarshal([]byte(output.String()), &report); err != nil {
		t.Fatal(err)
	}
	if !report.ReadOnly || len(report.Findings) != 1 || report.Findings[0].Status != workspacestorage.ReconciliationMissing {
		t.Fatalf("unexpected read-only report: %+v", report)
	}
	if api.created || api.deleted {
		t.Fatal("reconciliation attempted to mutate EFS")
	}
}

func TestRunRequiresDedicatedJournalPrefix(t *testing.T) {
	called := false
	err := run(context.Background(), []string{"--region", "us-east-1", "--filesystem", "fs-0123456789abcdef0",
		"--journal-bucket", "hako-rp-journal", "--journal-prefix", "/"}, &strings.Builder{},
		func(context.Context, string, string, string) (workspacestorage.EFSAPI, journalBindingLister, error) {
			called = true
			return nil, nil, nil
		})
	if err == nil || called {
		t.Fatal("empty normalized journal prefix reached AWS client creation")
	}
}

func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

func posixID(value string) uint32 {
	sum := sha256.Sum256([]byte(value))
	return (uint32(sum[0])<<16 | uint32(sum[1])<<8 | uint32(sum[2])) + 100000
}
