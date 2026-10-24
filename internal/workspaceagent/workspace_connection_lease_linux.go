//go:build linux

package workspaceagent

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	workspaceConnectionLeaseTTL     = 90 * time.Second
	workspaceConnectionLeaseRefresh = 30 * time.Second
)

var ErrWorkspaceConnectionLimit error = workspaceConnectionLimitError{}
var ErrWorkspaceSessionConnected error = workspaceSessionConnectedError{}

type workspaceConnectionLimitError struct{}

func (workspaceConnectionLimitError) Error() string              { return "Workspace connection limit reached" }
func (workspaceConnectionLimitError) GatewayHTTPStatusCode() int { return http.StatusTooManyRequests }

type workspaceSessionConnectedError struct{}

func (workspaceSessionConnectedError) Error() string {
	return "Workspace Session already has an active connection"
}
func (workspaceSessionConnectedError) GatewayHTTPStatusCode() int { return http.StatusConflict }

type workspaceLeaseHTTPError struct{ status int }

func (e workspaceLeaseHTTPError) Error() string {
	return fmt.Sprintf("Control Plane Workspace lease returned HTTP %d", e.status)
}
func (e workspaceLeaseHTTPError) GatewayHTTPStatusCode() int { return e.status }

type WorkspaceConnectionLeaseIdentity struct {
	SessionID            string
	TenantID             domain.TenantID
	WorkspaceID          domain.WorkspaceID
	ResourcePlaneID      domain.ResourcePlaneID
	MicroVMID            string
	AllocationGeneration int64
}

type WorkspaceConnectionLeaseHandle interface {
	Refresh(context.Context) error
	Close() error
}

type WorkspaceConnectionLeaseReporter interface {
	Open(context.Context, WorkspaceConnectionLeaseIdentity) (WorkspaceConnectionLeaseHandle, error)
}

type signedWorkspaceConnectionLeaseReporter struct {
	endpoint string
	key      []byte
	client   *http.Client
	now      func() time.Time
	newID    func() ([]byte, error)
}

func NewSignedWorkspaceConnectionLeaseReporter(endpoint string, key []byte, client *http.Client) (WorkspaceConnectionLeaseReporter, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || len(key) < 32 {
		return nil, errors.New("Workspace connection lease endpoint and HMAC key are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	client = noWorkspaceLeaseRedirectClient(client)
	return &signedWorkspaceConnectionLeaseReporter{endpoint: strings.TrimRight(endpoint, "/"), key: append([]byte(nil), key...), client: client,
		now: func() time.Time { return time.Now().UTC() }, newID: func() ([]byte, error) { raw := make([]byte, 24); _, err := rand.Read(raw); return raw, err }}, nil
}

func (r *signedWorkspaceConnectionLeaseReporter) Open(ctx context.Context, identity WorkspaceConnectionLeaseIdentity) (WorkspaceConnectionLeaseHandle, error) {
	if r == nil || ctx == nil || !validWorkspaceSessionID(identity.SessionID) || identity.TenantID == "" || identity.WorkspaceID == "" || identity.ResourcePlaneID == "" ||
		!validWorkspaceHookVMID(identity.MicroVMID) || identity.AllocationGeneration < 1 {
		return nil, ErrInteractiveChannelUnavailable
	}
	rawID, err := r.newID()
	if err != nil || len(rawID) != 24 {
		return nil, ErrInteractiveChannelUnavailable
	}
	lease := &signedWorkspaceConnectionLease{reporter: r, id: hex.EncodeToString(rawID), identity: identity}
	if err := lease.renew(ctx); err != nil {
		return nil, ErrInteractiveChannelUnavailable
	}
	return lease, nil
}

type signedWorkspaceConnectionLease struct {
	reporter *signedWorkspaceConnectionLeaseReporter
	id       string
	identity WorkspaceConnectionLeaseIdentity
	close    sync.Once
}

func (l *signedWorkspaceConnectionLease) renew(ctx context.Context) error {
	if l == nil || l.reporter == nil || ctx == nil {
		return ErrInteractiveChannelUnavailable
	}
	now := l.reporter.now().UTC()
	return l.reporter.send(ctx, http.MethodPut, l.id, l.identity, now.Add(workspaceConnectionLeaseTTL))
}

func (l *signedWorkspaceConnectionLease) Refresh(ctx context.Context) error { return l.renew(ctx) }

func (l *signedWorkspaceConnectionLease) Close() error {
	var releaseErr error
	l.close.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		now := l.reporter.now().UTC()
		releaseErr = l.reporter.send(ctx, http.MethodPost, l.id+"/release", l.identity, now.Add(workspaceConnectionLeaseTTL))
	})
	return releaseErr
}

