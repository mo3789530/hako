// Package workspacegateway implements the authenticated HTTPS/WSS edge for
// interactive Workspace streams. Control Plane is consulted only during
// connection authorization; terminal/application bytes flow directly through
// this Gateway to a Resource Plane runtime dialer.
package workspacegateway

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

const (
	defaultMaxPayloadBytes        = 64 << 10
	maxAllowedPayloadBytes        = 1 << 20
	defaultSessionRecheckInterval = 15 * time.Second
	maxSessionRecheckInterval     = time.Minute
)

var (
	ErrConfig        = errors.New("invalid Workspace Gateway configuration")
	ErrConnectionCap = errors.New("Workspace Gateway connection limit reached")
)

type StreamDialer interface {
	OpenShell(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenFileTransfer(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
	OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error)
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
	SessionRecheckInterval     time.Duration
	Logger                     *slog.Logger
	// MetricsWriter receives CloudWatch Embedded Metric Format JSON records.
	// It must be an operational log sink (normally stdout), never a client stream.
	MetricsWriter io.Writer
}

type Gateway struct {
	config    Config
	logger    *slog.Logger
	origins   map[string]struct{}
	mu        sync.Mutex
	active    int
	byWS      map[domain.WorkspaceID]int
	byTicket  map[string]struct{}
	metricsMu sync.Mutex
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
	if config.SessionRecheckInterval < 0 || config.SessionRecheckInterval > maxSessionRecheckInterval {
		return nil, ErrConfig
	}
	if config.SessionRecheckInterval == 0 {
		config.SessionRecheckInterval = defaultSessionRecheckInterval
	}
	if config.Logger == nil {
		config.Logger = slog.Default()
	}
	origins := make(map[string]struct{}, len(config.AllowedOrigins))
	for _, raw := range config.AllowedOrigins {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
			return nil, ErrConfig
		}
		origins[parsed.Scheme+"://"+strings.ToLower(parsed.Host)] = struct{}{}
	}
	return &Gateway{config: config, logger: config.Logger, origins: origins, byWS: make(map[domain.WorkspaceID]int), byTicket: make(map[string]struct{})}, nil
}

func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
	})
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/shell", g.serveShell)
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/exec", g.serveExec)
	mux.HandleFunc("GET /v1/workspaces/{workspace_id}/files", g.serveFiles)
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

func (g *Gateway) serveExec(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, workspacesession.PermissionExec, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		return g.config.Dialer.OpenExec(ctx, claims)
	})
}

func (g *Gateway) serveFiles(w http.ResponseWriter, r *http.Request) {
	g.serve(w, r, workspacesession.PermissionFileTransfer, func(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
		return g.config.Dialer.OpenFileTransfer(ctx, claims)
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
		return g.config.Dialer.OpenPort(ctx, claims, workspacesession.PermissionSSHProxy, 22)
	})
}

func (g *Gateway) servePort(w http.ResponseWriter, r *http.Request) {
	portValue, err := strconv.ParseUint(r.PathValue("port"), 10, 16)
	if err != nil || portValue == 0 {
		http.Error(w, "invalid Workspace route", http.StatusBadRequest)
		return
	}
	check := func(ctx context.Context, claims workspacesession.Claims) error {
		if previewprotocol.IsRuntimeWorkspaceID(string(claims.WorkspaceID)) && uint16(portValue) != previewprotocol.ApplicationPort {
			return errors.New("Preview application route is not available")
		}
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
		return g.config.Dialer.OpenPort(ctx, claims, workspacesession.PermissionPortForward, uint16(portValue))
	})
}

type streamOpener func(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error)
type streamCheck func(context.Context, workspacesession.Claims) error

func (g *Gateway) serve(w http.ResponseWriter, r *http.Request, permission workspacesession.Permission, open streamOpener) {
	g.serveChecked(w, r, permission, nil, open)
}

