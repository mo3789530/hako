// Package domain defines Hako's provider-independent control-plane model.
package domain

import (
	"encoding/json"
	"time"
)

type TenantID string
type UserID string
type WorkspaceID string
type ResourcePlaneID string
type OperationID string
type SessionID string
type ServiceID string
type VolumeID string

type Tenant struct {
	ID        TenantID  `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

type User struct {
	ID             UserID    `json:"id"`
	CognitoSubject string    `json:"cognito_subject"`
	Email          string    `json:"email"`
	CreatedAt      time.Time `json:"created_at"`
}

type TenantRole string

const (
	TenantRoleOwner  TenantRole = "owner"
	TenantRoleAdmin  TenantRole = "admin"
	TenantRoleMember TenantRole = "member"
)

type TenantMembership struct {
	TenantID TenantID   `json:"tenant_id"`
	UserID   UserID     `json:"user_id"`
	Role     TenantRole `json:"role"`
	JoinedAt time.Time  `json:"joined_at"`
}

type DesiredWorkspaceState string

const (
	DesiredWorkspaceRunning   DesiredWorkspaceState = "running"
	DesiredWorkspaceSuspended DesiredWorkspaceState = "suspended"
	DesiredWorkspaceDeleted   DesiredWorkspaceState = "deleted"
)

type ObservedWorkspaceState string

const (
	ObservedWorkspacePending      ObservedWorkspaceState = "pending"
	ObservedWorkspaceProvisioning ObservedWorkspaceState = "provisioning"
	ObservedWorkspaceRunning      ObservedWorkspaceState = "running"
	ObservedWorkspaceSuspending   ObservedWorkspaceState = "suspending"
	ObservedWorkspaceSuspended    ObservedWorkspaceState = "suspended"
	ObservedWorkspaceDeleting     ObservedWorkspaceState = "deleting"
	ObservedWorkspaceDeleted      ObservedWorkspaceState = "deleted"
	ObservedWorkspaceFailed       ObservedWorkspaceState = "failed"
)

type Workspace struct {
	ID           WorkspaceID `json:"id"`
	TenantID     TenantID    `json:"tenant_id"`
	OwnerID      UserID      `json:"owner_id"`
	Name         string      `json:"name"`
	RuntimeClass string      `json:"runtime_class"`
	Image        string      `json:"image"`
	CreatedAt    time.Time   `json:"created_at"`
	UpdatedAt    time.Time   `json:"updated_at"`
}

type WorkspaceStatus struct {
	WorkspaceID   WorkspaceID            `json:"workspace_id"`
	DesiredState  DesiredWorkspaceState  `json:"desired_state"`
	ObservedState ObservedWorkspaceState `json:"observed_state"`
	UpdatedAt     time.Time              `json:"updated_at"`
}

type Placement struct {
	WorkspaceID     WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID ResourcePlaneID `json:"resource_plane_id"`
	PlacedAt        time.Time       `json:"placed_at"`
}

type ResourcePlaneStatus string

const (
	ResourcePlaneActive   ResourcePlaneStatus = "active"
	ResourcePlaneDraining ResourcePlaneStatus = "draining"
	ResourcePlaneDisabled ResourcePlaneStatus = "disabled"
)

type ResourcePlane struct {
	ID           ResourcePlaneID `json:"id"`
	Provider     string          `json:"provider"`
	Region       string          `json:"region"`
	Capabilities []string        `json:"capabilities"`
}

type ResourcePlaneState struct {
	ResourcePlaneID ResourcePlaneID     `json:"resource_plane_id"`
	Status          ResourcePlaneStatus `json:"status"`
	UpdatedAt       time.Time           `json:"updated_at"`
}

type ResourceBinding struct {
	WorkspaceID     WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID ResourcePlaneID `json:"resource_plane_id"`
	Kind            string          `json:"kind"`
	ExternalID      string          `json:"external_id"`
	CreatedAt       time.Time       `json:"created_at"`
}

type OperationType string

const (
	OperationEnsureRunning OperationType = "ensure_running"
	OperationSuspend       OperationType = "suspend"
	OperationResume        OperationType = "resume"
	OperationDelete        OperationType = "delete"
)

type OperationStatus string

const (
	OperationPending   OperationStatus = "pending"
	OperationRunning   OperationStatus = "running"
	OperationSucceeded OperationStatus = "succeeded"
	OperationFailed    OperationStatus = "failed"
	OperationCancelled OperationStatus = "cancelled"
)

type Operation struct {
	ID              OperationID     `json:"id"`
	TenantID        TenantID        `json:"tenant_id"`
	WorkspaceID     WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID ResourcePlaneID `json:"resource_plane_id"`
	Type            OperationType   `json:"type"`
	Status          OperationStatus `json:"status"`
	IdempotencyKey  string          `json:"idempotency_key"`
	RequestHash     string          `json:"request_hash"`
	Attempt         int             `json:"attempt"`
	ErrorCode       string          `json:"error_code,omitempty"`
	CreatedAt       time.Time       `json:"created_at"`
	UpdatedAt       time.Time       `json:"updated_at"`
}

type OperationEvent struct {
	OperationID OperationID     `json:"operation_id"`
	Sequence    int64           `json:"sequence"`
	Type        string          `json:"type"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	CreatedAt   time.Time       `json:"created_at"`
}

type SessionPermission string

const (
	SessionPermissionVSCode      SessionPermission = "vscode"
	SessionPermissionShell       SessionPermission = "shell"
	SessionPermissionPortForward SessionPermission = "port-forward"
)

type Session struct {
	ID              SessionID           `json:"id"`
	UserID          UserID              `json:"user_id"`
	TenantID        TenantID            `json:"tenant_id"`
	WorkspaceID     WorkspaceID         `json:"workspace_id"`
	ResourcePlaneID ResourcePlaneID     `json:"resource_plane_id"`
	Permissions     []SessionPermission `json:"permissions"`
	ExpiresAt       time.Time           `json:"expires_at"`
	CreatedAt       time.Time           `json:"created_at"`
}

type Service struct {
	ID             ServiceID   `json:"id"`
	WorkspaceID    WorkspaceID `json:"workspace_id"`
	Name           string      `json:"name"`
	CatalogVersion string      `json:"catalog_version"`
	ConfigJSON     string      `json:"config_json"`
	DesiredState   string      `json:"desired_state"`
	ObservedState  string      `json:"observed_state"`
	CreatedAt      time.Time   `json:"created_at"`
	UpdatedAt      time.Time   `json:"updated_at"`
}

type Volume struct {
	ID          VolumeID    `json:"id"`
	WorkspaceID WorkspaceID `json:"workspace_id"`
	Name        string      `json:"name"`
	Kind        string      `json:"kind"`
	MountPath   string      `json:"mount_path"`
	CreatedAt   time.Time   `json:"created_at"`
}
