package workloadcontroller

import (
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
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var (
	ErrExecutionMetadata         = errors.New("trusted Workload execution metadata is unavailable")
	metadataIDPattern            = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	metadataCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

const (
	metadataRequestTimeout = 5 * time.Second
	maxMetadataResponse    = 16 << 10
	workloadSpecSchemaV1   = 1
	workloadSpecSchemaV2   = 2
	workloadSpecSchemaV3   = 3
)

type ControlPlaneExecutionMetadata struct {
	SchemaVersion        int                       `json:"schema_version"`
	Kind                 domain.WorkloadKind       `json:"kind,omitempty"`
	TenantID             domain.TenantID           `json:"tenant_id"`
	WorkloadRunID        domain.WorkloadRunID      `json:"workload_run_id"`
	AttemptID            string                    `json:"attempt_id"`
	Generation           int64                     `json:"generation"`
	ResourcePlaneID      domain.ResourcePlaneID    `json:"resource_plane_id"`
	CorrelationID        string                    `json:"correlation_id,omitempty"`
	GitHubInstallationID int64                     `json:"github_installation_id"`
	GitHubRepositoryID   int64                     `json:"github_repository_id"`
	GitHubOwner          string                    `json:"github_owner"`
	GitHubRepository     string                    `json:"github_repository"`
	GitHubIntegrationID  string                    `json:"github_integration_id,omitempty"`
	GitHubConfigRevision int64                     `json:"github_config_revision,omitempty"`
	GitHubCredentialGen  int64                     `json:"github_credential_generation,omitempty"`
	CommitSHA            string                    `json:"commit_sha"`
	Ref                  string                    `json:"ref"`
	TriggerEvent         string                    `json:"trigger_event,omitempty"`
	TriggerBranch        string                    `json:"trigger_branch,omitempty"`
	RuntimeClass         string                    `json:"runtime_class"`
	TrustLevel           domain.WorkloadTrustLevel `json:"trust_level"`
	RequestedAt          time.Time                 `json:"requested_at"`
	Deadline             time.Time                 `json:"deadline"`
}

type ControlPlaneMetadataConfig struct {
	Endpoint        string
	ResourcePlaneID domain.ResourcePlaneID
	HMACKey         []byte
	Transport       http.RoundTripper
	Now             func() time.Time
	Random          io.Reader
}

type ControlPlaneMetadataClient struct {
	endpoint string
	planeID  domain.ResourcePlaneID
	key      []byte
	client   *http.Client
	now      func() time.Time
	random   io.Reader
}

func NewControlPlaneMetadataClient(config ControlPlaneMetadataConfig) (*ControlPlaneMetadataClient, error) {
	endpoint, err := url.Parse(strings.TrimSpace(config.Endpoint))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Path != "" || endpoint.RawPath != "" || endpoint.RawQuery != "" || endpoint.Fragment != "" ||
		config.ResourcePlaneID == "" || len(config.HMACKey) < 32 {
		return nil, ErrExecutionMetadata
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	random := config.Random
	if random == nil {
		random = rand.Reader
	}
	transport := config.Transport
	if transport == nil {
		transport = &http.Transport{Proxy: nil, TLSHandshakeTimeout: 2 * time.Second, ResponseHeaderTimeout: 3 * time.Second,
			IdleConnTimeout: 30 * time.Second, MaxIdleConnsPerHost: 2}
	}
	return &ControlPlaneMetadataClient{endpoint: strings.TrimRight(endpoint.String(), "/"), planeID: config.ResourcePlaneID,
		key: append([]byte(nil), config.HMACKey...), client: &http.Client{Transport: transport, Timeout: metadataRequestTimeout,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, now: now, random: random}, nil
}

func (c *ControlPlaneMetadataClient) Resolve(ctx context.Context, id workloadjournal.Command) (ControlPlaneExecutionMetadata, error) {
	if c == nil || c.client == nil || ctx == nil || id.ResourcePlaneID != string(c.planeID) || id.TenantID == "" ||
		id.WorkloadRunID == "" || !metadataIDPattern.MatchString(id.AttemptID) || id.Generation < 1 || id.IdempotencyToken == "" {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	if _, err := workloadjournal.New(id, c.now().UTC()); err != nil {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	var nonce [16]byte
	if _, err := io.ReadFull(c.random, nonce[:]); err != nil {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	requestID := "req_" + hex.EncodeToString(nonce[:])
	timestamp := strconv.FormatInt(c.now().UTC().Unix(), 10)
	path := fmt.Sprintf("/v1/internal/resource-planes/%s/tenants/%s/workloads/%s/attempts/%s/generations/%d/specification",
		url.PathEscape(string(id.ResourcePlaneID)), url.PathEscape(string(id.TenantID)), url.PathEscape(string(id.WorkloadRunID)),
		url.PathEscape(id.AttemptID), id.Generation)
	u, err := url.Parse(c.endpoint + path)
	if err != nil {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	signature := metadataSignature(c.key, http.MethodGet, u.EscapedPath(), timestamp, requestID)
	req.Header.Set("X-Hako-Timestamp", timestamp)
	req.Header.Set("X-Hako-Request-ID", requestID)
	req.Header.Set("X-Hako-Signature", signature)
	response, err := c.client.Do(req)
	if err != nil {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataResponse+1))
	if err != nil || len(body) == 0 || len(body) > maxMetadataResponse {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	var metadata ControlPlaneExecutionMetadata
	decoder := json.NewDecoder(strings.NewReader(string(body)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&metadata) != nil || decoder.Decode(new(any)) != io.EOF || !metadata.matches(id) || !validExecutionMetadata(metadata, c.now().UTC()) {
		return ControlPlaneExecutionMetadata{}, ErrExecutionMetadata
	}
	return metadata, nil
}

func (m ControlPlaneExecutionMetadata) matches(id workloadjournal.Command) bool {
	return (m.SchemaVersion == workloadSpecSchemaV1 || m.SchemaVersion == workloadSpecSchemaV2 || m.SchemaVersion == workloadSpecSchemaV3) && m.TenantID == domain.TenantID(id.TenantID) && m.WorkloadRunID == domain.WorkloadRunID(id.WorkloadRunID) &&
		m.AttemptID == id.AttemptID && m.Generation == id.Generation && m.ResourcePlaneID == domain.ResourcePlaneID(id.ResourcePlaneID)
}

func validExecutionMetadata(m ControlPlaneExecutionMetadata, now time.Time) bool {
	kind := m.Kind
	if kind == "" && m.SchemaVersion <= workloadSpecSchemaV2 {
		kind = domain.WorkloadJob
	}
	if (m.CorrelationID != "" && !metadataCorrelationIDPattern.MatchString(m.CorrelationID)) ||
		m.GitHubInstallationID <= 0 || m.GitHubRepositoryID <= 0 || m.GitHubOwner == "" || m.GitHubRepository == "" ||
		len(m.GitHubOwner) > 100 || len(m.GitHubRepository) > 100 || strings.ContainsAny(m.GitHubOwner+m.GitHubRepository, "/\\\r\n\x00") ||
		!commitPattern.MatchString(m.CommitSHA) || m.RuntimeClass == "" || len(m.RuntimeClass) > 100 ||
		m.TrustLevel != domain.WorkloadUntrusted && m.TrustLevel != domain.WorkloadTrusted && m.TrustLevel != domain.WorkloadPrivileged ||
		m.RequestedAt.IsZero() || m.RequestedAt.After(now.Add(30*time.Second)) || !m.Deadline.After(m.RequestedAt) ||
		m.Deadline.IsZero() || !m.Deadline.After(now) || m.Deadline.Sub(m.RequestedAt) > 24*time.Hour ||
		strings.ContainsAny(m.Ref, "\r\n\x00") || len(m.Ref) > 1024 {
		return false
	}
	if (m.GitHubIntegrationID == "") != (m.GitHubConfigRevision == 0) || (m.GitHubIntegrationID == "") != (m.GitHubCredentialGen == 0) ||
		(m.GitHubIntegrationID != "" && !metadataIDPattern.MatchString(m.GitHubIntegrationID)) {
		return false
	}
	if m.SchemaVersion == workloadSpecSchemaV1 && m.TriggerEvent != "" ||
		m.SchemaVersion < workloadSpecSchemaV3 && m.Kind != "" ||
		m.SchemaVersion == workloadSpecSchemaV3 && m.Kind != domain.WorkloadJob && m.Kind != domain.WorkloadAgent {
		return false
	}
	if (m.TriggerEvent == "") != (m.TriggerBranch == "") ||
		(kind == domain.WorkloadJob && m.TriggerEvent != "" && m.TriggerEvent != "push" && m.TriggerEvent != "pull_request") ||
		(kind == domain.WorkloadAgent && m.TriggerEvent != "issue_comment") ||
		len(m.TriggerBranch) > 255 || strings.Contains(m.TriggerBranch, "..") ||
		strings.Contains(m.TriggerBranch, "//") || strings.HasSuffix(m.TriggerBranch, "/") || strings.HasSuffix(m.TriggerBranch, ".") || strings.Contains(m.TriggerBranch, "@{") {
		return false
	}
	for _, char := range m.TriggerBranch {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}

func effectiveWorkloadKind(metadata ControlPlaneExecutionMetadata) domain.WorkloadKind {
	if metadata.Kind == "" {
		return domain.WorkloadJob
	}
	return metadata.Kind
}

func metadataSignature(key []byte, method, path, timestamp, requestID string) string {
	emptyHash := sha256.Sum256(nil)
	message := fmt.Sprintf("%s\n%s\n%s\n%s\n%s", method, path, timestamp, requestID, hex.EncodeToString(emptyHash[:]))
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(message))
	return hex.EncodeToString(mac.Sum(nil))
}
