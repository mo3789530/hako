package previews

import (
	"testing"
	"time"
)

func TestRuntimeWorkspaceIDIsStableAndGenerationScoped(t *testing.T) {
	identity := CleanupIdentity{TenantID: "tenant_1", InstallationID: 42, RepositoryID: 84, PullRequestNumber: 7}
	first := runtimeWorkspaceID(identity, 1)
	if first != runtimeWorkspaceID(identity, 1) {
		t.Fatal("runtime Workspace ID changed for the same Preview generation")
	}
	if first == runtimeWorkspaceID(identity, 2) || first == runtimeWorkspaceID(CleanupIdentity{
		TenantID: "tenant_2", InstallationID: identity.InstallationID, RepositoryID: identity.RepositoryID, PullRequestNumber: identity.PullRequestNumber,
	}, 1) {
		t.Fatal("runtime Workspace ID collided across generation or Tenant")
	}
	if len(first) != len("prvws_")+32 || first[:6] != "prvws_" {
		t.Fatalf("runtime Workspace ID = %q, want private fixed-size ID", first)
	}
	exported, err := RuntimeWorkspaceID(identity, 1)
	if err != nil || exported != first {
		t.Fatalf("exported runtime Workspace ID = %q err=%v, want %q", exported, err, first)
	}
	if _, err := RuntimeWorkspaceID(identity, 0); err == nil {
		t.Fatal("runtime Workspace ID accepted generation zero")
	}
	if _, err := RuntimeWorkspaceID(CleanupIdentity{}, 1); err == nil {
		t.Fatal("runtime Workspace ID accepted incomplete identity")
	}
}

func TestProvisionJobRequiresGenerationDerivedWorkspaceBinding(t *testing.T) {
	job := ProvisionJob{
		CleanupIdentity: CleanupIdentity{TenantID: "tenant_1", InstallationID: 42, RepositoryID: 84, PullRequestNumber: 7},
		Generation:      3, TargetResourcePlaneID: "rp_preview_1", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", ExpiresAt: time.Now().Add(time.Hour),
		ClaimID: "preview-provision-1234", LeaseExpiresAt: time.Now().Add(time.Minute), Attempt: 1,
	}
	job.RuntimeWorkspaceID = runtimeWorkspaceID(job.CleanupIdentity, job.Generation)
	if !validProvisionJob(job) {
		t.Fatal("valid generation-pinned provision job was rejected")
	}
	job.RuntimeWorkspaceID = "ws_other_generation"
	if validProvisionJob(job) {
		t.Fatal("provision job accepted a Workspace ID from another generation")
	}
}
