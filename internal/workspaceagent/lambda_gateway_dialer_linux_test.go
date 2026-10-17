//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"io"
	"net/url"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type gatewayTokenProviderStub struct {
	vmID  string
	token string
	err   error
	calls int
}

func (p *gatewayTokenProviderStub) Create(_ context.Context, vmID string) (string, error) {
	p.calls++
	p.vmID = vmID
	return p.token, p.err
}

type gatewayWebSocketStub struct {
	endpoint, workspace, origin, awsToken, ticket string
	permission                                    workspacesession.Permission
	stream                                        io.ReadWriteCloser
	err                                           error
	calls                                         int
}

func (d *gatewayWebSocketStub) Dial(_ context.Context, endpoint, workspace, origin, token, ticket string, permission workspacesession.Permission) (io.ReadWriteCloser, error) {
	d.endpoint, d.workspace, d.origin, d.awsToken, d.ticket, d.permission = endpoint, workspace, origin, token, ticket, permission
	d.calls++
	return d.stream, d.err
}

func TestLambdaMicroVMGatewayDialerUsesDistinctVSCodeCapabilityAndRoute(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	endpoint := WorkspaceRuntimeEndpoint{TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		MicroVMID: "vm-1", Region: "us-east-1", Endpoint: "https://vm-1.lambda-microvm.us-east-1.on.aws"}
	stream := &gatewayTestStream{}
	websocket := &gatewayWebSocketStub{stream: stream}
	issuer := &AgentTicketIssuer{KeyID: "rp-key", PrivateKey: private, TTL: time.Minute}
	tokens := &gatewayTokenProviderStub{token: "short-token"}
	dialer, err := NewLambdaMicroVMGatewayDialer(LambdaMicroVMGatewayDialerConfig{Endpoint: endpoint, Token: tokens,
		Ticket: issuer, Origin: "https://gateway.hako.test", WebSocket: websocket})
	if err != nil {
		t.Fatal(err)
	}
	claims := workspacesession.Claims{UserID: "user-1", TenantID: endpoint.TenantID, WorkspaceID: endpoint.WorkspaceID,
		ResourcePlane: endpoint.ResourcePlane, Permissions: []workspacesession.Permission{workspacesession.PermissionVSCode}}
	opened, err := dialer.OpenVSCode(context.Background(), claims)
	if err != nil || opened != stream || websocket.permission != workspacesession.PermissionVSCode {
		t.Fatalf("OpenVSCode() stream=%v permission=%q err=%v", opened, websocket.permission, err)
	}
	if _, err := VerifyAgentTicket(websocket.ticket, map[string]ed25519.PublicKey{"rp-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID,
		ResourcePlane: claims.ResourcePlane, MicroVMID: endpoint.MicroVMID, Permission: workspacesession.PermissionVSCode}, time.Now().UTC()); err != nil {
		t.Fatalf("verify VS Code Agent ticket: %v", err)
	}
	config, err := newWorkspaceAgentWebSocketConfigForPermission(endpoint.Endpoint, string(endpoint.WorkspaceID),
		"https://gateway.hako.test", tokens.token, websocket.ticket, workspacesession.PermissionVSCode)
	if err != nil || config.Location.Path != "/v1/workspaces/workspace-1/vscode" {
		t.Fatalf("VS Code guest route config=%v err=%v", config, err)
	}
	if _, err := newWorkspaceAgentWebSocketConfigForPermission(endpoint.Endpoint, string(endpoint.WorkspaceID),
		"https://gateway.hako.test", tokens.token, websocket.ticket, workspacesession.PermissionPortForward); err == nil {
		t.Fatal("accepted unsupported permission as guest route")
	}
}

type gatewayTestStream struct{ bytes.Buffer }

func (s *gatewayTestStream) Close() error { return nil }

type lambdaTokenAPIStub struct {
	input  *lambdamicrovms.CreateMicrovmAuthTokenInput
	output *lambdamicrovms.CreateMicrovmAuthTokenOutput
	err    error
}

func (a *lambdaTokenAPIStub) CreateMicrovmAuthToken(_ context.Context, input *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	a.input = input
	return a.output, a.err
}

