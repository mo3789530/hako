package workspacegateway

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

type activeLookup struct {
	active bool
	err    error
}

type lockedBuffer struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *lockedBuffer) Write(value []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(value)
}

func (b *lockedBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.Buffer.Bytes()...)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func (l activeLookup) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	return l.active, l.err
}

type revokingActiveLookup struct {
	calls atomic.Int32
	err   error
}

func (l *revokingActiveLookup) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	if l.calls.Add(1) == 1 {
		return true, nil
	}
	return false, l.err
}

type portAuthorizer struct {
	allow bool
	err   error
	port  atomic.Uint32
}

func (a *portAuthorizer) AllowPort(_ context.Context, _ workspacesession.Claims, port uint16) (bool, error) {
	a.port.Store(uint32(port))
	return a.allow, a.err
}

type testDialer struct {
	opens atomic.Int32
	port  atomic.Uint32
	err   error
}

type testGatewayStatusError int

func (e testGatewayStatusError) Error() string              { return "test connection admission error" }
func (e testGatewayStatusError) GatewayHTTPStatusCode() int { return int(e) }

func (d *testDialer) OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open()
}
func (d *testDialer) OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open()
}
func (d *testDialer) OpenFileTransfer(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open()
}
func (d *testDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open()
}
func (d *testDialer) OpenPort(_ context.Context, _ workspacesession.Claims, _ workspacesession.Permission, port uint16) (io.ReadWriteCloser, error) {
	d.port.Store(uint32(port))
	return d.open()
}
func (d *testDialer) open() (io.ReadWriteCloser, error) {
	d.opens.Add(1)
	if d.err != nil {
		return nil, d.err
	}
	gatewayEnd, runtimeEnd := net.Pipe()
	go func() { _, _ = io.Copy(runtimeEnd, runtimeEnd); _ = runtimeEnd.Close() }()
	return gatewayEnd, nil
}

func TestGatewayWSShellRoutesOnlyFromSignedTicketAndRelaysBytes(t *testing.T) {
	gateway, server, token, dialer := newTestGateway(t, Config{MaxConnections: 2, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial shell WSS: %v", err)
	}
	if err := websocket.Message.Send(client, []byte("terminal-data")); err != nil {
		t.Fatal(err)
	}
	var received []byte
	if err := websocket.Message.Receive(client, &received); err != nil {
		t.Fatalf("receive shell stream: %v", err)
	}
	if string(received) != "terminal-data" {
		t.Fatalf("stream payload=%q", received)
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
	if dialer.opens.Load() != 1 {
		t.Fatalf("runtime opens=%d, want 1", dialer.opens.Load())
	}
}

func TestGatewaySSHProxyUsesDedicatedPermissionAndFixedLoopbackPort22(t *testing.T) {
	gateway, server, _, dialer := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionSSHProxy})
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/ssh", token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial SSH proxy WSS: %v", err)
	}
	if err := websocket.Message.Send(client, []byte("ssh-protocol-bytes")); err != nil {
		t.Fatal(err)
	}
	var received []byte
	if err := websocket.Message.Receive(client, &received); err != nil {
		t.Fatalf("receive SSH proxy stream: %v", err)
	}
	if string(received) != "ssh-protocol-bytes" {
		t.Fatalf("SSH proxy payload=%q", received)
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
	if dialer.port.Load() != 22 || dialer.opens.Load() != 1 {
		t.Fatalf("SSH proxy destination port=%d opens=%d, want fixed port 22 and one stream", dialer.port.Load(), dialer.opens.Load())
	}
}

func TestGatewaySSHProxyRejectsTicketWithoutDedicatedPermission(t *testing.T) {
	_, server, shellToken, dialer := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/ssh", shellToken, "https://ide.example.test"); err == nil {
		t.Fatal("SSH proxy accepted a ticket without ssh-proxy permission")
	}
	if dialer.opens.Load() != 0 {
		t.Fatalf("runtime was opened before permission validation: %d", dialer.opens.Load())
	}
}

