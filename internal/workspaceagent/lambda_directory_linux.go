//go:build linux

package workspaceagent

import (
	"context"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const workspaceRuntimeLookupTimeout = 10 * time.Second

type WorkspaceRuntimeJournalReader interface {
	Get(context.Context, domain.TenantID, domain.WorkspaceID, domain.ResourcePlaneID) (workspacejournal.Snapshot, error)
}

type WorkspaceLambdaMicroVMReader interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
}

// WorkspaceLambdaMicroVMStreamDirectory resolves the current running provider
// allocation from the Resource Plane's durable journal for every new stream.
// This avoids stale process-local registrations and works across Gateway
// replicas; the journal, not a client hostname or registration event, selects
// the MicroVM.
type WorkspaceLambdaMicroVMStreamDirectory struct {
	ResourcePlaneID domain.ResourcePlaneID
	Journal         WorkspaceRuntimeJournalReader
	MicroVMs        WorkspaceLambdaMicroVMReader
	Tokens          WorkspaceAgentTokenProvider
	Tickets         WorkspaceAgentTicketIssuer
	Region          string
	Origin          string
	WebSocket       WorkspaceWebSocketDialer
}

func (d *WorkspaceLambdaMicroVMStreamDirectory) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionShell)
}

func (d *WorkspaceLambdaMicroVMStreamDirectory) OpenVSCode(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionVSCode)
}

func (d *WorkspaceLambdaMicroVMStreamDirectory) OpenPort(ctx context.Context, claims workspacesession.Claims, port uint16) (io.ReadWriteCloser, error) {
	if port != 22 || !hasWorkspacePermission(claims.Permissions, workspacesession.PermissionSSHProxy) {
		return nil, ErrInteractiveChannelUnavailable
	}
	return d.open(ctx, claims, workspacesession.PermissionSSHProxy)
}

func (d *WorkspaceLambdaMicroVMStreamDirectory) open(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission) (io.ReadWriteCloser, error) {
	if d == nil || ctx == nil || claims.UserID == "" || claims.TenantID == "" || claims.WorkspaceID == "" ||
		claims.ResourcePlane == "" || d.ResourcePlaneID == "" || claims.ResourcePlane != d.ResourcePlaneID ||
		!hasWorkspacePermission(claims.Permissions, permission) ||
		(permission != workspacesession.PermissionShell && permission != workspacesession.PermissionVSCode && permission != workspacesession.PermissionSSHProxy) || d.Journal == nil || d.MicroVMs == nil ||
		d.Tokens == nil || d.Tickets == nil || !validWorkspaceRegion(d.Region) || !validGatewayOrigin(d.Origin) {
		return nil, ErrInteractiveChannelUnavailable
	}
	lookupCtx, cancel := context.WithTimeout(ctx, workspaceRuntimeLookupTimeout)
	defer cancel()
	snapshot, err := d.Journal.Get(lookupCtx, claims.TenantID, claims.WorkspaceID, claims.ResourcePlane)
	if err != nil || snapshot.ETag == "" || snapshot.Journal.Validate() != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	journal := snapshot.Journal
	if journal.TenantID != claims.TenantID || journal.WorkspaceID != claims.WorkspaceID || journal.ResourcePlaneID != claims.ResourcePlane ||
		journal.State != workspacejournal.Running || journal.Tombstone || journal.ProviderID == "" {
		return nil, ErrInteractiveChannelUnavailable
	}
	vm, err := d.MicroVMs.GetMicrovm(lookupCtx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(journal.ProviderID)})
	if err != nil || vm == nil || aws.ToString(vm.MicrovmId) != journal.ProviderID || vm.State != types.MicrovmStateRunning {
		return nil, ErrInteractiveChannelUnavailable
	}
	dialer, err := NewLambdaMicroVMGatewayDialer(LambdaMicroVMGatewayDialerConfig{
		Endpoint: WorkspaceRuntimeEndpoint{TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID,
			ResourcePlane: claims.ResourcePlane, MicroVMID: journal.ProviderID, Region: d.Region, Endpoint: aws.ToString(vm.Endpoint)},
		Token: d.Tokens, Ticket: d.Tickets, Origin: d.Origin, WebSocket: d.WebSocket,
	})
	if err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	return dialer.open(lookupCtx, claims, permission)
}

var _ WorkspaceStreamDialer = (*WorkspaceLambdaMicroVMStreamDirectory)(nil)