func TestLambdaMicroVMGatewayDialerMintsVMAndPortScopedCredentials(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &AgentTicketIssuer{KeyID: "rp-agent-key", PrivateKey: private, TTL: time.Minute}
	tokens := &gatewayTokenProviderStub{token: "ephemeral-aws-token"}
	stream := &gatewayTestStream{}
	ws := &gatewayWebSocketStub{stream: stream}
	endpoint := WorkspaceRuntimeEndpoint{TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-tokyo-1",
		MicroVMID: "microvm-1", Region: "ap-northeast-1", Endpoint: "https://microvm-1.lambda-microvm.ap-northeast-1.on.aws"}
	dialer, err := NewLambdaMicroVMGatewayDialer(LambdaMicroVMGatewayDialerConfig{Endpoint: endpoint, Token: tokens, Ticket: issuer,
		Origin: "https://gateway.hako.test", WebSocket: ws})
	if err != nil {
		t.Fatal(err)
	}
	claims := workspacesession.Claims{UserID: "user-1", TenantID: endpoint.TenantID, WorkspaceID: endpoint.WorkspaceID,
		ResourcePlane: endpoint.ResourcePlane, Permissions: []workspacesession.Permission{workspacesession.PermissionShell}}
	opened, err := dialer.OpenShell(context.Background(), claims)
	if err != nil || opened != stream {
		t.Fatalf("OpenShell() stream=%v err=%v", opened, err)
	}
	if tokens.calls != 1 || tokens.vmID != endpoint.MicroVMID || ws.calls != 1 || ws.endpoint != endpoint.Endpoint ||
		ws.workspace != string(endpoint.WorkspaceID) || ws.origin != "https://gateway.hako.test" || ws.awsToken != tokens.token {
		t.Fatalf("token/websocket binding mismatch: token=%+v websocket=%+v", tokens, ws)
	}
	verified, err := VerifyAgentTicket(ws.ticket, map[string]ed25519.PublicKey{"rp-agent-key": public}, AgentTicket{
		UserID: claims.UserID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane,
		MicroVMID: endpoint.MicroVMID, Permission: workspacesession.PermissionShell}, time.Now().UTC())
	if err != nil || verified.Permission != workspacesession.PermissionShell {
		t.Fatalf("guest ticket claims=%+v err=%v", verified, err)
	}
}

func TestLambdaMicroVMGatewayDialerRejectsForeignClaimsAndBadEndpointBeforeIssuing(t *testing.T) {
	_, private, _ := ed25519.GenerateKey(rand.Reader)
	tokens := &gatewayTokenProviderStub{token: "token"}
	issuer := &AgentTicketIssuer{KeyID: "k1", PrivateKey: private, TTL: time.Minute}
	ws := &gatewayWebSocketStub{stream: &gatewayTestStream{}}
	base := WorkspaceRuntimeEndpoint{TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1", MicroVMID: "vm-1",
		Region: "us-east-1", Endpoint: "https://vm-1.lambda-microvm.us-east-1.on.aws"}
	for _, endpoint := range []WorkspaceRuntimeEndpoint{
		{TenantID: base.TenantID, WorkspaceID: base.WorkspaceID, ResourcePlane: base.ResourcePlane, MicroVMID: base.MicroVMID, Region: base.Region, Endpoint: "https://attacker.example"},
		{TenantID: base.TenantID, WorkspaceID: base.WorkspaceID, ResourcePlane: base.ResourcePlane, MicroVMID: base.MicroVMID, Region: base.Region, Endpoint: "http://vm-1.lambda-microvm.us-east-1.on.aws"},
	} {
		if _, err := NewLambdaMicroVMGatewayDialer(LambdaMicroVMGatewayDialerConfig{Endpoint: endpoint, Token: tokens, Ticket: issuer,
			Origin: "https://gateway.hako.test", WebSocket: ws}); err == nil {
			t.Fatalf("accepted untrusted endpoint %+v", endpoint)
		}
	}
	dialer, err := NewLambdaMicroVMGatewayDialer(LambdaMicroVMGatewayDialerConfig{Endpoint: base, Token: tokens, Ticket: issuer,
		Origin: "https://gateway.hako.test", WebSocket: ws})
	if err != nil {
		t.Fatal(err)
	}
	for _, claims := range []workspacesession.Claims{
		{UserID: "user-1", TenantID: "tenant-2", WorkspaceID: base.WorkspaceID, ResourcePlane: base.ResourcePlane, Permissions: []workspacesession.Permission{workspacesession.PermissionShell}},
		{UserID: "user-1", TenantID: base.TenantID, WorkspaceID: "workspace-2", ResourcePlane: base.ResourcePlane, Permissions: []workspacesession.Permission{workspacesession.PermissionShell}},
		{UserID: "user-1", TenantID: base.TenantID, WorkspaceID: base.WorkspaceID, ResourcePlane: "rp-2", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}},
		{UserID: "user-1", TenantID: base.TenantID, WorkspaceID: base.WorkspaceID, ResourcePlane: base.ResourcePlane, Permissions: []workspacesession.Permission{workspacesession.PermissionPortForward}},
	} {
		if _, err := dialer.OpenShell(context.Background(), claims); !errors.Is(err, ErrInteractiveChannelUnavailable) {
			t.Fatalf("accepted foreign or unauthorized claims %+v, err=%v", claims, err)
		}
	}
	if tokens.calls != 0 || ws.calls != 0 {
		t.Fatalf("unauthorized claims reached AWS or network: token calls=%d dial calls=%d", tokens.calls, ws.calls)
	}
}