func TestGatewayFileTransferRequiresDedicatedPermissionAndRoutesFilesStream(t *testing.T) {
	gateway, server, _, dialer := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/files", issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell}), "https://ide.example.test"); err == nil {
		t.Fatal("file route accepted a shell-only ticket")
	}
	if dialer.opens.Load() != 0 {
		t.Fatalf("runtime was opened before permission validation: %d", dialer.opens.Load())
	}
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionFileTransfer})
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/files", token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial file-transfer WSS: %v", err)
	}
	if err := websocket.Message.Send(client, []byte("file-transfer-frame")); err != nil {
		t.Fatal(err)
	}
	var received []byte
	if err := websocket.Message.Receive(client, &received); err != nil || string(received) != "file-transfer-frame" {
		t.Fatalf("file-transfer relay=%q error=%v", received, err)
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
	if dialer.opens.Load() != 1 {
		t.Fatalf("file-transfer runtime opens=%d, want 1", dialer.opens.Load())
	}
}

func TestGatewayClientDialsTLSStreamWithShortLivedTicket(t *testing.T) {
	_, server, token, _ := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	serverTransport := server.Client().Transport.(*http.Transport)
	connection, err := Dial(context.Background(), server.URL, domain.WorkspaceID("ws_gateway_a"), workspacesession.PermissionShell, 0, token,
		&tls.Config{RootCAs: serverTransport.TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatalf("Gateway client dial: %v", err)
	}
	defer connection.Close()
	if _, err := connection.Write([]byte("hako-cli-stream")); err != nil {
		t.Fatal(err)
	}
	got := make([]byte, len("hako-cli-stream"))
	if _, err := io.ReadFull(connection, got); err != nil {
		t.Fatal(err)
	}
	if string(got) != "hako-cli-stream" {
		t.Fatalf("Gateway client received %q", got)
	}
}

func TestGatewayClientUsesSSHProxyRouteWithoutCallerPort(t *testing.T) {
	_, server, _, _ := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionSSHProxy})
	serverTransport := server.Client().Transport.(*http.Transport)
	connection, err := Dial(context.Background(), server.URL, domain.WorkspaceID("ws_gateway_a"), workspacesession.PermissionSSHProxy, 0,
		token, &tls.Config{RootCAs: serverTransport.TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatalf("dial SSH proxy endpoint: %v", err)
	}
	_ = connection.Close()
	if _, err := Dial(context.Background(), server.URL, domain.WorkspaceID("ws_gateway_a"), workspacesession.PermissionSSHProxy, 22,
		token, &tls.Config{RootCAs: serverTransport.TLSClientConfig.RootCAs}); err == nil {
		t.Fatal("SSH proxy client must not accept a caller-selected port")
	}
}

func TestGatewayClientUsesFileTransferRouteWithoutCallerPort(t *testing.T) {
	_, server, _, _ := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionFileTransfer})
	serverTransport := server.Client().Transport.(*http.Transport)
	connection, err := Dial(context.Background(), server.URL, domain.WorkspaceID("ws_gateway_a"), workspacesession.PermissionFileTransfer, 0,
		token, &tls.Config{RootCAs: serverTransport.TLSClientConfig.RootCAs})
	if err != nil {
		t.Fatalf("dial file-transfer endpoint: %v", err)
	}
	_ = connection.Close()
	if _, err := Dial(context.Background(), server.URL, domain.WorkspaceID("ws_gateway_a"), workspacesession.PermissionFileTransfer, 22,
		token, &tls.Config{RootCAs: serverTransport.TLSClientConfig.RootCAs}); err == nil {
		t.Fatal("file-transfer client must not accept a caller-selected port")
	}
}

func TestGatewayClientRejectsUnsafeConfiguration(t *testing.T) {
	tests := []struct {
		name       string
		endpoint   string
		workspace  domain.WorkspaceID
		permission workspacesession.Permission
		port       uint16
		ticket     string
		tlsConfig  *tls.Config
	}{
		{name: "plaintext", endpoint: "http://gateway.test", workspace: "ws_client", permission: workspacesession.PermissionShell, ticket: "ticket"},
		{name: "foreign URL path", endpoint: "https://gateway.test/other", workspace: "ws_client", permission: workspacesession.PermissionShell, ticket: "ticket"},
		{name: "caller query", endpoint: "https://gateway.test?host=evil", workspace: "ws_client", permission: workspacesession.PermissionShell, ticket: "ticket"},
		{name: "invalid workspace", endpoint: "https://gateway.test", workspace: "ws/other", permission: workspacesession.PermissionShell, ticket: "ticket"},
		{name: "missing port", endpoint: "https://gateway.test", workspace: "ws_client", permission: workspacesession.PermissionPortForward, ticket: "ticket"},
		{name: "insecure TLS", endpoint: "https://gateway.test", workspace: "ws_client", permission: workspacesession.PermissionShell, ticket: "ticket", tlsConfig: &tls.Config{InsecureSkipVerify: true}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if connection, err := Dial(context.Background(), test.endpoint, test.workspace, test.permission, test.port, test.ticket, test.tlsConfig); !errors.Is(err, ErrClientConfig) {
				if connection != nil {
					_ = connection.Close()
				}
				t.Fatalf("Dial error=%v, want ErrClientConfig", err)
			}
		})
	}
}

