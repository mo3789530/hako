//go:build linux

package workspaceagent

import (
	"context"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

const (
	WorkspaceAgentPort                 int32 = 8080
	WorkspaceAgentAuthTokenMinutes     int32 = 2
	workspaceAgentWebSocketSubprotocol       = "hako.workspace.agent.v1"
	workspaceAgentConnectTimeout             = 15 * time.Second
)

type LambdaMicroVMTokenAPI interface {
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
}

// LambdaMicroVMAuthTokenProvider mints a token for exactly one Workspace VM
// and the single root-init HTTPS/WSS port. Token values are never retained.
type LambdaMicroVMAuthTokenProvider struct{ API LambdaMicroVMTokenAPI }

func (p LambdaMicroVMAuthTokenProvider) Create(ctx context.Context, microVMID string) (string, error) {
	if p.API == nil || ctx == nil || !validWorkspaceHookVMID(microVMID) {
		return "", ErrInteractiveChannelUnavailable
	}
	response, err := p.API.CreateMicrovmAuthToken(ctx, &lambdamicrovms.CreateMicrovmAuthTokenInput{
		MicrovmIdentifier:   aws.String(microVMID),
		AllowedPorts:        []types.PortSpecification{&types.PortSpecificationMemberPort{Value: WorkspaceAgentPort}},
		ExpirationInMinutes: aws.Int32(WorkspaceAgentAuthTokenMinutes),
	})
	if err != nil || response == nil {
		return "", ErrInteractiveChannelUnavailable
	}
	token := response.AuthToken["X-aws-proxy-auth"]
	if !validWorkspaceHeaderCredential(token, 8000) {
		return "", ErrInteractiveChannelUnavailable
	}
	return token, nil
}

type WorkspaceAgentTokenProvider interface {
	Create(context.Context, string) (string, error)
}

type WorkspaceAgentTicketIssuer interface {
	Issue(AgentTicket) (string, AgentTicket, error)
}

type WorkspaceWebSocketDialer interface {
	Dial(context.Context, string, string, string, string, string, workspacesession.Permission) (io.ReadWriteCloser, error)
}

type WorkspaceRuntimeEndpoint struct {
	TenantID      domain.TenantID
	WorkspaceID   domain.WorkspaceID
	ResourcePlane domain.ResourcePlaneID
	MicroVMID     string
	Region        string
	Endpoint      string
}

// LambdaMicroVMGatewayDialer is the Resource Plane side of the Workspace
// Gateway connection. The runtime endpoint is immutable registration state;
// the browser cannot supply it. Every stream gets a fresh AWS proxy token and
// one-use Ed25519 guest ticket after the Gateway has authorized its Session.
type LambdaMicroVMGatewayDialer struct {
	endpoint  WorkspaceRuntimeEndpoint
	tokens    WorkspaceAgentTokenProvider
	tickets   WorkspaceAgentTicketIssuer
	origin    string
	websocket WorkspaceWebSocketDialer
}

type LambdaMicroVMGatewayDialerConfig struct {
	Endpoint  WorkspaceRuntimeEndpoint
	Token     WorkspaceAgentTokenProvider
	Ticket    WorkspaceAgentTicketIssuer
	Origin    string
	WebSocket WorkspaceWebSocketDialer
}

func NewLambdaMicroVMGatewayDialer(config LambdaMicroVMGatewayDialerConfig) (*LambdaMicroVMGatewayDialer, error) {
	if !validWorkspaceHookVMID(config.Endpoint.MicroVMID) || !validWorkspaceHookVMID(string(config.Endpoint.TenantID)) ||
		!validWorkspaceHookVMID(string(config.Endpoint.WorkspaceID)) || !validWorkspaceHookVMID(string(config.Endpoint.ResourcePlane)) ||
		!validWorkspaceRegion(config.Endpoint.Region) || !validWorkspaceMicroVMEndpoint(config.Endpoint) ||
		config.Token == nil || config.Ticket == nil || !validGatewayOrigin(config.Origin) {
		return nil, ErrInteractiveChannelUnavailable
	}
	if config.WebSocket == nil {
		config.WebSocket = workspaceAgentWebSocketDialer{}
	}
	return &LambdaMicroVMGatewayDialer{endpoint: config.Endpoint, tokens: config.Token, tickets: config.Ticket,
		origin: config.Origin, websocket: config.WebSocket}, nil
}

func validWorkspaceRegion(value string) bool { return workspaceRegionPattern.MatchString(value) }

func validWorkspaceMicroVMEndpoint(binding WorkspaceRuntimeEndpoint) bool {
	parsed, err := url.Parse(binding.Endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.User != nil || parsed.Port() != "" || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	wantHost := strings.ToLower(binding.MicroVMID + ".lambda-microvm." + binding.Region + ".on.aws")
	return strings.EqualFold(parsed.Host, wantHost)
}

func (d *LambdaMicroVMGatewayDialer) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionShell)
}

func (d *LambdaMicroVMGatewayDialer) OpenVSCode(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	return d.open(ctx, claims, workspacesession.PermissionVSCode)
}

