// Package workspacegateway implements the authenticated HTTPS/WSS edge for
// interactive Workspace streams. Control Plane is consulted only during
// connection authorization; terminal/application bytes flow directly through
// this Gateway to a Resource Plane runtime dialer.
package workspacegateway

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

const (
	defaultMaxPayloadBytes = 64 << 10
	maxAllowedPayloadBytes = 1 << 20
)

var (
	ErrConfig        = errors.New("invalid Workspace Gateway configuration")
	ErrConnectionCap = errors.New("Workspace Gateway connection limit reached")
)

type StreamDialer interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, uint16) (io.ReadWriteCloser, error)
}

// PortAuthorizer resolves allowed local Workspace ports from trusted state.
// It must never accept a host or destination from the client.
type PortAuthorizer interface {
	AllowPort(context.Context, workspacesession.Claims, uint16) (bool, error)
}

type Config struct {
	ResourcePlaneID            domain.ResourcePlaneID
	Verifier                   *workspacesession.Verifier
	Dialer                     StreamDialer
	PortAuthorizer             PortAuthorizer
	AllowedOrigins             []string
	MaxConnections             int
	MaxConnectionsPerWorkspace int
	MaxPayloadBytes            int
}

type Gateway struct {
	config   Config
	origins  map[string]struct{}
	mu       sync.Mutex
	active   int
	byWS     map[domain.WorkspaceID]int
	byTicket map[string]struct{}
}

func New(config Config) (*Gateway, error) {
	if config.ResourcePlaneID == "" || config.Verifier == nil || config.Dialer == nil ||
		config.MaxConnections < 1 || config.MaxConnections > 10000 ||
		config.MaxConnectionsPerWorkspace < 1 || config.MaxConnectionsPerWorkspace > config.MaxConnections {
		return nil, ErrConfig
	}
	if config.MaxPayloadBytes == 0 {
		config.MaxPayloadBytes = defaultMaxPayloadBytes
	}
	if config.MaxPayloadBytes < 1024 || config.MaxPayloadBytes > maxAllowedPayloadBytes {
		return nil, ErrConfig
	}
	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, raw := range config.AllowedOrigins {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, ErrConfig
		}
		origins[parsed.Scheme+"://"+strings.ToLower(parsed.Host)] = struct{}{}
	}
	return &Gateway{config: config, origins: origins, byWS: make(map[domain.WorkspaceID]int), byTicket: make(map[string]struct{})}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/shell", g.serveShell)
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/ssh", g.serveSSH)
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/vscode", g.serveVSCode)
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/ports/{port}", g.servePort)
	return noStore(mux)
}

func noStore(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func (g *Gateway) serveShell(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, workspacesession.PermissionShell, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		return g.config.Dialer.OpenShell(ctx, claims)
	})
}

func (g *Gateway) serveVSCode(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, workspacesession.PermissionVSCode, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		return g.config.Dialer.OpenVSCode(ctx, claims)
	})
}

func (g *Gateway) serveSSH(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, workspacesession.PermissionSSHProxy, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		// SSH remains private to the Workspace. The route fixes the destination
		// to loopback port 22; the caller cannot supply a host or port.
		return g.config.Dialer.OpenPort(ctx, claims, 22)
	})
}

func (g *Gateway) servePort(w http.ResponseWriter, r *http.Request) {
	portValue, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || portValue == 0 {
		http.Error(w, "invalid Workspace route", http.StatusBadRequest)
		return
	}
	check := func(ctx context.Context, claims workspacesession.Claims) error {
		if g.config.PortAuthorizer == nil {
			return errors.New("Workspace port forwarding is disabled")
		}
		allowed, err := g.config.PortAuthorizer.AllowPort(ctx, claims, uint16(portValue))
		if err != nil {
			return errors.New("Workspace port authorization is unavailable")
		}
		if !allowed {
			return errors.New("Workspace port is not available")
		}
		return nil
	}
	g.serveChecked(w, r, workspacesession.PermissionPortForward, check, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		return g.config.Dialer.OpenPort(ctx, claims, uint16(portValue))
	})
}