func TestGatewayWSShellConnectsToWorkspacePTY(t *testing.T) {
	root := filepath.Join(t.TempDir(), "workspace")
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	shells, err := workspaceagent.NewShellManager(workspaceagent.ShellConfig{WorkspaceRoot: root})
	if err != nil {
		t.Fatal(err)
	}
	defer shells.Close()
	dialer := &workspaceagent.GatewayDialer{WorkspaceID: "ws_gateway_a", Shells: shells}
	router := workspaceagent.NewWorkspaceRouter()
	if !router.Register("ws_gateway_a", dialer) {
		t.Fatal("register Workspace PTY runtime")
	}
	gateway, err := New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: newTestVerifier(t), Dialer: router,
		AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 2, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell}), "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial PTY WSS: %v", err)
	}
	defer client.Close()
	if err := websocket.Message.Send(client, []byte("printf 'hako-gateway-pty-ready\\n'\nexit\n")); err != nil {
		t.Fatalf("send PTY command: %v", err)
	}
	output := make(chan string, 1)
	go func() {
		var received strings.Builder
		for {
			var frame []byte
			if err := websocket.Message.Receive(client, &frame); err != nil {
				output <- received.String()
				return
			}
			received.Write(frame)
			if strings.Contains(received.String(), "hako-gateway-pty-ready") {
				output <- received.String()
				return
			}
		}
	}()
	select {
	case received := <-output:
		if !strings.Contains(received, "hako-gateway-pty-ready") {
			t.Fatalf("PTY stream ended without command output: %q", received)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for shell output through Gateway")
	}
	waitForConnections(t, gateway, 0)
}

func TestGatewayWSPortForwardConnectsOnlyToWorkspaceLocalPort(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	go func() {
		connection, err := listener.Accept()
		if err != nil {
			return
		}
		defer connection.Close()
		_, _ = io.Copy(connection, connection)
	}()
	port := uint16(listener.Addr().(*net.TCPAddr).Port)
	portPolicy := &portAuthorizer{allow: true}
	dialer := &workspaceagent.GatewayDialer{WorkspaceID: "ws_gateway_a"}
	gateway, err := New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: newTestVerifier(t), Dialer: dialer,
		PortAuthorizer: portPolicy, AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionPortForward})
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/ports/"+strconv.Itoa(int(port)), token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial port-forward WSS: %v", err)
	}
	defer client.Close()
	if portPolicy.port.Load() != uint32(port) {
		t.Fatalf("port policy saw %d, want %d", portPolicy.port.Load(), port)
	}
	if err := websocket.Message.Send(client, []byte("private-workspace-port")); err != nil {
		t.Fatal(err)
	}
	var received []byte
	if err := websocket.Message.Receive(client, &received); err != nil {
		t.Fatalf("receive forwarded bytes: %v", err)
	}
	if string(received) != "private-workspace-port" {
		t.Fatalf("forwarded response=%q", received)
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
}

func TestGatewayRejectsPermissionWorkspacePlaneOriginAndPlainHTTP(t *testing.T) {
	gateway, server, shellToken, dialer := newTestGateway(t, Config{MaxConnections: 2, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/vscode", shellToken, "https://ide.example.test"); err == nil {
		t.Fatal("shell-only ticket opened VS Code")
	}
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_b/shell", shellToken, "https://ide.example.test"); err == nil {
		t.Fatal("ticket for another Workspace spoofed the route")
	}
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", shellToken, "https://evil.example.test"); err == nil {
		t.Fatal("untrusted browser origin was accepted")
	}
	plainWebSocketRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/workspaces/ws_gateway_a/shell", nil)
	plainWebSocketRequest.Header.Set("Authorization", "Bearer "+shellToken)
	plainResponse, err := server.Client().Do(plainWebSocketRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = plainResponse.Body.Close()
	if plainResponse.StatusCode != http.StatusBadRequest {
		t.Fatalf("non-upgrade request status=%d", plainResponse.StatusCode)
	}
	if dialer.opens.Load() != 0 {
		t.Fatalf("unauthorized requests opened runtime %d times", dialer.opens.Load())
	}
	plainServer := httptest.NewServer(gateway.Handler())
	defer plainServer.Close()
	httpURL := plainServer.URL + "/v1/workspaces/ws_gateway_a/shell"
	request, _ := http.NewRequest(http.MethodGet, httpURL, nil)
	request.Header.Set("Authorization", "Bearer "+shellToken)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("plaintext HTTP status=%d", response.StatusCode)
	}
	if dialer.opens.Load() != 0 {
		t.Fatalf("plaintext request opened runtime")
	}
	_ = gateway
}

