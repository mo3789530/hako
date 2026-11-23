//go:build linux

package workspaceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

var firecrackerWorkspaceProviderIDPattern = regexp.MustCompile(`^fc_[a-f0-9]{32}$`)

// FirecrackerLocalWorkspaceLaunchResolver binds trusted Workspace identity to
// the Guest kernel command line. All executable/image paths remain in the
// host-owned base specification; an Operation cannot choose them.
type FirecrackerLocalWorkspaceLaunchResolver struct {
	base    FirecrackerLocalLaunchSpec
	journal workspacejournal.Store
	origin  string
}

func NewFirecrackerLocalWorkspaceLaunchResolver(payload []byte, journal workspacejournal.Store, gatewayOrigin string) (*FirecrackerLocalWorkspaceLaunchResolver, error) {
	if journal == nil || !validLocalGatewayOrigin(gatewayOrigin) {
		return nil, errors.New("local Workspace launch resolver configuration is invalid")
	}
	spec, err := decodeFirecrackerLaunchSpec(payload)
	if err != nil || prepareFirecrackerLaunchSpec(&spec) != nil || containsReservedGuestKernelArgs(spec.KernelBootArgs) {
		return nil, errors.New("local Workspace base launch profile is invalid")
	}
	return &FirecrackerLocalWorkspaceLaunchResolver{base: spec, journal: journal, origin: gatewayOrigin}, nil
}

func (r *FirecrackerLocalWorkspaceLaunchResolver) ResolveLocal(ctx context.Context, command resourcecontroller.Command) ([]byte, error) {
	if r == nil || ctx == nil || ctx.Err() != nil || command.Type != domain.OperationEnsureRunning ||
		!localAllocationIDPattern.MatchString(string(command.TenantID)) || !localAllocationIDPattern.MatchString(string(command.WorkspaceID)) ||
		!localAllocationIDPattern.MatchString(string(command.ResourcePlaneID)) || command.WorkspaceRevision < 1 ||
		!localAllocationIDPattern.MatchString(string(command.OperationID)) {
		return nil, ErrSpecification
	}
	providerJournal := workspacejournal.Journal{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, AllocationGeneration: 1}
	current, err := r.journal.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	switch {
	case errors.Is(err, workspacejournal.ErrNotFound):
	case err != nil:
		return nil, errors.New("read local Workspace allocation generation")
	default:
		if current.ETag == "" || current.Journal.Validate() != nil || current.Journal.TenantID != command.TenantID ||
			current.Journal.WorkspaceID != command.WorkspaceID || current.Journal.ResourcePlaneID != command.ResourcePlaneID ||
			current.Journal.State != workspacejournal.Terminated || current.Journal.Tombstone ||
			current.Journal.CleanupState != workspacejournal.CleanupConfirmed || current.Journal.AllocationGeneration < 1 {
			return nil, workspacejournal.ErrConflict
		}
		providerJournal.AllocationGeneration = current.Journal.AllocationGeneration + 1
	}
	providerID := localProviderID(providerJournal)
	if !firecrackerWorkspaceProviderIDPattern.MatchString(providerID) {
		return nil, ErrSpecification
	}
	spec := r.base
	identity := strings.Join([]string{
		"hako.tenant_id=" + string(command.TenantID),
		"hako.workspace_id=" + string(command.WorkspaceID),
		"hako.resource_plane_id=" + string(command.ResourcePlaneID),
		"hako.microvm_id=" + providerID,
		"hako.gateway_origin=" + r.origin,
	}, " ")
	if spec.KernelBootArgs == "" {
		spec.KernelBootArgs = identity
	} else {
		spec.KernelBootArgs += " " + identity
	}
	return json.Marshal(spec)
}

func containsReservedGuestKernelArgs(value string) bool {
	for _, field := range strings.Fields(value) {
		if strings.HasPrefix(field, "hako.") {
			return true
		}
	}
	return false
}

func validLocalGatewayOrigin(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Path == "" && parsed.RawQuery == "" && parsed.Fragment == "" && parsed.Hostname() != ""
}

var _ LocalLaunchSpecResolver = (*FirecrackerLocalWorkspaceLaunchResolver)(nil)
