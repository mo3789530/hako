package workspacegateway

import (
	"context"

	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

// PreviewPortAuthorizer permits only the fixed application port on the
// generation-specific Preview runtime identity. Ordinary Workspace port
// forwarding remains denied until it has a separate service-state policy.
type PreviewPortAuthorizer struct{}

func (PreviewPortAuthorizer) AllowPort(ctx context.Context, claims workspacesession.Claims, port uint16) (bool, error) {
	if ctx == nil || ctx.Err() != nil || !previewprotocol.IsRuntimeWorkspaceID(string(claims.WorkspaceID)) ||
		port != previewprotocol.ApplicationPort || !hasPortForwardPermission(claims.Permissions) {
		return false, nil
	}
	return true, nil
}

func hasPortForwardPermission(permissions []workspacesession.Permission) bool {
	for _, permission := range permissions {
		if permission == workspacesession.PermissionPortForward {
			return true
		}
	}
	return false
}

var _ PortAuthorizer = PreviewPortAuthorizer{}

// PortAuthorizerChain checks policies in order. A policy error is fail-closed;
// a later policy is consulted only when earlier policies do not allow a port.
type PortAuthorizerChain []PortAuthorizer

func (chain PortAuthorizerChain) AllowPort(ctx context.Context, claims workspacesession.Claims, port uint16) (bool, error) {
	for _, policy := range chain {
		if policy == nil {
			continue
		}
		allowed, err := policy.AllowPort(ctx, claims, port)
		if err != nil {
			return false, err
		}
		if allowed {
			return true, nil
		}
	}
	return false, nil
}
