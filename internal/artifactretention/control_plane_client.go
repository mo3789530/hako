package artifactretention

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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const maxCleanupResponseBytes = 256 << 10

var cleanupIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)

type ControlPlaneClientConfig struct {
	Endpoint        string
	ResourcePlaneID domain.ResourcePlaneID
	HMACKey         []byte
	Transport       http.RoundTripper
	Now             func() time.Time
	Random          io.Reader
}

type ControlPlaneClient struct {
	endpoint string
	planeID  domain.ResourcePlaneID
	key      []byte
	client   *http.Client
	now      func() time.Time
	random   io.Reader
}

func NewControlPlaneClient(config ControlPlaneClientConfig) (*ControlPlaneClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		!cleanupIdentityPattern.MatchString(string(config.ResourcePlaneID)) || len(config.HMACKey) < 32 {
		return nil, errors.New("invalid Control Plane artifact cleanup client configuration")
	}
	transport := config.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
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
	return &ControlPlaneClient{endpoint: strings.TrimRight(endpoint.String(), "/"), planeID: config.ResourcePlaneID,
		key: append([]byte(nil), config.HMACKey...), client: &http.Client{Transport: transport, Timeout: 5 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: now, random: random}, nil
}

func (c *ControlPlaneClient) ListExpired(ctx context.Context, now time.Time, limit int) ([]domain.WorkloadArtifact, error) {
	if c == nil || ctx == nil || now.IsZero() || limit < 1 || limit > MaxBatchSize {
		return nil, errors.New("invalid artifact cleanup listing request")
	}
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(c.planeID)) + "/workload-artifacts/expired"
	query := url.Values{"limit": []string{strconv.Itoa(limit)}}
	u, err := url.Parse(c.endpoint + path + "?" + query.Encode())
	if err != nil {
		return nil, errors.New("invalid Control Plane artifact cleanup endpoint")
	}
	response, err := c.do(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, errors.New("Control Plane artifact cleanup listing failed")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxCleanupResponseBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxCleanupResponseBytes {
		return nil, errors.New("invalid Control Plane artifact cleanup response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var result struct {
		Artifacts []domain.WorkloadArtifact `json:"artifacts"`
	}
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF || len(result.Artifacts) > limit {
		return nil, errors.New("invalid Control Plane artifact cleanup response")
	}
	seen := make(map[string]struct{}, len(result.Artifacts))
	for _, artifact := range result.Artifacts {
		if !cleanupIdentityPattern.MatchString(artifact.ID) || artifact.TenantID == "" || artifact.RunID == "" || artifact.StorageRef == "" ||
			artifact.ExpiresAt == nil || artifact.ExpiresAt.After(now) {
			return nil, errors.New("invalid Control Plane artifact cleanup candidate")
		}
		if _, exists := seen[artifact.ID]; exists {
			return nil, errors.New("duplicate Control Plane artifact cleanup candidate")
		}
		seen[artifact.ID] = struct{}{}
	}
	return result.Artifacts, nil
}

func (c *ControlPlaneClient) DeleteExpired(ctx context.Context, artifact domain.WorkloadArtifact, now time.Time) (bool, error) {
	if c == nil || ctx == nil || !cleanupIdentityPattern.MatchString(artifact.ID) || artifact.TenantID == "" || artifact.RunID == "" ||
		artifact.StorageRef == "" || artifact.ExpiresAt == nil || artifact.ExpiresAt.After(now) || now.IsZero() {
		return false, errors.New("invalid artifact cleanup acknowledgement")
	}
	body, err := json.Marshal(struct {
		TenantID   domain.TenantID      `json:"tenant_id"`
		RunID      domain.WorkloadRunID `json:"workload_run_id"`
		StorageRef string               `json:"storage_ref"`
		ExpiresAt  time.Time            `json:"expires_at"`
	}{artifact.TenantID, artifact.RunID, artifact.StorageRef, artifact.ExpiresAt.UTC()})
	if err != nil {
		return false, errors.New("encode artifact cleanup acknowledgement")
	}
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(c.planeID)) + "/workload-artifacts/" + url.PathEscape(string(artifact.ID)) + "/deleted"
	u, err := url.Parse(c.endpoint + path)
	if err != nil {
		return false, errors.New("invalid Control Plane artifact cleanup endpoint")
	}
	response, err := c.do(ctx, http.MethodPost, u, body)
	if err != nil {
		return false, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false, errors.New("Control Plane artifact cleanup acknowledgement failed")
	}
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, 1024))
	if err != nil || len(responseBody) == 0 || len(responseBody) >= 1024 {
		return false, errors.New("invalid Control Plane artifact cleanup acknowledgement response")
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	var result struct {
		Removed bool `json:"removed"`
	}
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF {
		return false, errors.New("invalid Control Plane artifact cleanup acknowledgement response")
	}
	return result.Removed, nil
}

func (c *ControlPlaneClient) do(ctx context.Context, method string, u *url.URL, body []byte) (*http.Response, error) {
	var nonce [16]byte
	if _, err := io.ReadFull(c.random, nonce[:]); err != nil {
		return nil, errors.New("create Control Plane artifact cleanup request ID")
	}
	requestID := "req_" + hex.EncodeToString(nonce[:])
	timestamp := strconv.FormatInt(c.now().UTC().Unix(), 10)
	request, err := http.NewRequestWithContext(ctx, method, u.String(), bytes.NewReader(body))
	if err != nil {
		return nil, errors.New("create Control Plane artifact cleanup request")
	}
	request.Header.Set("X-Hako-Timestamp", timestamp)
	request.Header.Set("X-Hako-Request-ID", requestID)
	request.Header.Set("X-Hako-Signature", cleanupSignature(c.key, method, signedURLPath(u), timestamp, requestID, body))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("Control Plane artifact cleanup request failed: %w", err)
	}
	return response, nil
}

func signedURLPath(u *url.URL) string {
	path := u.EscapedPath()
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

func cleanupSignature(key []byte, method, path, timestamp, requestID string, body []byte) string {
	mac := hmac.New(sha256.New, key)
	bodyHash := sha256.Sum256(body)
	_, _ = fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%s", method, path, timestamp, requestID, hex.EncodeToString(bodyHash[:]))
	return hex.EncodeToString(mac.Sum(nil))
}
