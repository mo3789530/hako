package resourcecontroller

import (
	"errors"

	"github.com/mo3789530/hako/internal/domain"
)

// WorkspaceResourceTags returns the required ownership and provenance tags
// for AWS resources created or managed for a Workspace operation. Runtime
// implementations must apply these tags to every taggable resource.
func WorkspaceResourceTags(command Command) (map[string]string, error) {
	if command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.OperationID == "" {
		return nil, errors.New("Tenant, Workspace, Resource Plane, and Operation IDs are required for resource tags")
	}
	return map[string]string{
		"hako:managed-by":        "hako",
		"hako:tenant-id":         string(command.TenantID),
		"hako:workspace-id":      string(command.WorkspaceID),
		"hako:resource-plane-id": string(command.ResourcePlaneID),
		"hako:operation-id":      string(command.OperationID),
	}, nil
}

// HasWorkspaceResourceTags checks that a discovered AWS resource belongs to
// both the requested Workspace and its Tenant/Resource Plane scope.
func HasWorkspaceResourceTags(tags map[string]string, tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID) bool {
	if tenantID == "" || workspaceID == "" || resourcePlaneID == "" {
		return false
	}
	return tags["hako:managed-by"] == "hako" &&
		tags["hako:tenant-id"] == string(tenantID) &&
		tags["hako:workspace-id"] == string(workspaceID) &&
		tags["hako:resource-plane-id"] == string(resourcePlaneID)
}
