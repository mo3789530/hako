package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/efs"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

type reconcileEFSStub struct{ deleted, created bool }

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

func TestRunProducesReadOnlyJournalAndEFSReport(t *testing.T) {
	identity := workspacestorage.Identity{TenantID: domain.TenantID("tenant_a"), WorkspaceID: domain.WorkspaceID("workspace_a"), ResourcePlaneID: domain.ResourcePlaneID("rp_tokyo")}
	uid := posixID(string(identity.TenantID) + ":" + string(identity.WorkspaceID))
	binding := workspacestorage.Binding{
		FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/" + hash(string(identity.ResourcePlaneID)) + "/" + hash(string(identity.TenantID)) + "/" + hash(string(identity.WorkspaceID)), UID: uid, GID: uid,
	}
	inputPath := filepath.Join(t.TempDir(), "bindings.json")
	data, err := json.Marshal([]workspacestorage.WorkspaceBinding{{Identity: identity, Binding: binding}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	api := &reconcileEFSStub{}
	var output strings.Builder
	err = run(context.Background(), []string{"--region", "us-east-1", "--filesystem", binding.FileSystemID, "--bindings", inputPath}, &output,
		func(context.Context, string) (workspacestorage.EFSAPI, error) { return api, nil })
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

func TestReadBindingsRejectsUnknownFieldsTrailingDataAndOversize(t *testing.T) {
	for name, contents := range map[string]string{
		"unknown":  `[{"identity":{"tenant_id":"t","workspace_id":"w","resource_plane_id":"rp"},"binding":{},"delete":true}]`,
		"trailing": `[] []`,
		"oversize": strings.Repeat(" ", maxJournalInputBytes+1),
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "bindings.json")
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readBindings(path); err == nil {
				t.Fatal("invalid input was accepted")
			}
		})
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