func TestLambdaMicroVMAuthTokenProviderScopesPortAndLifetime(t *testing.T) {
	api := &lambdaTokenAPIStub{output: &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "short-token"}}}
	token, err := (LambdaMicroVMAuthTokenProvider{API: api}).Create(context.Background(), "microvm-1")
	if err != nil || token != "short-token" || aws.ToString(api.input.MicrovmIdentifier) != "microvm-1" ||
		aws.ToInt32(api.input.ExpirationInMinutes) != WorkspaceAgentAuthTokenMinutes || len(api.input.AllowedPorts) != 1 {
		t.Fatalf("token=%q input=%+v err=%v", token, api.input, err)
	}
	port, ok := api.input.AllowedPorts[0].(*types.PortSpecificationMemberPort)
	if !ok || port.Value != WorkspaceAgentPort {
		t.Fatalf("allowed port = %#v", api.input.AllowedPorts)
	}
	for _, invalid := range []*lambdamicrovms.CreateMicrovmAuthTokenOutput{
		nil,
		{AuthToken: map[string]string{}},
		{AuthToken: map[string]string{"X-aws-proxy-auth": "bad\r\nheader"}},
	} {
		api.output = invalid
		if token, err := (LambdaMicroVMAuthTokenProvider{API: api}).Create(context.Background(), "microvm-1"); err == nil || token != "" {
			t.Errorf("accepted invalid AWS token response: token=%q err=%v", token, err)
		}
	}
	api.err = errors.New("aws api error")
	if _, err := (LambdaMicroVMAuthTokenProvider{API: api}).Create(context.Background(), "microvm-1"); err == nil {
		t.Fatal("accepted AWS token API failure")
	}
	if _, err := (LambdaMicroVMAuthTokenProvider{API: api}).Create(context.Background(), "../other"); err == nil {
		t.Fatal("accepted invalid MicroVM identity")
	}
}

func TestWorkspaceAgentWebSocketConfigUsesScopedHeadersAndFixedRoute(t *testing.T) {
	config, err := newWorkspaceAgentWebSocketConfig("https://microvm-1.lambda-microvm.us-east-1.on.aws", "workspace-1",
		"https://gateway.hako.test", "aws-short-token", "signed-agent-ticket")
	if err != nil {
		t.Fatal(err)
	}
	if config.Location.Scheme != "wss" || config.Location.Host != "microvm-1.lambda-microvm.us-east-1.on.aws" ||
		config.Location.Path != "/v1/workspaces/workspace-1/shell" || config.Origin.String() != "https://gateway.hako.test" ||
		config.Header.Get("Authorization") != "Bearer signed-agent-ticket" || config.Header.Get("X-aws-proxy-auth") != "" ||
		config.Header.Get("X-aws-proxy-port") != "" || len(config.Protocol) != 4 || config.Protocol[0] != "lambda-microvms" ||
		config.Protocol[1] != "lambda-microvms.authentication.aws-short-token" || config.Protocol[2] != "lambda-microvms.port.8080" ||
		config.Protocol[3] != workspaceAgentWebSocketSubprotocol ||
		config.TlsConfig == nil || config.TlsConfig.MinVersion != tls.VersionTLS12 {
		t.Fatalf("unsafe or incomplete upstream WebSocket config: location=%s origin=%s headers=%v protocols=%v", config.Location, config.Origin, config.Header, config.Protocol)
	}
	for _, invalid := range []struct{ endpoint, workspace, origin, token, ticket string }{
		{"https://microvm-1.lambda-microvm.us-east-1.on.aws?redirect=1", "workspace-1", "https://gateway.hako.test", "token", "ticket"},
		{"https://user@microvm-1.lambda-microvm.us-east-1.on.aws", "workspace-1", "https://gateway.hako.test", "token", "ticket"},
		{"http://microvm-1.lambda-microvm.us-east-1.on.aws", "workspace-1", "https://gateway.hako.test", "token", "ticket"},
		{"https://microvm-1.lambda-microvm.us-east-1.on.aws", "../other", "https://gateway.hako.test", "token", "ticket"},
		{"https://microvm-1.lambda-microvm.us-east-1.on.aws", "workspace-1", "https://gateway.hako.test", "bad\r\nheader", "ticket"},
		{"https://microvm-1.lambda-microvm.us-east-1.on.aws", "workspace-1", "https://gateway.hako.test", "bad,protocol", "ticket"},
	} {
		if config, err := newWorkspaceAgentWebSocketConfig(invalid.endpoint, invalid.workspace, invalid.origin, invalid.token, invalid.ticket); err == nil || config != nil {
			t.Errorf("accepted invalid websocket config %+v: config=%v err=%v", invalid, config, err)
		}
	}
	if parsed, _ := url.Parse(config.Location.String()); parsed.RawQuery != "" || parsed.User != nil {
		t.Fatalf("upstream URL retained untrusted components: %s", parsed)
	}
}