type streamOpener func(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
type streamCheck func(context.Context, workspacesession.Claims) error

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, permission workspacesession.Permission, open streamOpener) {
	g.serveChecked(w, r, permission, nil, open)
}

func (g *Gateway) serveChecked(w http.ResponseWriter, r *http.Request, permission workspacesession.Permission, check streamCheck, open streamOpener) {
	if r.TLS == nil {
		http.Error(w, "HTTPS is required", http.StatusUpgradeRequired)
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		http.Error(w, "invalid Workspace route", http.StatusBadRequest)
		return
	}
	workspaceID := domain.WorkspaceID(r.PathValue("workspace_id"))
	if !validWorkspaceID(workspaceID) {
		http.Error(w, "invalid Workspace route", http.StatusBadRequest)
		return
	}
	token := bearer(r.Header.Get("Authorization"))
	claims, err := g.config.Verifier.Authorize(r.Context(), token, permission)
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, workspacesession.ErrSessionStateUnavailable) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "Workspace session is not authorized", status)
		return
	}
	if claims.WorkspaceID != workspaceID || claims.ResourcePlane != g.config.ResourcePlaneID {
		http.Error(w, "Workspace session is not authorized", http.StatusNotFound)
		return
	}
	if check != nil {
		if err := check(r.Context(), claims); err != nil {
			http.Error(w, "Workspace route is not authorized", http.StatusForbidden)
			return
		}
	}
	release, err := g.acquire(workspaceID, claims.TicketID)
	if err != nil {
		http.Error(w, "Workspace Gateway is at connection capacity", http.StatusTooManyRequests)
		return
	}
	defer release()
	if !allowedOrigin(r.Header.Get("Origin"), g.origins) {
		http.Error(w, "Workspace session origin is not allowed", http.StatusForbidden)
		return
	}
	if !validUpgradeRequest(r) {
		http.Error(w, "valid WebSocket upgrade is required", http.StatusBadRequest)
		return
	}
	upstream, err := open(r.Context(), claims)
	if err != nil || upstream == nil {
		http.Error(w, "Workspace runtime is unavailable", http.StatusBadGateway)
		return
	}
	defer upstream.Close()
	server := websocket.Server{
		Config: websocket.Config{},
		Handshake: func(config *websocket.Config, request *http.Request) error {
			if request.Method != http.MethodGet || !allowedOrigin(request.Header.Get("Origin"), g.origins) {
				return errors.New("invalid Workspace Gateway handshake")
			}
			protocolOffered := false
			for _, protocol := range strings.Split(request.Header.Get("Sec-WebSocket-Protocol"), ",") {
				if strings.TrimSpace(protocol) == "hako.workspace.v1" {
					protocolOffered = true
					break
				}
			}
			if !protocolOffered {
				return errors.New("unsupported Workspace Gateway protocol")
			}
			config.Protocol = []string{"hako.workspace.v1"}
			return nil
		},
		Handler: func(client *websocket.Conn) {
			client.MaxPayloadBytes = g.config.MaxPayloadBytes
			client.PayloadType = websocket.BinaryFrame
			proxyStreams(r.Context(), client, upstream, g.config.MaxPayloadBytes)
		},
	}
	server.ServeHTTP(w, r)
}

func (g *Gateway) acquire(workspaceID domain.WorkspaceID, ticketID string) (func(), error) {
	g.mu.Lock()
	_, ticketAlreadyActive := g.byTicket[ticketID]
	if g.active >= g.config.MaxConnections || g.byWS[workspaceID] >= g.config.MaxConnectionsPerWorkspace || ticketAlreadyActive {
		g.mu.Unlock()
		return nil, ErrConnectionCap
	}
	g.active++
	g.byWS[workspaceID]++
	g.byTicket[ticketID] = struct{}{}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.active--
			g.byWS[workspaceID]--
			delete(g.byTicket, ticketID)
			if g.byWS[workspaceID] == 0 {
				delete(g.byWS, workspaceID)
			}
			g.mu.Unlock()
		})
	}, nil
}