func TestGatewayAppliesPortPolicyBeforeUpgradeAndLimitsFrames(t *testing.T) {
	portPolicy := &portAuthorizer{allow: false}
	gateway, server, portToken, dialer := newTestGateway(t, Config{MaxConnections: 2, MaxConnectionsPerWorkspace: 1, PortAuthorizer: portPolicy})
	defer server.Close()
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/ports/5432", portToken, "https://ide.example.test"); err == nil {
		t.Fatal("port policy denial upgraded WebSocket")
	}
	if portPolicy.port.Load() != 5432 || dialer.opens.Load() != 0 {
		t.Fatalf("denied port policy=%d runtime opens=%d", portPolicy.port.Load(), dialer.opens.Load())
	}
	portPolicy.allow = true
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/ports/5432", portToken, "https://ide.example.test")
	if err != nil {
		t.Fatalf("allowed port WSS: %v", err)
	}
	if dialer.port.Load() != 5432 {
		t.Fatalf("runtime port=%d", dialer.port.Load())
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
}

func TestGatewayRejectsOversizedWebSocketFrames(t *testing.T) {
	gateway, server, token, dialer := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1, MaxPayloadBytes: 1024})
	defer server.Close()
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if err := websocket.Message.Send(client, make([]byte, 1025)); err != nil {
		t.Fatal(err)
	}
	var reply []byte
	if err := websocket.Message.Receive(client, &reply); err == nil {
		t.Fatal("oversized terminal frame was accepted")
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
	if dialer.opens.Load() != 1 {
		t.Fatalf("runtime opened %d times", dialer.opens.Load())
	}
}

func TestGatewayConnectionLimitAndUpstreamFailure(t *testing.T) {
	gateway, server, token, _ := newTestGateway(t, Config{MaxConnections: 2, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	first, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test"); err == nil {
		t.Fatal("per-Workspace connection limit was not enforced")
	}
	_ = first.Close()
	waitForConnections(t, gateway, 0)

	broken := &testDialer{err: errors.New("upstream secret must not leak")}
	gateway, err = New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: newTestVerifier(t), Dialer: broken,
		AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	brokenServer := httptest.NewTLSServer(gateway.Handler())
	defer brokenServer.Close()
	brokenToken := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell})
	request, _ := http.NewRequest(http.MethodGet, brokenServer.URL+"/v1/workspaces/ws_gateway_a/shell", nil)
	request.Header.Set("Authorization", "Bearer "+brokenToken)
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "keep-alive, Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	request.Header.Set("Sec-WebSocket-Protocol", "hako.workspace.v1")
	response, err := brokenServer.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusBadGateway {
		t.Fatalf("upstream failure status=%d body=%s", response.StatusCode, response.Body)
	}
}

func TestGatewayReturnsTooManyRequestsForSharedWorkspaceConnectionLimit(t *testing.T) {
	dialer := &testDialer{err: testGatewayStatusError(http.StatusTooManyRequests)}
	gateway, err := New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: newTestVerifier(t), Dialer: dialer,
		AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 2, MaxConnectionsPerWorkspace: 2})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "https://gateway.example.test/v1/workspaces/ws_gateway_a/shell", nil)
	request.Header.Set("Authorization", "Bearer "+issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell}))
	request.Header.Set("Origin", "https://ide.example.test")
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Version", "13")
	request.Header.Set("Sec-WebSocket-Key", base64.StdEncoding.EncodeToString([]byte("1234567890123456")))
	request.Header.Set("Sec-WebSocket-Protocol", "hako.workspace.v1")
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusTooManyRequests {
		t.Fatalf("Gateway status=%d body=%s, want 429", response.Code, response.Body.String())
	}
}

