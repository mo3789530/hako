package main

import (
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/api"
)

func TestWriteTenantUsageRejectsInvalidFieldsBeforeWriting(t *testing.T) {
	valid := api.TenantUsage{WorkspacesLimit: 10, WorkloadRunsLimit: 5, QueuedWorkloadRunsLimit: 5,
		PreviewRunsLimit: 2, PreviewLifetimeLimit: 86400, ArtifactBytesLimit: 10 << 30}
	tests := []struct {
		name   string
		tenant string
		usage  api.TenantUsage
	}{
		{name: "empty tenant", usage: valid},
		{name: "negative workspace usage", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkspacesUsed = -1; return v }()},
		{name: "missing workspace limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkspacesLimit = 0; return v }()},
		{name: "missing Workload limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkloadRunsLimit = 0; return v }()},
		{name: "queue above concurrent limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.QueuedWorkloadRunsLimit = 6; return v }()},
		{name: "invalid Preview lifetime", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.PreviewLifetimeLimit = 299; return v }()},
		{name: "negative Artifact reservation", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.ArtifactBytesReserved = -1; return v }()},
		{name: "missing Artifact limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.ArtifactBytesLimit = 0; return v }()},
		{name: "negative compute reservation", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkspaceComputeCPUMilliUsed = -1; return v }()},
		{name: "negative compute limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkspaceComputeCPUMilliLimit = -1; return v }()},
		{name: "negative memory limit", tenant: "tenant_1", usage: func() api.TenantUsage { v := valid; v.WorkspaceComputeMemoryMiBLimit = -1; return v }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			if err := writeTenantUsage(&output, test.tenant, test.usage); err == nil {
				t.Fatal("invalid usage was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("invalid usage produced output: %q", output.String())
			}
		})
	}
}

func TestWriteTenantUsagePropagatesOutputFailure(t *testing.T) {
	usage := api.TenantUsage{WorkspacesLimit: 10, WorkloadRunsLimit: 5, QueuedWorkloadRunsLimit: 5,
		PreviewRunsLimit: 2, PreviewLifetimeLimit: 86400, ArtifactBytesLimit: 10 << 30}
	if err := writeTenantUsage(failingUsageWriter{}, "tenant_1", usage); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("usage output error = %v, want closed pipe", err)
	}
}

func TestWriteWorkspaceServiceResourceUsageValidatesBeforeWriting(t *testing.T) {
	valid := api.WorkspaceServiceResourceUsage{MemoryBytesUsed: 1024, MemoryBytesLimit: 2048, MilliCPUUsed: 500, MilliCPULimit: 1000}
	tests := []struct {
		name      string
		workspace string
		usage     api.WorkspaceServiceResourceUsage
	}{
		{name: "empty workspace", usage: valid},
		{name: "negative memory usage", workspace: "ws_1", usage: func() api.WorkspaceServiceResourceUsage { v := valid; v.MemoryBytesUsed = -1; return v }()},
		{name: "missing memory limit", workspace: "ws_1", usage: func() api.WorkspaceServiceResourceUsage { v := valid; v.MemoryBytesLimit = 0; return v }()},
		{name: "negative CPU usage", workspace: "ws_1", usage: func() api.WorkspaceServiceResourceUsage { v := valid; v.MilliCPUUsed = -1; return v }()},
		{name: "missing CPU limit", workspace: "ws_1", usage: func() api.WorkspaceServiceResourceUsage { v := valid; v.MilliCPULimit = 0; return v }()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output strings.Builder
			if err := writeWorkspaceServiceResourceUsage(&output, test.workspace, test.usage); err == nil {
				t.Fatal("invalid usage was accepted")
			}
			if output.Len() != 0 {
				t.Fatalf("invalid usage produced output: %q", output.String())
			}
		})
	}
}

func TestWriteWorkspaceServiceResourceUsagePropagatesOutputFailure(t *testing.T) {
	usage := api.WorkspaceServiceResourceUsage{MemoryBytesLimit: 2048, MilliCPULimit: 1000}
	if err := writeWorkspaceServiceResourceUsage(failingUsageWriter{}, "ws_1", usage); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("Workspace Service usage output error = %v, want closed pipe", err)
	}
}

type failingUsageWriter struct{}

func (failingUsageWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
