package resourcecontroller

import "testing"

func TestWorkspaceResourceTagsCarriesOwnershipAndOperation(t *testing.T) {
	command := Command{
		TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", OperationID: "op_1",
	}
	tags, err := WorkspaceResourceTags(command)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"hako:managed-by":        "hako",
		"hako:tenant-id":         "tenant_1",
		"hako:workspace-id":      "ws_1",
		"hako:resource-plane-id": "rp_1",
		"hako:operation-id":      "op_1",
	}
	if len(tags) != len(want) {
		t.Fatalf("got %d tags, want %d: %#v", len(tags), len(want), tags)
	}
	for key, value := range want {
		if tags[key] != value {
			t.Errorf("tag %s = %q, want %q", key, tags[key], value)
		}
	}
}

func TestWorkspaceResourceTagsRejectsIncompleteIdentity(t *testing.T) {
	if _, err := WorkspaceResourceTags(Command{TenantID: "tenant_1", WorkspaceID: "ws_1"}); err == nil {
		t.Fatal("expected missing Resource Plane and Operation IDs to be rejected")
	}
}

func TestHasWorkspaceResourceTagsRequiresExactTenantScope(t *testing.T) {
	tags, err := WorkspaceResourceTags(Command{
		TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", OperationID: "op_1",
	})
	if err != nil {
		t.Fatal(err)
	}
	if !HasWorkspaceResourceTags(tags, "tenant_1", "ws_1", "rp_1") {
		t.Fatal("expected exact identity tags to match")
	}
	if HasWorkspaceResourceTags(tags, "tenant_2", "ws_1", "rp_1") || HasWorkspaceResourceTags(tags, "tenant_1", "ws_2", "rp_1") || HasWorkspaceResourceTags(tags, "tenant_1", "ws_1", "rp_2") {
		t.Fatal("resource tagged for another scope must not match")
	}
	if HasWorkspaceResourceTags(tags, "", "ws_1", "rp_1") {
		t.Fatal("empty expected identity must not match")
	}
}