func allowedOrigin(raw string, allowed map[string]struct{}) bool {
	if raw == "" {
		return true
	} // Native CLI clients do not send Origin.
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return false
	}
	_, ok := allowed[parsed.Scheme+"://"+strings.ToLower(parsed.Host)]
	return ok
}

func validUpgradeRequest(request *http.Request) bool {
	if request == nil || request.Method != http.MethodGet ||
		!strings.EqualFold(strings.TrimSpace(request.Header.Get("Upgrade")), "websocket") ||
		!headerHasToken(request.Header.Get("Connection"), "upgrade") ||
		request.Header.Get("Sec-WebSocket-Version") != "13" {
		return false
	}
	key, err := base64.StdEncoding.DecodeString(request.Header.Get("Sec-WebSocket-Key"))
	if err != nil || len(key) != 16 {
		return false
	}
	for _, protocol := range strings.Split(request.Header.Get("Sec-WebSocket-Protocol"), ",") {
		if strings.TrimSpace(protocol) == "hako.workspace.v1" {
			return true
		}
	}
	return false
}

func headerHasToken(header, token string) bool {
	for _, value := range strings.Split(header, ",") {
		if strings.EqualFold(strings.TrimSpace(value), token) {
			return true
		}
	}
	return false
}

func bearer(value string) string {
	parts := strings.Fields(value)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
		return ""
	}
	return parts[1]
}

func validWorkspaceID(value domain.WorkspaceID) bool {
	if len(value) < 3 || len(value) > 128 {
		return false
	}
	for _, char := range string(value) {
		if !(char >= 'a' && char <= 'z') && !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}

func proxyStreams(ctx context.Context, client *websocket.Conn, upstream io.ReadWriteCloser, maxPayload int) {
	if ctx == nil {
		_ = client.Close()
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	finished := make(chan error, 2)
	go func() { finished <- copyWebSocketToStream(ctx, client, upstream) }()
	go func() { finished <- copyStreamToWebSocket(ctx, upstream, client, maxPayload) }()
	select {
	case <-ctx.Done():
	case <-finished:
	}
	cancel()
	_ = upstream.Close()
	_ = client.Close()
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-finished:
	case <-timer.C:
	}
}

func copyWebSocketToStream(ctx context.Context, client *websocket.Conn, upstream io.Writer) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		var frame []byte
		if err := websocket.Message.Receive(client, &frame); err != nil {
			return err
		}
		for len(frame) > 0 {
			n, err := upstream.Write(frame)
			if err != nil {
				return err
			}
			if n <= 0 || n > len(frame) {
				return io.ErrShortWrite
			}
			frame = frame[n:]
		}
	}
}

func copyStreamToWebSocket(ctx context.Context, upstream io.Reader, client *websocket.Conn, maxPayload int) error {
	buffer := make([]byte, maxPayload)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, err := upstream.Read(buffer)
		if n > 0 {
			if sendErr := websocket.Message.Send(client, buffer[:n]); sendErr != nil {
				return sendErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		if n == 0 {
			return io.ErrNoProgress
		}
	}
}

func (g *Gateway) ActiveConnections() (total int, byWorkspace map[domain.WorkspaceID]int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	byWorkspace = make(map[domain.WorkspaceID]int, len(g.byWS))
	for workspaceID, count := range g.byWS {
		byWorkspace[workspaceID] = count
	}
	return g.active, byWorkspace
}

func (g *Gateway) String() string {
	if g == nil {
		return "Workspace Gateway <nil>"
	}
	return fmt.Sprintf("Workspace Gateway Resource Plane %s", g.config.ResourcePlaneID)
}
