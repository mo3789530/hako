package workspaceactivity

import (
	"testing"
	"time"
)

func TestValidLeaseBoundsAndIdentity(t *testing.T) {
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	lease := Lease{ID: "0123456789abcdef0123456789abcdef0123456789abcdef", SessionID: "abcdef0123456789abcdef0123456789abcdef0123456789", TenantID: "tenant_a", WorkspaceID: "workspace_a",
		ResourcePlaneID: "rp_a", MicroVMID: "vm_a", AllocationGeneration: 3, Now: now, ExpiresAt: now.Add(90 * time.Second)}
	if !validLease(lease) {
		t.Fatal("valid 90-second lease rejected")
	}
	invalid := []Lease{
		{ID: "short", TenantID: lease.TenantID, WorkspaceID: lease.WorkspaceID, ResourcePlaneID: lease.ResourcePlaneID,
			MicroVMID: lease.MicroVMID, AllocationGeneration: lease.AllocationGeneration, Now: now, ExpiresAt: lease.ExpiresAt},
		{ID: lease.ID, TenantID: lease.TenantID, WorkspaceID: lease.WorkspaceID, ResourcePlaneID: lease.ResourcePlaneID,
			MicroVMID: lease.MicroVMID, AllocationGeneration: lease.AllocationGeneration, Now: now, ExpiresAt: now.Add(MaxLeaseTTL + time.Nanosecond)},
		{ID: lease.ID, TenantID: lease.TenantID, WorkspaceID: lease.WorkspaceID, ResourcePlaneID: lease.ResourcePlaneID,
			MicroVMID: lease.MicroVMID, AllocationGeneration: 0, Now: now, ExpiresAt: lease.ExpiresAt},
	}
	for i, value := range invalid {
		if validLease(value) {
			t.Errorf("invalid lease %d accepted", i)
		}
	}
}
