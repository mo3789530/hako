package workspacestorage

import (
	"context"
	"errors"
	"fmt"
)

// WorkspaceStorage composes the Access Point and execution-role lifecycle.
// It returns one immutable binding for the Resource Plane runtime journal.
type WorkspaceStorage struct {
	accessPoints *EFS
	roles        *ExecutionRoleProvider
}

func NewWorkspaceStorage(accessPoints *EFS, roles *ExecutionRoleProvider) (*WorkspaceStorage, error) {
	if accessPoints == nil || roles == nil {
		return nil, errors.New("Workspace EFS Access Point and execution-role providers are required")
	}
	return &WorkspaceStorage{accessPoints: accessPoints, roles: roles}, nil
}

func (s *WorkspaceStorage) Ensure(ctx context.Context, identity Identity) (Binding, error) {
	if s == nil || s.accessPoints == nil || s.roles == nil || ctx == nil || ctx.Err() != nil {
		return Binding{}, errors.New("Workspace storage provisioner is not configured")
	}
	binding, accessPointCreated, err := s.accessPoints.EnsureWithCreated(ctx, identity)
	if err != nil {
		return Binding{}, err
	}
	role, err := s.roles.Ensure(ctx, identity, binding)
	if err != nil {
		if accessPointCreated {
			if cleanupErr := s.accessPoints.DeleteOwned(ctx, identity, binding); cleanupErr != nil {
				return Binding{}, errors.Join(fmt.Errorf("ensure Workspace execution role: %w", err), fmt.Errorf("compensate newly created Workspace Access Point: %w", cleanupErr))
			}
		}
		return Binding{}, fmt.Errorf("ensure Workspace execution role: %w", err)
	}
	binding.ExecutionRoleARN = role.ARN
	if err := binding.ValidateFor(identity); err != nil {
		return Binding{}, err
	}
	return binding, nil
}

func (s *WorkspaceStorage) DeleteOwned(ctx context.Context, identity Identity, binding Binding) error {
	if s == nil || s.accessPoints == nil || s.roles == nil || ctx == nil || ctx.Err() != nil || binding.ExecutionRoleARN == "" {
		return errors.New("Workspace storage deletion requires a complete EFS and execution-role binding")
	}
	if err := binding.ValidateFor(identity); err != nil {
		return err
	}
	if err := s.roles.DeleteOwned(ctx, identity, binding); err != nil {
		return err
	}
	return s.accessPoints.DeleteOwned(ctx, identity, binding)
}