func (r *signedWorkspaceConnectionLeaseReporter) send(ctx context.Context, method, leasePath string,
	identity WorkspaceConnectionLeaseIdentity, expiresAt time.Time) error {
	body, err := json.Marshal(struct {
		SessionID            string                 `json:"session_id"`
		TenantID             domain.TenantID        `json:"tenant_id"`
		WorkspaceID          domain.WorkspaceID     `json:"workspace_id"`
		ResourcePlaneID      domain.ResourcePlaneID `json:"resource_plane_id"`
		MicroVMID            string                 `json:"microvm_id"`
		AllocationGeneration int64                  `json:"allocation_generation"`
		ExpiresAt            time.Time              `json:"expires_at"`
	}{identity.SessionID, identity.TenantID, identity.WorkspaceID, identity.ResourcePlaneID, identity.MicroVMID, identity.AllocationGeneration, expiresAt.UTC()})
	if err != nil || len(body) > 2<<10 {
		return ErrInteractiveChannelUnavailable
	}
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(identity.ResourcePlaneID)) + "/workspace-connection-leases/" + leasePath
	timestamp := fmt.Sprint(r.now().UTC().Unix())
	bodyHash := sha256.Sum256(body)
	message := strings.Join([]string{method, path, timestamp, strings.TrimSuffix(leasePath, "/release"), hex.EncodeToString(bodyHash[:])}, "\n")
	mac := hmac.New(sha256.New, r.key)
	_, _ = mac.Write([]byte(message))
	request, err := http.NewRequestWithContext(ctx, method, r.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return ErrInteractiveChannelUnavailable
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hako-Timestamp", timestamp)
	request.Header.Set("X-Hako-Report-ID", strings.TrimSuffix(leasePath, "/release"))
	request.Header.Set("X-Hako-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := r.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 1024))
		return workspaceLeaseHTTPError{status: response.StatusCode}
	}
	return nil
}

func validWorkspaceSessionID(value string) bool {
	if len(value) != 48 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

func noWorkspaceLeaseRedirectClient(client *http.Client) *http.Client {
	copy := *client
	copy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &copy
}

// leasedWorkspaceStream renews the metadata-only lease while the Gateway owns
// an upstream stream. A failed heartbeat closes the stream; Close always tries
// to release the lease, while expiry remains the crash-recovery mechanism.
type leasedWorkspaceStream struct {
	stream io.ReadWriteCloser
	lease  WorkspaceConnectionLeaseHandle
	stop   chan struct{}
	done   chan struct{}
	once   sync.Once
	errMu  sync.Mutex
	err    error
}

func newLeasedWorkspaceStream(ctx context.Context, stream io.ReadWriteCloser, lease WorkspaceConnectionLeaseHandle) io.ReadWriteCloser {
	w := &leasedWorkspaceStream{stream: stream, lease: lease, stop: make(chan struct{}), done: make(chan struct{})}
	go func() {
		defer close(w.done)
		ticker := time.NewTicker(workspaceConnectionLeaseRefresh)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-w.stop:
				return
			case <-ticker.C:
				heartbeatCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
				err := lease.Refresh(heartbeatCtx)
				cancel()
				if err != nil {
					w.errMu.Lock()
					w.err = err
					w.errMu.Unlock()
					_ = stream.Close()
					return
				}
			}
		}
	}()
	return w
}

func (w *leasedWorkspaceStream) Read(p []byte) (int, error)  { return w.stream.Read(p) }
func (w *leasedWorkspaceStream) Write(p []byte) (int, error) { return w.stream.Write(p) }

func (w *leasedWorkspaceStream) Close() error {
	var closeErr error
	w.once.Do(func() {
		close(w.stop)
		closeErr = w.stream.Close()
		<-w.done
		if w.lease != nil {
			if err := w.lease.Close(); closeErr == nil {
				closeErr = err
			}
		}
		w.errMu.Lock()
		if closeErr == nil {
			closeErr = w.err
		}
		w.errMu.Unlock()
	})
	return closeErr
}
