//go:build linux

package previewcontroller

import (
	"context"
	"errors"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var ErrPreviewSourceInstall = errors.New("Preview source could not be installed in the Workspace")

type previewMicroVMAPI interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
}

type LambdaPreviewSourceInstaller struct {
	plane     domain.ResourcePlaneID
	region    string
	origin    string
	api       previewMicroVMAPI
	tokens    workspaceagent.WorkspaceAgentTokenProvider
	tickets   workspaceagent.WorkspaceAgentTicketIssuer
	reader    workloadsource.SourceArchiveReader
	websocket workspaceagent.WorkspaceWebSocketDialer
}

type LambdaPreviewSourceInstallerConfig struct {
	ResourcePlaneID domain.ResourcePlaneID
	Region          string
	GatewayOrigin   string
	MicroVMs        previewMicroVMAPI
	Tokens          workspaceagent.WorkspaceAgentTokenProvider
	Tickets         workspaceagent.WorkspaceAgentTicketIssuer
	Archives        workloadsource.SourceArchiveReader
	WebSocket       workspaceagent.WorkspaceWebSocketDialer
}

func NewLambdaPreviewSourceInstaller(config LambdaPreviewSourceInstallerConfig) (*LambdaPreviewSourceInstaller, error) {
	if !resultPlaneIDPattern.MatchString(string(config.ResourcePlaneID)) || strings.TrimSpace(config.Region) == "" ||
		config.MicroVMs == nil || config.Tokens == nil || config.Tickets == nil || config.Archives == nil {
		return nil, ErrPreviewSourceInstall
	}
	return &LambdaPreviewSourceInstaller{plane: config.ResourcePlaneID, region: strings.TrimSpace(config.Region),
		origin: strings.TrimSpace(config.GatewayOrigin), api: config.MicroVMs, tokens: config.Tokens,
		tickets: config.Tickets, reader: config.Archives, websocket: config.WebSocket}, nil
}

func (i *LambdaPreviewSourceInstaller) Install(ctx context.Context, command previewprotocol.Command, binding Binding,
	source workloadsource.MaterializedSource) error {
	if i == nil || ctx == nil || ctx.Err() != nil || command.Action != previewprotocol.ActionProvision ||
		command.ResourcePlaneID != string(i.plane) || !safeProviderID(binding.MicroVMID) || binding.GatewayRouteID != previewGatewayRouteID(command) {
		return ErrPreviewSourceInstall
	}
	identity := workloadjournal.Command{TenantID: command.TenantID, WorkloadRunID: command.RuntimeWorkspaceID,
		AttemptID: command.ClaimID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID}
	if !validPreviewSource(source, identity) {
		return ErrPreviewSourceInstall
	}
	archive, err := i.reader.OpenSourceArchive(ctx, identity, source.ArtifactPrefix, source.Digest)
	if err != nil || archive.Body == nil || archive.Size < 1 || archive.Size > workloadsource.DefaultMaxArchiveBytes {
		if archive.Body != nil {
			_ = archive.Body.Close()
		}
		return ErrPreviewSourceInstall
	}
	defer archive.Body.Close()
	vm, err := i.api.GetMicrovm(ctx, &lambdamicrovms.GetMicrovmInput{MicrovmIdentifier: aws.String(binding.MicroVMID)})
	if err != nil || vm == nil || aws.ToString(vm.MicrovmId) != binding.MicroVMID || aws.ToString(vm.Endpoint) == "" {
		return ErrPreviewSourceInstall
	}
	dialer, err := workspaceagent.NewLambdaMicroVMGatewayDialer(workspaceagent.LambdaMicroVMGatewayDialerConfig{
		Endpoint: workspaceagent.WorkspaceRuntimeEndpoint{TenantID: domain.TenantID(command.TenantID),
			WorkspaceID: domain.WorkspaceID(command.RuntimeWorkspaceID), ResourcePlane: domain.ResourcePlaneID(command.ResourcePlaneID),
			MicroVMID: binding.MicroVMID, Region: i.region, Endpoint: aws.ToString(vm.Endpoint)},
		Token: i.tokens, Ticket: i.tickets, Origin: i.origin, WebSocket: i.websocket,
	})
	if err != nil {
		return ErrPreviewSourceInstall
	}
	claims := workspacesession.Claims{UserID: domain.UserID("hako-preview-runtime"), TenantID: domain.TenantID(command.TenantID),
		WorkspaceID: domain.WorkspaceID(command.RuntimeWorkspaceID), ResourcePlane: domain.ResourcePlaneID(command.ResourcePlaneID),
		Permissions: []workspacesession.Permission{workspacesession.PermissionFileTransfer}}
	stream, err := dialer.OpenFileTransfer(ctx, claims)
	if err != nil {
		return ErrPreviewSourceInstall
	}
	installErr := workspaceagent.TransferSourceArchive(ctx, stream, archive, source.Digest)
	_ = stream.Close()
	if installErr != nil {
		return ErrPreviewSourceInstall
	}
	startStream, err := dialer.OpenFileTransfer(ctx, claims)
	if err != nil {
		return ErrPreviewSourceInstall
	}
	defer startStream.Close()
	if _, err := workspaceagent.StartPreviewApplication(ctx, startStream); err != nil {
		return ErrPreviewSourceInstall
	}
	return nil
}

var _ PreviewSourceInstaller = (*LambdaPreviewSourceInstaller)(nil)
