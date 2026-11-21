//go:build linux

package previewcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net"
	"reflect"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type previewInstallerMicroVMAPI struct{}

func (previewInstallerMicroVMAPI) GetMicrovm(_ context.Context, input *lambdamicrovms.GetMicrovmInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error) {
	if aws.ToString(input.MicrovmIdentifier) != "vm-preview" {
		return nil, ErrPreviewSourceInstall
	}
	return &lambdamicrovms.GetMicrovmOutput{MicrovmId: aws.String("vm-preview"), Endpoint: aws.String("https://vm-preview.lambda-microvm.us-east-1.on.aws")}, nil
}

type previewInstallerToken struct{}

func (previewInstallerToken) Create(_ context.Context, vmID string) (string, error) {
	if vmID != "vm-preview" {
		return "", ErrPreviewSourceInstall
	}
	return "short-agent-token", nil
}

type previewInstallerTickets struct{ calls int }

func (t *previewInstallerTickets) Issue(ticket workspaceagent.AgentTicket) (string, workspaceagent.AgentTicket, error) {
	t.calls++
	ticket.TicketID = "ticket-" + string(rune('0'+t.calls))
	ticket.UserID = domain.UserID("controller")
	ticket.IssuedAt = time.Now().Add(-time.Second).Unix()
	ticket.ExpiresAt = time.Now().Add(time.Minute).Unix()
	return "agent-capability-" + string(rune('0'+t.calls)), ticket, nil
}

type previewInstallerArchiveReader struct {
	data []byte
	id   workloadjournal.Command
}

func (r *previewInstallerArchiveReader) OpenSourceArchive(_ context.Context, id workloadjournal.Command, _ string, _ string) (workloadsource.SourceArchive, error) {
	r.id = id
	return workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(r.data)), Size: int64(len(r.data))}, nil
}

type previewInstallerWebSocket struct {
	operations []string
	archive    []byte
}

func (w *previewInstallerWebSocket) Dial(_ context.Context, endpoint, workspaceID, origin, awsToken, ticket string, permission workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	if endpoint != "https://vm-preview.lambda-microvm.us-east-1.on.aws" || workspaceID != "prvws_0123456789abcdef0123456789abcdef" ||
		origin != "https://gateway.hako.test" || awsToken != "short-agent-token" || ticket == "" ||
		permission != workspacesession.PermissionFileTransfer || port != 0 {
		return nil, ErrPreviewSourceInstall
	}
	client, guest := net.Pipe()
	go func() {
		defer guest.Close()
		request, err := workspaceprotocol.ReadFileRequest(guest)
		if err != nil {
			return
		}
		w.operations = append(w.operations, request.Operation)
		switch request.Operation {
		case "install_archive":
			if request.Path != "/workspace/src" || request.Size != int64(len(w.archive)) {
				return
			}
			body := make([]byte, request.Size)
			if _, err := io.ReadFull(guest, body); err != nil || !bytes.Equal(body, w.archive) {
				return
			}
			_ = workspaceprotocol.WriteFileResponse(guest, workspaceprotocol.FileResponse{OK: true, Size: request.Size})
		case "start_preview":
			if request.Path != "/workspace/src/.hako/preview.yaml" {
				return
			}
			_ = workspaceprotocol.WriteFileResponse(guest, workspaceprotocol.FileResponse{OK: true, Port: previewprotocol.ApplicationPort})
		}
	}()
	return client, nil
}

func TestLambdaPreviewSourceInstallerInstallsPinnedArchiveThenStartsGuestApplication(t *testing.T) {
	now := time.Now().UTC()
	command := validCommand(now, previewprotocol.ActionProvision)
	archiveBytes := []byte("immutable-preview-source-archive")
	digest := sha256.Sum256(archiveBytes)
	identity := workloadjournal.Command{TenantID: command.TenantID, WorkloadRunID: command.RuntimeWorkspaceID,
		AttemptID: command.ClaimID, Generation: command.Generation, ResourcePlaneID: command.ResourcePlaneID}
	archive := &previewInstallerArchiveReader{data: archiveBytes}
	tickets := &previewInstallerTickets{}
	websocket := &previewInstallerWebSocket{archive: archiveBytes}
	installer, err := NewLambdaPreviewSourceInstaller(LambdaPreviewSourceInstallerConfig{
		ResourcePlaneID: domain.ResourcePlaneID(command.ResourcePlaneID), Region: "us-east-1", GatewayOrigin: "https://gateway.hako.test",
		MicroVMs: previewInstallerMicroVMAPI{}, Tokens: previewInstallerToken{}, Tickets: tickets, Archives: archive, WebSocket: websocket,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := installer.Install(context.Background(), command,
		Binding{MicroVMID: "vm-preview", GatewayRouteID: previewGatewayRouteID(command)},
		workloadsource.MaterializedSource{Digest: hex.EncodeToString(digest[:]), ArtifactPrefix: "s3://source-bucket/previews/tenant_1/prvws_0123456789abcdef0123456789abcdef/preview-claim-1234/"}); err != nil {
		t.Fatalf("install and start Preview source: %v", err)
	}
	if !reflect.DeepEqual(websocket.operations, []string{"install_archive", "start_preview"}) || tickets.calls != 2 || archive.id != identity {
		t.Fatalf("Guest Preview sequence operations=%v tickets=%d source identity=%+v want=%+v", websocket.operations, tickets.calls, archive.id, identity)
	}
}
