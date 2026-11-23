//go:build linux

package workspaceagent

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

func TestCloudHypervisorWorkspaceVsockDirectoryOpensSignedAgentStream(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &AgentTicketIssuer{KeyID: "local-gateway-key", PrivateKey: privateKey, TTL: time.Minute}
	claims := workspacesession.Claims{UserID: "user_local", TenantID: "tenant_local", WorkspaceID: "workspace_local",
		ResourcePlane: "rp_local", TicketID: "gateway_session_local", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	providerID := "ch_" + strings.Repeat("a", 32)
	journal := newRunningWorkspaceJournal(t, claims, providerID)
	journalReader := &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: journal, ETag: `"running-v1"`}}
	stateRoot := t.TempDir()
	if err := os.Chmod(stateRoot, 0700); err != nil {
		t.Fatal(err)
	}
	allocationDir := filepath.Join(stateRoot, providerID)
	if err := os.Mkdir(allocationDir, 0700); err != nil {
		t.Fatal(err)
	}

	const gatewayOrigin = "https://gateway.local.test"
	server := httptest.NewServer(websocket.Server{
		Config: websocket.Config{Protocol: []string{workspaceAgentWebSocketSubprotocol}},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.URL.Path != "/v1/workspaces/workspace_local/shell" || request.Header.Get("Origin") != gatewayOrigin ||
				!offeredAgentProtocol(request.Header.Get("Sec-WebSocket-Protocol")) {
				t.Errorf("unexpected local Agent WebSocket request: path=%q origin=%q protocol=%q", request.URL.Path,
					request.Header.Get("Origin"), request.Header.Get("Sec-WebSocket-Protocol"))
				return ErrAgentTicket
			}
			expected := AgentTicket{UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID,
				ResourcePlane: claims.ResourcePlane, MicroVMID: providerID, Permission: workspacesession.PermissionShell}
			if _, err := VerifyAgentTicket(strings.TrimPrefix(request.Header.Get("Authorization"), "Bearer "),
				map[string]ed25519.PublicKey{"local-gateway-key": publicKey}, expected, time.Now().UTC()); err != nil {
				t.Errorf("local Agent ticket did not verify: %v", err)
				return err
			}
			config.Protocol = []string{workspaceAgentWebSocketSubprotocol}
			return nil
		},
		Handler: func(connection *websocket.Conn) {
			connection.PayloadType = websocket.BinaryFrame
			_ = websocket.Message.Send(connection, []byte("agent-stream-ready"))
		},
	})
	defer server.Close()
	serverAddress := strings.TrimPrefix(server.URL, "http://")
	dialCalls := 0
	directory := &CloudHypervisorWorkspaceVsockStreamDirectory{
		ResourcePlaneID: claims.ResourcePlane, Journal: journalReader, StateDirectory: stateRoot,
		Tickets: issuer, Origin: gatewayOrigin,
		Dial: func(ctx context.Context, stateDirectory string) (net.Conn, error) {
			dialCalls++
			if stateDirectory != allocationDir {
				t.Fatalf("vsock was dialed in caller-selected state directory %q", stateDirectory)
			}
			if _, ok := ctx.Deadline(); !ok {
				t.Fatal("vsock dial has no deadline")
			}
			return (&net.Dialer{}).DialContext(ctx, "tcp", serverAddress)
		},
	}
	stream, err := directory.OpenShell(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.Close()
	var payload []byte
	if err := websocket.Message.Receive(stream.(*contextWorkspaceStream).stream.(*websocket.Conn), &payload); err != nil || string(payload) != "agent-stream-ready" {
		t.Fatalf("received local Guest stream %q, err=%v", payload, err)
	}
	if dialCalls != 1 || journalReader.calls != 1 {
		t.Fatalf("local directory lookups: dial=%d journal=%d", dialCalls, journalReader.calls)
	}
}

func TestCloudHypervisorWorkspaceVsockDirectoryRejectsForeignAndNonrunningBinding(t *testing.T) {
	claims := workspacesession.Claims{UserID: "user_local", TenantID: "tenant_local", WorkspaceID: "workspace_local",
		ResourcePlane: "rp_local", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	providerID := "ch_" + strings.Repeat("b", 32)
	journal := newRunningWorkspaceJournal(t, claims, providerID)
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	called := 0
	directory := &CloudHypervisorWorkspaceVsockStreamDirectory{ResourcePlaneID: claims.ResourcePlane,
		Journal:        &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: journal, ETag: "active"}},
		StateDirectory: root, Tickets: &AgentTicketIssuer{}, Origin: "https://gateway.local.test",
		Dial: func(context.Context, string) (net.Conn, error) { called++; return nil, io.EOF },
	}
	foreign := claims
	foreign.ResourcePlane = "rp_foreign"
	if _, err := directory.OpenShell(context.Background(), foreign); err == nil {
		t.Fatal("foreign Resource Plane claim was accepted")
	}
	if called != 0 {
		t.Fatal("foreign claim reached the Guest dialer")
	}

	suspended := journal
	suspended.State = workspacejournal.Suspended
	directory.Journal = &workspaceJournalReaderStub{snapshot: workspacejournal.Snapshot{Journal: suspended, ETag: "suspended"}}
	if _, err := directory.OpenShell(context.Background(), claims); err == nil {
		t.Fatal("suspended allocation accepted a stream")
	}
	if called != 0 {
		t.Fatal("nonrunning allocation reached the Guest dialer")
	}
}