func (d *LambdaMicroVMGatewayDialer) open(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission) (io.ReadWriteCloser, error) {
	if d == nil || ctx == nil || claims.UserID == "" || claims.TenantID != d.endpoint.TenantID ||
		claims.WorkspaceID != d.endpoint.WorkspaceID || claims.ResourcePlane != d.endpoint.ResourcePlane ||
		!hasWorkspacePermission(claims.Permissions, permission) ||
		(permission != workspacesession.PermissionShell && permission != workspacesession.PermissionVSCode) {
		return nil, ErrInteractiveChannelUnavailable
	}
	connectCtx, cancel := context.WithTimeout(ctx, workspaceAgentConnectTimeout)
	defer cancel()
	ticket, _, err := d.tickets.Issue(AgentTicket{UserID: claims.UserID, TenantID: claims.TenantID,
		WorkspaceID: claims.WorkspaceID, ResourcePlane: claims.ResourcePlane, MicroVMID: d.endpoint.MicroVMID,
		Permission: permission})
	if err != nil || !validWorkspaceHeaderCredential(ticket, maxAgentTicketBytes) {
		return nil, ErrInteractiveChannelUnavailable
	}
	token, err := d.tokens.Create(connectCtx, d.endpoint.MicroVMID)
	if err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	stream, err := d.websocket.Dial(connectCtx, d.endpoint.Endpoint, string(d.endpoint.WorkspaceID), d.origin, token, ticket, permission)
	if err != nil || stream == nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	return stream, nil
}

func validWorkspaceHeaderCredential(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes {
		return false
	}
	for _, character := range []byte(value) {
		if character < 0x21 || character > 0x7e {
			return false
		}
	}
	return true
}

func hasWorkspacePermission(permissions []workspacesession.Permission, wanted workspacesession.Permission) bool {
	for _, permission := range permissions {
		if permission == wanted {
			return true
		}
	}
	return false
}

func (d *LambdaMicroVMGatewayDialer) OpenPort(context.Context, workspacesession.Claims, uint16) (io.ReadWriteCloser, error) {
	return nil, ErrInteractiveChannelUnavailable
}

type workspaceAgentWebSocketDialer struct{}

func (workspaceAgentWebSocketDialer) Dial(ctx context.Context, httpsEndpoint, workspaceID, origin, awsToken, agentTicket string, permission workspacesession.Permission) (io.ReadWriteCloser, error) {
	if ctx == nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	config, err := newWorkspaceAgentWebSocketConfigForPermission(httpsEndpoint, workspaceID, origin, awsToken, agentTicket, permission)
	if err != nil {
		return nil, err
	}
	config.Dialer = &net.Dialer{Timeout: 5 * time.Second}
	connection, err := config.DialContext(ctx)
	if err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	connection.PayloadType = websocket.BinaryFrame
	return connection, nil
}

func newWorkspaceAgentWebSocketConfig(httpsEndpoint, workspaceID, origin, awsToken, agentTicket string) (*websocket.Config, error) {
	return newWorkspaceAgentWebSocketConfigForPermission(httpsEndpoint, workspaceID, origin, awsToken, agentTicket, workspacesession.PermissionShell)
}

func newWorkspaceAgentWebSocketConfigForPermission(httpsEndpoint, workspaceID, origin, awsToken, agentTicket string,
	permission workspacesession.Permission) (*websocket.Config, error) {
	if !validWebSocketSubprotocol(awsToken, 8000) || !validWorkspaceHeaderCredential(agentTicket, maxAgentTicketBytes) ||
		!validGatewayOrigin(origin) || !validWorkspaceHookVMID(workspaceID) ||
		(permission != workspacesession.PermissionShell && permission != workspacesession.PermissionVSCode) {
		return nil, ErrInteractiveChannelUnavailable
	}
	endpoint, err := url.Parse(httpsEndpoint)
	if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Port() != "" ||
		endpoint.RawQuery != "" || endpoint.Fragment != "" || (endpoint.Path != "" && endpoint.Path != "/") {
		return nil, ErrInteractiveChannelUnavailable
	}
	endpoint.Scheme = "wss"
	channel := "shell"
	if permission == workspacesession.PermissionVSCode {
		channel = "vscode"
	}
	endpoint.Path = "/v1/workspaces/" + url.PathEscape(workspaceID) + "/" + channel
	config, err := websocket.NewConfig(endpoint.String(), origin)
	if err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	config.Protocol = []string{
		"lambda-microvms",
		"lambda-microvms.authentication." + awsToken,
		"lambda-microvms.port." + strconv.Itoa(int(WorkspaceAgentPort)),
		workspaceAgentWebSocketSubprotocol,
	}
	config.TlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	config.Header = http.Header{}
	config.Header.Set("Authorization", "Bearer "+agentTicket)
	return config, nil
}

func validWebSocketSubprotocol(value string, maxBytes int) bool {
	if len(value) == 0 || len(value) > maxBytes {
		return false
	}
	for _, character := range []byte(value) {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || strings.ContainsRune("!#$%&'*+-.^_`|~", rune(character)) {
			continue
		}
		return false
	}
	return true
}

var _ interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, uint16) (io.ReadWriteCloser, error)
} = (*LambdaMicroVMGatewayDialer)(nil)
