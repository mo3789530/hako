package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type workspaceShellCredentials string

func (c workspaceShellCredentials) Get(string, string) (string, error) { return string(c), nil }

type activeWorkspaceSession struct{}

func (activeWorkspaceSession) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	return true, nil
}

type shellTestDialer struct {
	opened chan struct{}
}

func (d shellTestDialer) OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	client, runtime := net.Pipe()
	if d.opened != nil {
		select {
		case d.opened <- struct{}{}:
		default:
		}
	}
	go func() {
		defer runtime.Close()
		buffer := make([]byte, 64*1024)
		var decoder workspaceprotocol.Decoder
		for {
			n, err := runtime.Read(buffer)
			if n > 0 {
				frames, decodeErr := decoder.Feed(buffer[:n])
				if decodeErr != nil {
					return
				}
				for _, decoded := range frames {
					if decoded.Resize {
						continue
					}
					frame, encodeErr := workspaceprotocol.EncodeData(decoded.Data)
					if encodeErr != nil {
						return
					}
					if _, writeErr := runtime.Write(frame); writeErr != nil {
						return
					}
					if strings.Contains(string(decoded.Data), string([]byte{4})) {
						return
					}
				}
			}
			if err != nil {
				return
			}
		}
	}()
	return client, nil
}

func (shellTestDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (shellTestDialer) OpenPort(context.Context, workspacesession.Claims, uint16) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}

type forwardTestDialer struct{}

func (forwardTestDialer) OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (forwardTestDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, workspacegateway.ErrClientConfig
}
func (forwardTestDialer) OpenPort(_ context.Context, _ workspacesession.Claims, port uint16) (io.ReadWriteCloser, error) {
	if port != 5432 && port != 22 {
		return nil, workspacegateway.ErrClientConfig
	}
	client, runtime := net.Pipe()
	go func() {
		defer runtime.Close()
		buffer := make([]byte, 1024)
		for {
			n, err := runtime.Read(buffer)
			if n > 0 {
				_, _ = runtime.Write(buffer[:n])
			}
			if err != nil {
				return
			}
		}
	}()
	return client, nil
}

type sshProxyOutput struct {
	chunks chan []byte
}

func (w sshProxyOutput) Write(data []byte) (int, error) {
	w.chunks <- append([]byte(nil), data...)
	return len(data), nil
}

func TestWorkspaceSSHProxyCLIUsesDedicatedTicketAndRevokes(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspacesession.Ed25519Issuer{KeyID: "ssh-proxy-test", PrivateKey: privateKey, TTL: time.Minute}
	ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: "usr_cli", TenantID: "tenant_cli", WorkspaceID: "ws_cli",
		ResourcePlane: "rp_cli", Permissions: []workspacesession.Permission{workspacesession.PermissionSSHProxy}})
	if err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Int32
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-access" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var request struct {
			Permissions []workspacesession.Permission `json:"permissions"`
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions" ||
			json.NewDecoder(r.Body).Decode(&request) != nil || len(request.Permissions) != 1 || request.Permissions[0] != workspacesession.PermissionSSHProxy {
			http.Error(w, "unexpected SSH session request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
			ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: []workspacesession.Permission{workspacesession.PermissionSSHProxy}})
	}))
	defer apiServer.Close()
	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: "rp_cli",
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"ssh-proxy-test": publicKey}, Audience: workspacesession.AudienceGateway, Lookup: activeWorkspaceSession{}},
		Dialer:   forwardTestDialer{}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
	ctx, cancel := context.WithCancel(context.Background())
	stdin, stdinWriter := io.Pipe()
	output := sshProxyOutput{chunks: make(chan []byte, 1)}
	done := make(chan error, 1)
	go func() {
		done <- workspaceSSHProxyWith(ctx, "tenant_cli", "ws_cli", stdin, output, apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs})
	}()
	if _, err := stdinWriter.Write([]byte("ssh-client-protocol")); err != nil {
		t.Fatal(err)
	}
	select {
	case chunk := <-output.chunks:
		if string(chunk) != "ssh-client-protocol" {
			t.Fatalf("SSH proxy output=%q", chunk)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSH proxy did not relay bytes through Gateway")
	}
	cancel()
	_ = stdinWriter.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled SSH proxy returned error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SSH proxy did not stop after cancellation")
	}
	if revoked.Load() != 1 {
		t.Fatalf("SSH proxy ticket revoke count=%d, want 1", revoked.Load())
	}
}

type allowForwardTestPort struct{}

func (allowForwardTestPort) AllowPort(_ context.Context, _ workspacesession.Claims, port uint16) (bool, error) {
	return port == 5432, nil
}

