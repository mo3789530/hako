package workspacegateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

var ErrClientConfig = errors.New("invalid Hako Gateway client configuration")

// Dial opens one TLS WebSocket stream using a short-lived Hako Session Ticket.
// GatewayURL is an HTTPS/WSS origin only; workspace routing is always encoded
// from the validated Workspace ID, and port destinations are never accepted.
func Dial(ctx context.Context, gatewayURL string, workspaceID domain.WorkspaceID, permission workspacesession.Permission,
	port uint16, ticket string, tlsConfig *tls.Config) (*websocket.Conn, error) {
	if ctx == nil || !validWorkspaceID(workspaceID) || strings.TrimSpace(ticket) == "" || len(ticket) > 8192 ||
		strings.ContainsAny(ticket, " \t\r\n") {
		return nil, ErrClientConfig
	}
	endpoint, err := url.Parse(strings.TrimSpace(gatewayURL))
	if err != nil || endpoint == nil || (endpoint.Scheme != "https" && endpoint.Scheme != "wss") || endpoint.Host == "" ||
		endpoint.User != nil || endpoint.Path != "" && endpoint.Path != "/" || endpoint.RawQuery != "" || endpoint.ForceQuery || endpoint.Fragment != "" {
		return nil, ErrClientConfig
	}
	path := "/v1/workspaces/" + url.PathEscape(string(workspaceID))
	switch permission {
	case workspacesession.PermissionShell:
		if port != 0 {
			return nil, ErrClientConfig
		}
		path += "/shell"
	case workspacesession.PermissionVSCode:
		if port != 0 {
			return nil, ErrClientConfig
		}
		path += "/vscode"
	case workspacesession.PermissionSSHProxy:
		if port != 0 {
			return nil, ErrClientConfig
		}
		path += "/ssh"
	case workspacesession.PermissionPortForward:
		if port == 0 {
			return nil, ErrClientConfig
		}
		path += "/ports/" + strconv.Itoa(int(port))
	default:
		return nil, ErrClientConfig
	}
	endpoint.Scheme = "wss"
	endpoint.Path = path
	endpoint.RawPath = ""

	var tlsOptions *tls.Config
	if tlsConfig == nil {
		tlsOptions = &tls.Config{MinVersion: tls.VersionTLS12}
	} else {
		if tlsConfig.InsecureSkipVerify {
			return nil, ErrClientConfig
		}
		tlsOptions = tlsConfig.Clone()
		if tlsOptions.MinVersion < tls.VersionTLS12 {
			tlsOptions.MinVersion = tls.VersionTLS12
		}
	}
	config := &websocket.Config{
		Location:  endpoint,
		Origin:    &url.URL{}, // Native CLI: omit Origin so browser allowlists do not apply.
		Protocol:  []string{"hako.workspace.v1"},
		Version:   websocket.ProtocolVersionHybi13,
		TlsConfig: tlsOptions,
		Header: http.Header{
			"Authorization": []string{"Bearer " + ticket},
		},
		Dialer: &net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second},
	}
	connection, err := config.DialContext(ctx)
	if err != nil {
		return nil, fmt.Errorf("connect to Hako Gateway: %w", err)
	}
	connection.PayloadType = websocket.BinaryFrame
	connection.MaxPayloadBytes = maxAllowedPayloadBytes
	return connection, nil
}
