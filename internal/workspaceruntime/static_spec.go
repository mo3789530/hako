package workspaceruntime

import (
	"context"
	"errors"

	"github.com/mo3789530/hako/internal/resourcecontroller"
)

// StaticSpecResolver returns one immutable, Resource Plane-owned launch
// profile. Workspace commands do not contain image, role, or network values;
// those are selected by the Resource Plane deployment configuration.
type StaticSpecResolver struct {
	spec LaunchSpec
}

func NewStaticSpecResolver(spec LaunchSpec) (*StaticSpecResolver, error) {
	if spec.RunHookPayload != "" {
		return nil, errors.New("static Workspace launch profiles must not contain per-Workspace hook payloads")
	}
	if err := ValidateLaunchSpec(spec); err != nil {
		return nil, err
	}
	return &StaticSpecResolver{spec: cloneLaunchSpec(spec)}, nil
}

func (r *StaticSpecResolver) Resolve(ctx context.Context, command resourcecontroller.Command) (LaunchSpec, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || command.TenantID == "" || command.WorkspaceID == "" ||
		command.ResourcePlaneID == "" || command.WorkspaceRevision < 1 || command.OperationID == "" {
		return LaunchSpec{}, ErrSpecification
	}
	return cloneLaunchSpec(r.spec), nil
}

func cloneLaunchSpec(spec LaunchSpec) LaunchSpec {
	spec.EgressNetworkConnectors = append([]string(nil), spec.EgressNetworkConnectors...)
	spec.IngressNetworkConnectors = append([]string(nil), spec.IngressNetworkConnectors...)
	return spec
}