func TestGatewayEmitsRedactedCloudWatchEMFConnectionMetrics(t *testing.T) {
	var metrics lockedBuffer
	gateway, server, token, _ := newTestGateway(t, Config{
		MaxConnections: 2, MaxConnectionsPerWorkspace: 1, MetricsWriter: &metrics,
	})
	defer server.Close()
	client, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("dial Gateway: %v", err)
	}
	_ = client.Close()
	waitForConnections(t, gateway, 0)
	deadline := time.Now().Add(time.Second)
	for len(metrics.Bytes()) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}

	var event map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(metrics.Bytes()), &event); err != nil {
		t.Fatalf("decode EMF metric: %v; raw=%s", err, metrics.String())
	}
	if event["ResourcePlaneId"] != "rp_gateway_a" || event["Channel"] != string(workspacesession.PermissionShell) || event["Outcome"] != "disconnected" {
		t.Fatalf("unexpected metric dimensions: %#v", event)
	}
	if event["ConnectionEvents"] != float64(1) || event["RejectedConnections"] != float64(0) || event["ActiveConnections"] != float64(0) {
		t.Fatalf("unexpected metric values: %#v", event)
	}
	if strings.Contains(metrics.String(), token) || strings.Contains(metrics.String(), "tenant_gateway") || strings.Contains(metrics.String(), "ws_gateway_a") {
		t.Fatalf("metric contains a credential or high-cardinality identity: %s", metrics.String())
	}
	aws, ok := event["_aws"].(map[string]any)
	if !ok || aws["CloudWatchMetrics"] == nil {
		t.Fatalf("missing CloudWatch EMF directive: %#v", event["_aws"])
	}
}

func TestGatewayRechecksAndClosesActiveStreamAfterSessionRevocation(t *testing.T) {
	lookup := &revokingActiveLookup{}
	verifier := &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"gateway-test": testPublicKey()},
		Audience: workspacesession.AudienceGateway, Lookup: lookup}
	gateway, err := New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: verifier, Dialer: &testDialer{},
		AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 2, MaxConnectionsPerWorkspace: 2,
		SessionRecheckInterval: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	connection, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell",
		issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell}), "https://ide.example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := websocket.Message.Send(connection, []byte("before-revocation")); err != nil {
		t.Fatal(err)
	}
	var echoed []byte
	if err := websocket.Message.Receive(connection, &echoed); err != nil || string(echoed) != "before-revocation" {
		t.Fatalf("authorized stream before revocation = %q, %v", echoed, err)
	}
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var discarded []byte
	if err := websocket.Message.Receive(connection, &discarded); err == nil {
		t.Fatal("revoked active Session stream remained open")
	}
	waitForConnections(t, gateway, 0)
	if got := lookup.calls.Load(); got < 2 {
		t.Fatalf("active Session lookup calls=%d, want initial authorization and stream recheck", got)
	}
}

func TestGatewayFailsClosedWhenActiveSessionRecheckIsUnavailable(t *testing.T) {
	lookup := &revokingActiveLookup{err: errors.New("synthetic lookup failure")}
	var logs lockedBuffer
	verifier := &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"gateway-test": testPublicKey()},
		Audience: workspacesession.AudienceGateway, Lookup: lookup}
	gateway, err := New(Config{ResourcePlaneID: "rp_gateway_a", Verifier: verifier, Dialer: &testDialer{},
		AllowedOrigins: []string{"https://ide.example.test"}, MaxConnections: 2, MaxConnectionsPerWorkspace: 2,
		SessionRecheckInterval: 20 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(&logs, nil))})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	connection, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell",
		issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell}), "https://ide.example.test")
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()
	if err := connection.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	var discarded []byte
	if err := websocket.Message.Receive(connection, &discarded); err == nil {
		t.Fatal("active stream remained open after Session lookup failure")
	}
	waitForConnections(t, gateway, 0)
	if got := lookup.calls.Load(); got < 2 {
		t.Fatalf("active Session lookup calls=%d, want initial authorization and stream recheck", got)
	}
	deadline := time.Now().Add(time.Second)
	for !strings.Contains(logs.String(), "event=workspace_gateway.connection") && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !strings.Contains(logs.String(), "failure_class=session_recheck") || !strings.Contains(logs.String(), "workspace_id=ws_gateway_a") || strings.Contains(logs.String(), "ticket_id=") || strings.Contains(logs.String(), "synthetic lookup failure") {
		t.Fatalf("Gateway failure log must include trusted identity without credentials/raw errors: %s", logs.String())
	}
}

