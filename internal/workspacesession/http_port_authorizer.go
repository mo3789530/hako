package workspacesession

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const workspacePortAuthorizationTimeout = 3 * time.Second

// HTTPWorkspacePortAuthorizer asks the Control Plane whether the requested
// port is declared by a healthy Service in the current Workspace runtime.
// Its key is the Resource Plane's existing per-plane session lookup key.
type HTTPWorkspacePortAuthorizer struct {
	endpoint string
	planeID  domain.ResourcePlaneID
	key      []byte
	client   *http.Client
	now      func() time.Time
	random   io.Reader
}

type HTTPWorkspacePortAuthorizerConfig struct {
	Endpoint        string
	ResourcePlaneID domain.ResourcePlaneID
	HMACKey         []byte
	Transport       http.RoundTripper
	Now             func() time.Time
	Random          io.Reader
}

func NewHTTPWorkspacePortAuthorizer(config HTTPWorkspacePortAuthorizerConfig) (*HTTPWorkspacePortAuthorizer, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		!lookupResourcePlanePattern.MatchString(string(config.ResourcePlaneID)) || len(config.HMACKey) < 32 {
		return nil, ErrSessionStateUnavailable
	}
	transport := config.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil, TLSHandshakeTimeout: time.Second, ResponseHeaderTimeout: 2 * time.Second,
			IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	return &HTTPWorkspacePortAuthorizer{endpoint: strings.TrimRight(endpoint.String(), "/"), planeID: config.ResourcePlaneID,
		key: append([]byte(nil), config.HMACKey...), client: &http.Client{Transport: transport, Timeout: workspacePortAuthorizationTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: now, random: random}, nil
}

func (a *HTTPWorkspacePortAuthorizer) AllowPort(ctx context.Context, claims Claims, port uint16) (bool, error) {
	if a == nil || a.client == nil || ctx == nil || claims.ResourcePlane != a.planeID || port == 0 || claims.UserID == "" || claims.TenantID == "" ||
		claims.WorkspaceID == "" || claims.Version != 1 || claims.Audience != AudienceGateway || !validTicketID(claims.TicketID) ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(TicketMaxTTL/time.Second) ||
		!a.now().Before(time.Unix(claims.ExpiresAt, 0)) || ValidatePermissions(claims.Permissions) != nil || !hasPermission(claims.Permissions, PermissionPortForward) {
		return false, ErrSessionStateUnavailable
	}
	body, err := json.Marshal(struct {
		Claims Claims `json:"claims"`
		Port   uint16 `json:"port"`
	}{Claims: claims, Port: port})
	if err != nil || len(body) > 8<<10 {
		return false, ErrSessionStateUnavailable
	}
	var nonce [16]byte
	if _, err := io.ReadFull(a.random, nonce[:]); err != nil {
		return false, ErrSessionStateUnavailable
	}
	requestID := "req_" + hex.EncodeToString(nonce[:])
	timestamp := strconv.FormatInt(a.now().UTC().Unix(), 10)
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(a.planeID)) + "/workspace-ports/authorize"
	u, err := url.Parse(a.endpoint + path)
	if err != nil {
		return false, ErrSessionStateUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(body))
	if err != nil {
		return false, ErrSessionStateUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(activeLookupTimestampHeader, timestamp)
	req.Header.Set(activeLookupRequestIDHeader, requestID)
	req.Header.Set(activeLookupSignatureHeader, activeLookupSignature(a.key, http.MethodPost, u.EscapedPath(), timestamp, requestID, body))
	response, err := a.client.Do(req)
	if err != nil {
		return false, ErrSessionStateUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, ErrSessionStateUnavailable
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return false, ErrSessionStateUnavailable
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 256))
	if err != nil || len(responseBody) == 0 || len(responseBody) >= 256 {
		return false, ErrSessionStateUnavailable
	}
	var result struct {
		Allowed bool `json:"allowed"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return false, ErrSessionStateUnavailable
	}
	return result.Allowed, nil
}

func hasPermission(permissions []Permission, wanted Permission) bool {
	for _, permission := range permissions {
		if permission == wanted {
			return true
		}
	}
	return false
}