func (g *Gateway) serveChecked(w http.ResponseWriter, r *http.Request, permission workspacesession.Permission, check streamCheck, open streamOpener) {
	startedAt := time.Now()
	outcome := "rejected"
	var claims workspacesession.Claims
	defer func() {
		g.recordConnectionMetric(permission, outcome, time.Since(startedAt), claims)
	}()
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
	verifiedClaims, err := g.config.Verifier.Authorize(r.Context(), token, permission)
	claims = verifiedClaims
	if err != nil {
		status := http.StatusUnauthorized
		if errors.Is(err, workspacesession.ErrSessionStateUnavailable) {
			status = http.StatusServiceUnavailable
		}
		http.Error(w, "Workspace session is not authorized", status)
		return
	}
	if claims.WorkspaceID != workspaceID || claims.ResourcePlane != g.config.ResourcePlaneID {
		g.logFailure(r.Context(), claims, permission, "session_scope_mismatch")
		http.Error(w, "Workspace session is not authorized", http.StatusNotFound)
		return
	}
	if check != nil {
		if err := check(r.Context(), claims); err != nil {
			g.logFailure(r.Context(), claims, permission, "route_authorization")
			http.Error(w, "Workspace route is not authorized", http.StatusForbidden)
			return
		}
	}
	release, err := g.acquire(workspaceID, claims.TicketID)
	if err != nil {
		g.logFailure(r.Context(), claims, permission, "connection_capacity")
		http.Error(w, "Workspace Gateway is at connection capacity", http.StatusTooManyRequests)
		return
	}
	defer release()
	if !allowedOrigin(r.Header.Get("Origin"), g.origins) {
		g.logFailure(r.Context(), claims, permission, "origin_rejected")
		http.Error(w, "Workspace session origin is not allowed", http.StatusForbidden)
		return
	}
	if !validUpgradeRequest(r) {
		g.logFailure(r.Context(), claims, permission, "websocket_upgrade_rejected")
		http.Error(w, "valid WebSocket upgrade is required", http.StatusBadRequest)
		return
	}
	upstream, err := open(r.Context(), claims)
	if err != nil || upstream == nil {
		status := http.StatusBadGateway
		var statusError interface{ GatewayHTTPStatusCode() int }
		if errors.As(err, &statusError) {
			candidate := statusError.GatewayHTTPStatusCode()
			if candidate == http.StatusTooManyRequests || candidate == http.StatusConflict {
				status = candidate
			}
		}
		failureClass := "runtime_dial"
		if status == http.StatusTooManyRequests {
			failureClass = "connection_capacity"
		} else if status == http.StatusConflict {
			failureClass = "session_connection_replay"
		}
		g.logFailure(r.Context(), claims, permission, failureClass)
		if status == http.StatusTooManyRequests {
			http.Error(w, "Workspace connection limit reached", status)
			return
		}
		if status == http.StatusConflict {
			http.Error(w, "Workspace Session already has an active connection", status)
			return
		}
		http.Error(w, "Workspace runtime is unavailable", status)
		return
	}
	defer upstream.Close()
	sessionRecheckFailed := false
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
			proxyAuthorizedStreams(r.Context(), client, upstream, g.config.MaxPayloadBytes,
				g.config.SessionRecheckInterval, func(ctx context.Context) error {
					_, err := g.config.Verifier.Authorize(ctx, token, permission)
					if err != nil {
						sessionRecheckFailed = true
						g.logFailure(ctx, claims, permission, "session_recheck")
					}
					return err
				})
		},
	}
	server.ServeHTTP(w, r)
	if sessionRecheckFailed {
		outcome = "session_recheck_failed"
	} else {
		outcome = "disconnected"
	}
}

type gatewayMetricEvent struct {
	AWS struct {
		Timestamp         int64 `json:"Timestamp"`
		CloudWatchMetrics []struct {
			Namespace  string     `json:"Namespace"`
			Dimensions [][]string `json:"Dimensions"`
			Metrics    []struct {
				Name string `json:"Name"`
				Unit string `json:"Unit"`
			} `json:"Metrics"`
		} `json:"CloudWatchMetrics"`
	} `json:"_aws"`
	ResourcePlaneID      string  `json:"ResourcePlaneId"`
	Channel              string  `json:"Channel"`
	Outcome              string  `json:"Outcome"`
	ConnectionEvents     float64 `json:"ConnectionEvents"`
	RejectedConnections  float64 `json:"RejectedConnections"`
	ActiveConnections    float64 `json:"ActiveConnections"`
	ConnectionDurationMS float64 `json:"ConnectionDurationMs"`
}

