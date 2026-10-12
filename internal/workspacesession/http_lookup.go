package workspacesession

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const (
	activeSessionLookupTimeout  = 3 * time.Second
	activeSessionLookupMaxBody  = 8 << 10
	activeLookupTimestampHeader = "X-Hako-Timestamp"
	activeLookupRequestIDHeader = "X-Hako-Request-ID"
	activeLookupSignatureHeader = "X-Hako-Signature"
)

var activeLookupRequestIDPattern = regexp.MustCompile(`^req_[0-9a-f]{32}$`)

type HTTPActiveSessionLookupConfig struct {
	Endpoint        string
	ResourcePlaneID domain.ResourcePlaneID
	HMACKey         []byte
	Transport       http.RoundTripper
	Now             func() time.Time
	Random          io.Reader
}

// HTTPActiveSessionLookup calls the Control Plane's narrowly scoped internal
// HTTPS endpoint. Each Resource Plane has a distinct HMAC key; the Control
// Plane must still compare every claim with the persisted session row.
type HTTPActiveSessionLookup struct {
	endpoint string
	planeID  domain.ResourcePlaneID
	key      []byte
	client   *http.Client
	now      func() time.Time
	random   io.Reader
}

func NewHTTPActiveSessionLookup(config HTTPActiveSessionLookupConfig) (*HTTPActiveSessionLookup, error) {
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
	return &HTTPActiveSessionLookup{endpoint: strings.TrimRight(endpoint.String(), "/"), planeID: config.ResourcePlaneID,
		key: append([]byte(nil), config.HMACKey...), client: &http.Client{Transport: transport, Timeout: activeSessionLookupTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: now, random: random}, nil
}

func (l *HTTPActiveSessionLookup) IsActive(ctx context.Context, claims Claims) (bool, error) {
	if l == nil || l.client == nil || ctx == nil || claims.ResourcePlane != l.planeID || claims.UserID == "" || claims.TenantID == "" ||
		claims.WorkspaceID == "" || claims.Version != 1 || claims.Audience != AudienceGateway || !validTicketID(claims.TicketID) ||
		claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > int64(TicketMaxTTL/time.Second) ||
		!l.now().Before(time.Unix(claims.ExpiresAt, 0)) || ValidatePermissions(claims.Permissions) != nil {
		return false, ErrSessionStateUnavailable
	}
	body, err := json.Marshal(claims)
	if err != nil || len(body) == 0 || len(body) > activeSessionLookupMaxBody {
		return false, ErrSessionStateUnavailable
	}
	var nonce [16]byte
	if _, err := io.ReadFull(l.random, nonce[:]); err != nil {
		return false, ErrSessionStateUnavailable
	}
	requestID := "req_" + hex.EncodeToString(nonce[:])
	timestamp := strconv.FormatInt(l.now().UTC().Unix(), 10)
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(l.planeID)) + "/workspace-sessions/active"
	u, err := url.Parse(l.endpoint + path)
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
	req.Header.Set(activeLookupSignatureHeader, activeLookupSignature(l.key, http.MethodPost, u.EscapedPath(), timestamp, requestID, body))
	response, err := l.client.Do(req)
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
		Active bool `json:"active"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return false, ErrSessionStateUnavailable
	}
	return result.Active, nil
}

func validTicketID(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 24 && len(value) == 48
}

func activeLookupSignature(key []byte, method, path, timestamp, requestID string, body []byte) string {
	bodyHash := sha256.Sum256(body)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, timestamp, requestID, hex.EncodeToString(bodyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}

func validActiveLookupSignature(key []byte, method, path, timestamp, requestID string, body []byte, signature string) bool {
	if len(key) < 32 || !activeLookupRequestIDPattern.MatchString(requestID) || len(signature) != sha256.Size*2 {
		return false
	}
	supplied, err := hex.DecodeString(signature)
	if err != nil {
		return false
	}
	expected, _ := hex.DecodeString(activeLookupSignature(key, method, path, timestamp, requestID, body))
	return hmac.Equal(supplied, expected)
}

// VerifyActiveLookupSignature validates the canonical request MAC used by the
// Control Plane's internal active-session endpoint.
func VerifyActiveLookupSignature(key []byte, method, path, timestamp, requestID string, body []byte, signature string) bool {
	return validActiveLookupSignature(key, method, path, timestamp, requestID, body, signature)
}

var lookupResourcePlanePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