func TestGatewayRejectsConcurrentTicketReplayAndAllowsReconnect(t *testing.T) {
	gateway, server, token, _ := newTestGateway(t, Config{MaxConnections: 2, MaxConnectionsPerWorkspace: 2})
	defer server.Close()
	first, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test"); err == nil {
		t.Fatal("concurrent connection replayed the same ticket")
	}
	_ = first.Close()
	waitForConnections(t, gateway, 0)
	reconnected, err := dialWorkspace(server.URL, "/v1/workspaces/ws_gateway_a/shell", token, "https://ide.example.test")
	if err != nil {
		t.Fatalf("ticket reconnect after prior connection closed: %v", err)
	}
	_ = reconnected.Close()
	waitForConnections(t, gateway, 0)
}

func TestGatewayHealthAndConfigValidation(t *testing.T) {
	if _, err := New(Config{ResourcePlaneID: "rp1", Verifier: newTestVerifier(t), Dialer: &testDialer{}, MaxConnections: 2, MaxConnectionsPerWorkspace: 3}); !errors.Is(err, ErrConfig) {
		t.Fatalf("per-Workspace cap greater than total accepted: %v", err)
	}
	if _, err := New(Config{ResourcePlaneID: "rp1", Verifier: newTestVerifier(t), Dialer: &testDialer{}, AllowedOrigins: []string{"https://example.test/path"}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1}); !errors.Is(err, ErrConfig) {
		t.Fatalf("origin URL with path accepted: %v", err)
	}
	gateway, server, _, _ := newTestGateway(t, Config{MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	defer server.Close()
	request := httptest.NewRequest(http.MethodGet, "https://gateway.example.test/healthz", nil)
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("health response=%d headers=%v", response.Code, response.Header())
	}
}

func newTestGateway(t *testing.T, config Config) (*Gateway, *httptest.Server, string, *testDialer) {
	t.Helper()
	dialer := &testDialer{}
	if config.MaxConnections == 0 {
		config.MaxConnections = 4
	}
	if config.MaxConnectionsPerWorkspace == 0 {
		config.MaxConnectionsPerWorkspace = 2
	}
	config.ResourcePlaneID = "rp_gateway_a"
	config.Verifier = newTestVerifier(t)
	config.Dialer = dialer
	if config.AllowedOrigins == nil {
		config.AllowedOrigins = []string{"https://ide.example.test"}
	}
	gateway, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	token := issueTestTicket(t, []workspacesession.Permission{workspacesession.PermissionShell, workspacesession.PermissionPortForward})
	return gateway, server, token, dialer
}

func dialWorkspace(serverURL, path, token, origin string) (*websocket.Conn, error) {
	location := "wss" + strings.TrimPrefix(serverURL, "https") + path
	config, err := websocket.NewConfig(location, origin)
	if err != nil {
		return nil, err
	}
	config.Protocol = []string{"hako.workspace.v1"}
	config.Header.Set("Authorization", "Bearer "+token)
	config.TlsConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // Test server uses an ephemeral self-signed certificate.
	return websocket.DialConfig(config)
}

func newTestVerifier(t *testing.T) *workspacesession.Verifier {
	t.Helper()
	public := testPublicKey()
	return &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"gateway-test": public}, Audience: workspacesession.AudienceGateway,
		Lookup: activeLookup{active: true}}
}

func issueTestTicket(t *testing.T, permissions []workspacesession.Permission) string {
	t.Helper()
	issuer := &workspacesession.Ed25519Issuer{KeyID: "gateway-test", PrivateKey: testPrivateKey(), TTL: time.Minute}
	token, _, err := issuer.Issue(workspacesession.Claims{UserID: "usr_gateway", TenantID: "tenant_gateway", WorkspaceID: "ws_gateway_a",
		ResourcePlane: domain.ResourcePlaneID("rp_gateway_a"), Permissions: permissions})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

func testPrivateKey() ed25519.PrivateKey {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	return ed25519.NewKeyFromSeed(seed)
}

func testPublicKey() ed25519.PublicKey { return testPrivateKey().Public().(ed25519.PublicKey) }

func waitForConnections(t *testing.T, gateway *Gateway, want int) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		active, _ := gateway.ActiveConnections()
		if active == want {
			return
		}
		select {
		case <-deadline:
			t.Fatalf("active Gateway connections=%d want %d", active, want)
		case <-time.After(10 * time.Millisecond):
		}
	}
}