func (g *Gateway) recordConnectionMetric(permission workspacesession.Permission, outcome string, duration time.Duration, claims workspacesession.Claims) {
	active, _ := g.ActiveConnections()
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(context.Background(), "Workspace Gateway connection lifecycle",
		"event", "workspace_gateway.connection", "resource_plane_id", g.config.ResourcePlaneID,
		"channel", permission, "outcome", outcome, "duration_ms", duration.Milliseconds(),
		"active_connections", active, "tenant_id", claims.TenantID, "workspace_id", claims.WorkspaceID)
	if g.config.MetricsWriter == nil {
		return
	}
	event := gatewayMetricEvent{
		ResourcePlaneID: string(g.config.ResourcePlaneID), Channel: string(permission), Outcome: outcome,
		ConnectionEvents: 1, ActiveConnections: float64(active), ConnectionDurationMS: float64(duration.Milliseconds()),
	}
	if outcome == "rejected" {
		event.RejectedConnections = 1
	}
	event.AWS.Timestamp = time.Now().UnixMilli()
	metric := struct {
		Namespace  string     `json:"Namespace"`
		Dimensions [][]string `json:"Dimensions"`
		Metrics    []struct {
			Name string `json:"Name"`
			Unit string `json:"Unit"`
		} `json:"Metrics"`
	}{Namespace: "Hako/WorkspaceGateway", Dimensions: [][]string{{"ResourcePlaneId", "Channel", "Outcome"}}}
	for _, definition := range []struct{ name, unit string }{
		{"ConnectionEvents", "Count"}, {"RejectedConnections", "Count"},
		{"ActiveConnections", "Count"}, {"ConnectionDurationMs", "Milliseconds"},
	} {
		metric.Metrics = append(metric.Metrics, struct {
			Name string `json:"Name"`
			Unit string `json:"Unit"`
		}{Name: definition.name, Unit: definition.unit})
	}
	event.AWS.CloudWatchMetrics = []struct {
		Namespace  string     `json:"Namespace"`
		Dimensions [][]string `json:"Dimensions"`
		Metrics    []struct {
			Name string `json:"Name"`
			Unit string `json:"Unit"`
		} `json:"Metrics"`
	}{metric}
	encoded, err := json.Marshal(event)
	if err != nil {
		return
	}
	g.metricsMu.Lock()
	defer g.metricsMu.Unlock()
	if _, err := fmt.Fprintln(g.config.MetricsWriter, string(encoded)); err != nil {
		logger.WarnContext(context.Background(), "Workspace Gateway metric write failed", "failure_class", "metrics_write")
	}
}

func (g *Gateway) logFailure(ctx context.Context, claims workspacesession.Claims, permission workspacesession.Permission, failureClass string) {
	logger := g.logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.ErrorContext(ctx, "Workspace Gateway request failed",
		"failure_class", failureClass,
		"tenant_id", claims.TenantID,
		"workspace_id", claims.WorkspaceID,
		"resource_plane_id", claims.ResourcePlane,
		"permission", permission)
}

// proxyAuthorizedStreams periodically rechecks the persisted Session while a
// WebSocket remains open. The Control Plane carries only this bounded
// authorization check; application bytes continue to flow directly between
// the client and Resource Plane runtime. Any revocation or lookup failure
// cancels both stream directions (fail closed).
func proxyAuthorizedStreams(ctx context.Context, client *websocket.Conn, upstream io.ReadWriteCloser, maxPayload int,
	interval time.Duration, authorize func(context.Context) error) {
	if ctx == nil || authorize == nil || interval <= 0 || interval > maxSessionRecheckInterval {
		_ = upstream.Close()
		_ = client.Close()
		return
	}
	streamCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-streamCtx.Done():
				return
			case <-ticker.C:
				checkCtx, checkCancel := context.WithTimeout(streamCtx, 5*time.Second)
				err := authorize(checkCtx)
				checkCancel()
				if err != nil {
					cancel()
					return
				}
			}
		}
	}()
	proxyStreams(streamCtx, client, upstream, maxPayload)
	cancel()
	<-done
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