func TestWorkspaceShellCLIRequestsTicketStreamsAndRevokes(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", "")
	t.Setenv("HAKO_GATEWAY_URL", "")

	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspacesession.Ed25519Issuer{KeyID: "cli-test", PrivateKey: privateKey, TTL: time.Minute}
	ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: "usr_cli", TenantID: "tenant_cli", WorkspaceID: "ws_cli",
		ResourcePlane: "rp_cli", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}})
	if err != nil {
		t.Fatal(err)
	}
	var created, revoked atomic.Int32
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-access" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions/"+claims.TicketID && r.Method == http.MethodDelete {
			http.Error(w, "wrong revoke route", http.StatusNotFound)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions" {
				http.Error(w, "wrong create route", http.StatusNotFound)
				return
			}
			var request struct {
				Permissions []workspacesession.Permission `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Permissions) != 1 || request.Permissions[0] != workspacesession.PermissionShell {
				http.Error(w, "wrong permissions", http.StatusBadRequest)
				return
			}
			created.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
				ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: []workspacesession.Permission{workspacesession.PermissionShell}})
		case http.MethodDelete:
			revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer apiServer.Close()

	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: "rp_cli",
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"cli-test": publicKey}, Audience: workspacesession.AudienceGateway, Lookup: activeWorkspaceSession{}},
		Dialer:   shellTestDialer{}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
	var output strings.Builder
	err = workspaceShellWith(context.Background(), "tenant_cli", "ws_cli", strings.NewReader("echo hako-cli-shell\n"), &output,
		apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs})
	if err != nil {
		t.Fatalf("workspaceShellWith: %v", err)
	}
	if created.Load() != 1 || revoked.Load() != 1 {
		t.Fatalf("session lifecycle create=%d revoke=%d", created.Load(), revoked.Load())
	}
	if !strings.Contains(output.String(), "echo hako-cli-shell") {
		t.Fatalf("shell output did not reach CLI: %q", output.String())
	}
}

func TestWorkspaceShellCLICancellationClosesStreamAndRevokesSession(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspacesession.Ed25519Issuer{KeyID: "cancel-test", PrivateKey: privateKey, TTL: time.Minute}
	ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: "usr_cli", TenantID: "tenant_cli", WorkspaceID: "ws_cli",
		ResourcePlane: "rp_cli", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}})
	if err != nil {
		t.Fatal(err)
	}
	var revoked atomic.Int32
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer cli-access" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodDelete {
			revoked.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
			ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: []workspacesession.Permission{workspacesession.PermissionShell}})
	}))
	defer apiServer.Close()
	opened := make(chan struct{}, 1)
	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: "rp_cli",
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"cancel-test": publicKey}, Audience: workspacesession.AudienceGateway, Lookup: activeWorkspaceSession{}},
		Dialer:   shellTestDialer{opened: opened}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stdin, stdinWriter := io.Pipe()
	defer stdinWriter.Close()
	done := make(chan error, 1)
	go func() {
		done <- workspaceShellWith(ctx, "tenant_cli", "ws_cli", stdin, io.Discard, apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs})
	}()
	select {
	case <-opened:
	case <-time.After(3 * time.Second):
		_ = stdin.Close()
		t.Fatal("Gateway did not open the Workspace shell")
	}
	cancel()
	_ = stdin.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("cancelled shell returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("shell did not stop after cancellation")
	}
	if revoked.Load() != 1 {
		t.Fatalf("session revoke count=%d, want 1", revoked.Load())
	}
}

func TestWorkspaceShellCLIRejectsMissingGatewayBeforeSessionCreation(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	t.Setenv("HAKO_API_URL", "https://api.example.test")
	t.Setenv("HAKO_GATEWAY_URL", "")
	if err := workspaceShellWith(context.Background(), "tenant_cli", "ws_cli", strings.NewReader(""), io.Discard, nil, nil); err == nil || !strings.Contains(err.Error(), "HAKO_GATEWAY_URL") {
		t.Fatalf("missing Gateway URL error=%v", err)
	}
}

func TestWorkspaceShellCLIDoesNotConnectWhenAuthorizationIsDenied(t *testing.T) {
	previousGetter := cliCredentialGetter
	cliCredentialGetter = workspaceShellCredentials(`{"access_token":"cli-access","token_type":"Bearer","expires_in":3600,"stored_at":"` + time.Now().UTC().Format(time.RFC3339) + `"}`)
	t.Cleanup(func() { cliCredentialGetter = previousGetter })
	var gatewayRequests atomic.Int32
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_foreign/sessions" {
			http.Error(w, "unexpected request", http.StatusNotFound)
			return
		}
		http.Error(w, "workspace access denied", http.StatusForbidden)
	}))
	defer apiServer.Close()
	gatewayServer := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		gatewayRequests.Add(1)
	}))
	defer gatewayServer.Close()
	t.Setenv("HAKO_API_URL", apiServer.URL)
	t.Setenv("HAKO_GATEWAY_URL", gatewayServer.URL)
	err := workspaceShellWith(context.Background(), "tenant_cli", "ws_foreign", strings.NewReader(""), io.Discard, apiServer.Client(), nil)
	if err == nil || !strings.Contains(err.Error(), "request Workspace shell session") {
		t.Fatalf("authorization denial error=%v", err)
	}
	if gatewayRequests.Load() != 0 {
		t.Fatalf("Gateway received %d requests after API authorization denial", gatewayRequests.Load())
	}
}

func TestWorkspaceForwardCLIUsesLoopbackAndPerConnectionSession(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &workspacesession.Ed25519Issuer{KeyID: "forward-test", PrivateKey: privateKey, TTL: time.Minute}
	var created, revoked atomic.Int32
	revokedSignal := make(chan struct{}, 1)
	apiServer := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer forward-access" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		switch r.Method {
		case http.MethodPost:
			if r.URL.Path != "/v1/tenants/tenant_cli/workspaces/ws_cli/sessions" {
				http.Error(w, "wrong create route", http.StatusNotFound)
				return
			}
			var request struct {
				Permissions []workspacesession.Permission `json:"permissions"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Permissions) != 1 || request.Permissions[0] != workspacesession.PermissionPortForward {
				http.Error(w, "wrong permissions", http.StatusBadRequest)
				return
			}
			ticket, claims, err := issuer.Issue(workspacesession.Claims{UserID: "usr_cli", TenantID: "tenant_cli", WorkspaceID: "ws_cli",
				ResourcePlane: "rp_cli", Permissions: []workspacesession.Permission{workspacesession.PermissionPortForward}})
			if err != nil {
				http.Error(w, "ticket issue failed", http.StatusInternalServerError)
				return
			}
			created.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(api.WorkspaceSession{SessionID: domain.SessionID(claims.TicketID), Ticket: ticket,
				ExpiresAt: time.Unix(claims.ExpiresAt, 0).UTC(), Permissions: []workspacesession.Permission{workspacesession.PermissionPortForward}})
		case http.MethodDelete:
			revoked.Add(1)
			select {
			case revokedSignal <- struct{}{}:
			default:
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}
	}))
	defer apiServer.Close()
	gateway, err := workspacegateway.New(workspacegateway.Config{ResourcePlaneID: "rp_cli",
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"forward-test": publicKey}, Audience: workspacesession.AudienceGateway, Lookup: activeWorkspaceSession{}},
		Dialer:   forwardTestDialer{}, PortAuthorizer: allowForwardTestPort{}, MaxConnections: 4, MaxConnectionsPerWorkspace: 4})
	if err != nil {
		t.Fatal(err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()
	gatewayTLS := gatewayServer.Client().Transport.(*http.Transport).TLSClientConfig
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var stderr strings.Builder
	serveDone := make(chan error, 1)
	go func() {
		serveDone <- serveWorkspaceForward(ctx, listener, "tenant_cli", "ws_cli", 5432, apiServer.URL, gatewayServer.URL,
			"forward-access", &stderr, apiServer.Client(), &tls.Config{RootCAs: gatewayTLS.RootCAs})
	}()
	localConn, err := net.Dial("tcp", listener.Addr().String())
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if _, err := localConn.Write([]byte("db protocol")); err != nil {
		_ = localConn.Close()
		cancel()
		t.Fatal(err)
	}
	_ = localConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	response := make([]byte, len("db protocol"))
	if _, err := io.ReadFull(localConn, response); err != nil {
		_ = localConn.Close()
		cancel()
		t.Fatal(err)
	}
	if string(response) != "db protocol" {
		t.Fatalf("forwarded response=%q", response)
	}
	_ = localConn.Close()
	select {
	case <-revokedSignal:
	case <-time.After(3 * time.Second):
		t.Fatal("port-forward session was not revoked")
	}
	if created.Load() != 1 || revoked.Load() != 1 {
		t.Fatalf("session lifecycle create=%d revoke=%d", created.Load(), revoked.Load())
	}
	cancel()
	select {
	case err := <-serveDone:
		if err != nil {
			t.Fatalf("serveWorkspaceForward: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("forward listener did not stop after cancellation")
	}
}

func TestWorkspaceForwardRejectsInvalidPortsBeforeOpeningListener(t *testing.T) {
	for _, test := range []struct {
		name       string
		remotePort string
		localPort  string
	}{
		{name: "remote zero", remotePort: "0"},
		{name: "remote overflow", remotePort: "65536"},
		{name: "remote nonnumeric", remotePort: "postgres"},
		{name: "local overflow", remotePort: "5432", localPort: "65536"},
		{name: "local nonnumeric", remotePort: "5432", localPort: "local"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := workspaceForward(context.Background(), "tenant_cli", "ws_cli", test.remotePort, test.localPort, io.Discard); err == nil {
				t.Fatal("invalid port accepted")
			}
		})
	}
}
