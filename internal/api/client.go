package api

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const maxAPIResponseSize = 64 << 10

type TenantMembership struct {
	TenantID string `json:"tenant_id"`
	UserID   string `json:"user_id"`
	Role     string `json:"role"`
}

type TenantPlacementPolicy struct {
	TenantID             string     `json:"tenant_id"`
	AllowedRegions       []string   `json:"allowed_regions"`
	ResourcePlaneIDs     []string   `json:"resource_plane_ids"`
	RequiredCapabilities []string   `json:"required_capabilities"`
	MaxCostTier          string     `json:"max_cost_tier,omitempty"`
	MinimumIsolationTier string     `json:"minimum_isolation_tier,omitempty"`
	UpdatedAt            *time.Time `json:"updated_at,omitempty"`
}

type TenantWorkspaceIdlePolicy struct {
	TenantID           string     `json:"tenant_id"`
	Enabled            bool       `json:"enabled"`
	GracePeriodSeconds int        `json:"grace_period_seconds"`
	Revision           int64      `json:"revision"`
	UpdatedAt          *time.Time `json:"updated_at,omitempty"`
}

type WorkspaceCreateResult struct {
	ID             string `json:"id"`
	TenantID       string `json:"tenant_id"`
	OwnerID        string `json:"owner_id"`
	Name           string `json:"name"`
	DesiredState   string `json:"desired_state"`
	ObservedState  string `json:"observed_state"`
	OperationID    string `json:"operation_id"`
	OperationState string `json:"operation_state"`
}

type WorkspaceView struct {
	Workspace WorkspaceSummary `json:"workspace"`
	Status    WorkspaceStatus  `json:"status"`
}

type WorkspaceSummary struct {
	ID           string `json:"id"`
	TenantID     string `json:"tenant_id"`
	OwnerID      string `json:"owner_id"`
	Name         string `json:"name"`
	RuntimeClass string `json:"runtime_class"`
	Image        string `json:"image"`
}

type WorkspaceStatus struct {
	DesiredState  string `json:"desired_state"`
	ObservedState string `json:"observed_state"`
}

type WorkspaceListResult struct {
	Items  []WorkspaceView `json:"items"`
	Total  int64           `json:"total"`
	Limit  int             `json:"limit"`
	Offset int             `json:"offset"`
}

type TenantUsage struct {
	WorkspacesUsed                  int64 `json:"workspaces_used"`
	WorkspacesLimit                 int64 `json:"workspaces_limit"`
	WorkloadRunsUsed                int64 `json:"workload_runs_used"`
	WorkloadRunsLimit               int64 `json:"workload_runs_limit"`
	QueuedWorkloadRunsUsed          int64 `json:"queued_workload_runs_used"`
	QueuedWorkloadRunsLimit         int64 `json:"queued_workload_runs_limit"`
	PreviewRunsUsed                 int64 `json:"preview_runs_used"`
	PreviewRunsLimit                int64 `json:"preview_runs_limit"`
	PreviewLifetimeLimit            int64 `json:"preview_lifetime_seconds_limit"`
	ArtifactBytesUsed               int64 `json:"artifact_bytes_used"`
	ArtifactBytesReserved           int64 `json:"artifact_bytes_reserved"`
	ArtifactBytesLimit              int64 `json:"artifact_bytes_limit"`
	WorkspaceComputeCPUMilliUsed    int64 `json:"workspace_compute_milli_cpu_used"`
	WorkspaceComputeMemoryMiBUsed   int64 `json:"workspace_compute_memory_mib_used"`
	WorkspaceComputeProfilesMissing int64 `json:"workspace_compute_profiles_missing"`
	WorkspaceComputeCPUMilliLimit   int64 `json:"workspace_compute_milli_cpu_limit"`
	WorkspaceComputeMemoryMiBLimit  int64 `json:"workspace_compute_memory_mib_limit"`
}

type TenantQuotaLimits struct {
	TenantID                         string `json:"tenant_id"`
	WorkspacesLimit                  int16  `json:"workspaces_limit"`
	WorkloadRunsLimit                int16  `json:"workload_runs_limit"`
	QueuedWorkloadRunsLimit          int16  `json:"queued_workload_runs_limit"`
	PreviewRunsLimit                 int16  `json:"preview_runs_limit"`
	PreviewLifetimeSecondsLimit      int64  `json:"preview_lifetime_seconds_limit"`
	ArtifactBytesLimit               int64  `json:"artifact_bytes_limit"`
	WorkspaceServiceMemoryBytesLimit int64  `json:"service_memory_bytes_limit"`
	WorkspaceServiceMilliCPULimit    int64  `json:"service_milli_cpu_limit"`
	WorkspaceComputeMilliCPULimit    int64  `json:"workspace_compute_milli_cpu_limit"`
	WorkspaceComputeMemoryMiBLimit   int64  `json:"workspace_compute_memory_mib_limit"`
	Revision                         int64  `json:"revision"`
	UpdatedBy                        string `json:"updated_by,omitempty"`
}

type WorkspaceActionResult struct {
	Workspace      WorkspaceView `json:"workspace"`
	OperationID    string        `json:"operation_id"`
	OperationType  string        `json:"operation_type"`
	OperationState string        `json:"operation_state"`
}

type ServiceCatalogResult struct {
	SchemaVersion int                       `json:"schema_version"`
	Items         []servicecatalog.Template `json:"items"`
}

type WorkspaceServiceListResult struct {
	Items []domain.Service `json:"items"`
}

type WorkspaceServiceResourceUsage struct {
	MemoryBytesUsed  int64 `json:"memory_bytes_used"`
	MemoryBytesLimit int64 `json:"memory_bytes_limit"`
	MilliCPUUsed     int64 `json:"milli_cpu_used"`
	MilliCPULimit    int64 `json:"milli_cpu_limit"`
}

type AddWorkspaceServiceRequest struct {
	CatalogName string `json:"catalog_name"`
	Name        string `json:"name,omitempty"`
}

type WorkspaceSession struct {
	SessionID   domain.SessionID              `json:"session_id"`
	Ticket      string                        `json:"ticket"`
	ExpiresAt   time.Time                     `json:"expires_at"`
	Permissions []workspacesession.Permission `json:"permissions"`
}

type RepositoryPreviewSession struct {
	SessionID   domain.SessionID              `json:"session_id"`
	WorkspaceID domain.WorkspaceID            `json:"workspace_id"`
	Ticket      string                        `json:"ticket"`
	ExpiresAt   time.Time                     `json:"expires_at"`
	Permissions []workspacesession.Permission `json:"permissions"`
}

type WorkloadRunListResult struct {
	Items  []domain.WorkloadRun `json:"items"`
	Total  int64                `json:"total"`
	Limit  int                  `json:"limit"`
	Offset int                  `json:"offset"`
}

type WorkloadArtifactDownloadTicket struct {
	Ticket    string    `json:"ticket"`
	ExpiresAt time.Time `json:"expires_at"`
	SizeBytes int64     `json:"size_bytes"`
	SHA256    string    `json:"sha256"`
	Kind      string    `json:"kind"`
}

type WorkloadRunCreateRequest struct {
	GitHubInstallationID int64  `json:"github_installation_id"`
	GitHubRepositoryID   int64  `json:"github_repository_id"`
	CommitSHA            string `json:"commit_sha"`
	Ref                  string `json:"ref,omitempty"`
	TimeoutSeconds       int64  `json:"timeout_seconds,omitempty"`
}

type APIError struct {
	StatusCode int
	Code       string
	Message    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("Hako API error %s (HTTP %d): %s", e.Code, e.StatusCode, e.Message)
	}
	return fmt.Sprintf("Hako API returned HTTP %d", e.StatusCode)
}

// Health calls the protected Hako API health route with a saved access token.
func Health(ctx context.Context, baseURL, accessToken string, client *http.Client) error {
	endpoint, err := apiEndpoint(baseURL, "/v1/health")
	if err != nil {
		return err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Hako API returned HTTP %d", response.StatusCode)
	}
	var result healthResponse
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return errors.New("Hako API returned an invalid health response")
	}
	if result.Status != "ok" {
		return errors.New("Hako API health check did not return status ok")
	}
	return nil
}

// GetTenantMembership requests the authenticated user's membership in a
// Tenant. A 404 intentionally does not distinguish missing Tenants from
// Tenants the caller cannot access.
func GetTenantMembership(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (TenantMembership, error) {
	var membership TenantMembership
	if !validTenantID(tenantID) {
		return membership, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/membership")
	if err != nil {
		return membership, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return membership, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return membership, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&membership); err != nil {
		return TenantMembership{}, errors.New("Hako API returned an invalid Tenant membership response")
	}
	return membership, nil
}

// GetTenantPlacementPolicy retrieves Tenant-level placement constraints.
func GetTenantPlacementPolicy(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (TenantPlacementPolicy, error) {
	var result TenantPlacementPolicy
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/placement-policy")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return TenantPlacementPolicy{}, errors.New("Hako API returned an invalid Tenant placement policy")
	}
	return result, nil
}

// PutTenantPlacementPolicy replaces Tenant-level placement constraints.
func PutTenantPlacementPolicy(ctx context.Context, baseURL, accessToken string, policy TenantPlacementPolicy, client *http.Client) (TenantPlacementPolicy, error) {
	var result TenantPlacementPolicy
	if !validTenantID(policy.TenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(policy.TenantID)+"/placement-policy")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		AllowedRegions       []string `json:"allowed_regions"`
		ResourcePlaneIDs     []string `json:"resource_plane_ids"`
		RequiredCapabilities []string `json:"required_capabilities"`
		MaxCostTier          string   `json:"max_cost_tier,omitempty"`
		MinimumIsolationTier string   `json:"minimum_isolation_tier,omitempty"`
	}{policy.AllowedRegions, policy.ResourcePlaneIDs, policy.RequiredCapabilities, policy.MaxCostTier, policy.MinimumIsolationTier})
	if err != nil {
		return result, fmt.Errorf("encode Tenant placement policy: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return TenantPlacementPolicy{}, errors.New("Hako API returned an invalid Tenant placement policy")
	}
	return result, nil
}

func GetTenantWorkspaceIdlePolicy(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (TenantWorkspaceIdlePolicy, error) {
	var result TenantWorkspaceIdlePolicy
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspace-idle-policy")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.TenantID != tenantID || result.GracePeriodSeconds < 300 || result.GracePeriodSeconds > 86400 || result.Revision < 0 {
		return TenantWorkspaceIdlePolicy{}, errors.New("Hako API returned an invalid Tenant Workspace idle policy")
	}
	return result, nil
}

func PutTenantWorkspaceIdlePolicy(ctx context.Context, baseURL, accessToken string, policy TenantWorkspaceIdlePolicy, client *http.Client) (TenantWorkspaceIdlePolicy, error) {
	var result TenantWorkspaceIdlePolicy
	if !validTenantID(policy.TenantID) || strings.TrimSpace(accessToken) == "" || policy.GracePeriodSeconds < 300 || policy.GracePeriodSeconds > 86400 {
		return result, errors.New("Tenant Workspace idle policy requires a valid Tenant ID and a grace period between 300 and 86400 seconds")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(policy.TenantID)+"/workspace-idle-policy")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Enabled            bool `json:"enabled"`
		GracePeriodSeconds int  `json:"grace_period_seconds"`
	}{policy.Enabled, policy.GracePeriodSeconds})
	if err != nil {
		return result, fmt.Errorf("encode Tenant Workspace idle policy: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		copy := *client
		client = &copy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(new(any)) != io.EOF ||
		result.TenantID != policy.TenantID || result.GracePeriodSeconds < 300 || result.GracePeriodSeconds > 86400 || result.Revision < 1 {
		return TenantWorkspaceIdlePolicy{}, errors.New("Hako API returned an invalid Tenant Workspace idle policy")
	}
	return result, nil
}

// CreateWorkspace requests asynchronous Workspace provisioning. The caller
// must reuse idempotencyKey if retrying the same logical request.
func CreateWorkspace(ctx context.Context, baseURL, accessToken, tenantID, name, idempotencyKey string, client *http.Client) (WorkspaceCreateResult, error) {
	var result WorkspaceCreateResult
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 128 {
		return result, errors.New("workspace name must contain 1 to 128 characters")
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return result, errors.New("a valid idempotency key is required")
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Name string `json:"name"`
	}{Name: name})
	if err != nil {
		return result, fmt.Errorf("encode Workspace request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	var wire struct {
		Workspace struct {
			ID       string `json:"id"`
			TenantID string `json:"tenant_id"`
			OwnerID  string `json:"owner_id"`
			Name     string `json:"name"`
		} `json:"workspace"`
		Status struct {
			DesiredState  string `json:"desired_state"`
			ObservedState string `json:"observed_state"`
		} `json:"status"`
		OperationID    string `json:"operation_id"`
		OperationState string `json:"operation_state"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&wire); err != nil {
		return result, errors.New("Hako API returned an invalid Workspace response")
	}
	result = WorkspaceCreateResult{
		ID: wire.Workspace.ID, TenantID: wire.Workspace.TenantID, OwnerID: wire.Workspace.OwnerID, Name: wire.Workspace.Name,
		DesiredState: wire.Status.DesiredState, ObservedState: wire.Status.ObservedState,
		OperationID: wire.OperationID, OperationState: wire.OperationState,
	}
	return result, nil
}

// ListWorkspaces retrieves one bounded page visible to the authenticated user.
func ListWorkspaces(ctx context.Context, baseURL, accessToken, tenantID string, limit, offset int, client *http.Client) (WorkspaceListResult, error) {
	var result WorkspaceListResult
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		return result, errors.New("limit must be 1-100 and offset must be 0-1000000")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces")
	if err != nil {
		return result, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return result, errors.New("invalid Hako API endpoint")
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	parsed.RawQuery = query.Encode()
	response, err := doAPIRequest(ctx, parsed.String(), accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return WorkspaceListResult{}, errors.New("Hako API returned an invalid Workspace list response")
	}
	return result, nil
}

// GetTenantUsage retrieves aggregate Workspace quota use for a Tenant member.
func GetTenantUsage(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (TenantUsage, error) {
	var result TenantUsage
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/usage")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil || result.WorkspacesUsed < 0 || result.WorkspacesLimit < 1 || result.WorkloadRunsUsed < 0 || result.WorkloadRunsLimit < 1 ||
		result.QueuedWorkloadRunsUsed < 0 || result.QueuedWorkloadRunsLimit < 1 ||
		result.QueuedWorkloadRunsLimit > result.WorkloadRunsLimit || result.PreviewRunsUsed < 0 || result.PreviewRunsLimit < 1 || result.PreviewLifetimeLimit < 300 || result.PreviewLifetimeLimit > 86400 || result.ArtifactBytesUsed < 0 || result.ArtifactBytesReserved < 0 || result.ArtifactBytesLimit < 1 ||
		result.WorkspaceComputeCPUMilliUsed < 0 || result.WorkspaceComputeMemoryMiBUsed < 0 || result.WorkspaceComputeProfilesMissing < 0 ||
		result.WorkspaceComputeCPUMilliLimit < 0 || result.WorkspaceComputeCPUMilliLimit > 80000 || result.WorkspaceComputeMemoryMiBLimit < 0 || result.WorkspaceComputeMemoryMiBLimit > 163840 {
		return TenantUsage{}, errors.New("Hako API returned invalid Tenant quota usage")
	}
	return result, nil
}

func GetTenantQuotaLimits(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (TenantQuotaLimits, error) {
	var result TenantQuotaLimits
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/quotas")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.WorkspacesLimit < 1 || result.WorkspacesLimit > 10 || result.WorkloadRunsLimit < 1 || result.WorkloadRunsLimit > 5 ||
		result.QueuedWorkloadRunsLimit < 1 || result.QueuedWorkloadRunsLimit > result.WorkloadRunsLimit ||
		result.PreviewRunsLimit < 1 || result.PreviewRunsLimit > 5 || result.PreviewLifetimeSecondsLimit < 300 || result.PreviewLifetimeSecondsLimit > 86400 ||
		result.ArtifactBytesLimit < 1 || result.ArtifactBytesLimit > 100<<30 || result.WorkspaceServiceMemoryBytesLimit < 1 ||
		result.WorkspaceServiceMemoryBytesLimit > 2<<30 || result.WorkspaceServiceMilliCPULimit < 1 || result.WorkspaceServiceMilliCPULimit > 1500 ||
		result.WorkspaceComputeMilliCPULimit < 0 || result.WorkspaceComputeMilliCPULimit > 80000 || result.WorkspaceComputeMemoryMiBLimit < 0 || result.WorkspaceComputeMemoryMiBLimit > 163840 {
		return TenantQuotaLimits{}, errors.New("Hako API returned invalid Tenant quota limits")
	}
	return result, nil
}

func SetTenantQuotaLimits(ctx context.Context, baseURL, accessToken, tenantID string, limits TenantQuotaLimits, client *http.Client) (TenantQuotaLimits, error) {
	var result TenantQuotaLimits
	if !validTenantID(tenantID) || limits.WorkspacesLimit < 1 || limits.WorkspacesLimit > 10 || limits.WorkloadRunsLimit < 1 || limits.WorkloadRunsLimit > 5 ||
		limits.QueuedWorkloadRunsLimit < 1 || limits.QueuedWorkloadRunsLimit > limits.WorkloadRunsLimit ||
		limits.PreviewRunsLimit < 1 || limits.PreviewRunsLimit > 5 || limits.PreviewLifetimeSecondsLimit < 300 || limits.PreviewLifetimeSecondsLimit > 86400 ||
		limits.ArtifactBytesLimit < 1 || limits.ArtifactBytesLimit > 100<<30 || limits.WorkspaceServiceMemoryBytesLimit < 1 ||
		limits.WorkspaceServiceMemoryBytesLimit > 2<<30 || limits.WorkspaceServiceMilliCPULimit < 1 || limits.WorkspaceServiceMilliCPULimit > 1500 ||
		limits.WorkspaceComputeMilliCPULimit < 0 || limits.WorkspaceComputeMilliCPULimit > 80000 || limits.WorkspaceComputeMemoryMiBLimit < 0 || limits.WorkspaceComputeMemoryMiBLimit > 163840 {
		return result, errors.New("Tenant limits are out of range for Workspaces, Workloads, artifacts, or Workspace Services")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/quotas")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]any{"workspaces_limit": limits.WorkspacesLimit, "workload_runs_limit": limits.WorkloadRunsLimit,
		"queued_workload_runs_limit": limits.QueuedWorkloadRunsLimit,
		"preview_runs_limit":         limits.PreviewRunsLimit, "preview_lifetime_seconds_limit": limits.PreviewLifetimeSecondsLimit,
		"artifact_bytes_limit": limits.ArtifactBytesLimit, "service_memory_bytes_limit": limits.WorkspaceServiceMemoryBytesLimit,
		"service_milli_cpu_limit":            limits.WorkspaceServiceMilliCPULimit,
		"workspace_compute_milli_cpu_limit":  limits.WorkspaceComputeMilliCPULimit,
		"workspace_compute_memory_mib_limit": limits.WorkspaceComputeMemoryMiBLimit})
	if err != nil {
		return result, err
	}
	response, err := doAPIRequestWithBody(ctx, http.MethodPut, endpoint, accessToken, body, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.WorkspacesLimit < 1 || result.WorkspacesLimit > 10 || result.WorkloadRunsLimit < 1 || result.WorkloadRunsLimit > 5 ||
		result.QueuedWorkloadRunsLimit < 1 || result.QueuedWorkloadRunsLimit > result.WorkloadRunsLimit ||
		result.PreviewRunsLimit < 1 || result.PreviewRunsLimit > 5 || result.PreviewLifetimeSecondsLimit < 300 || result.PreviewLifetimeSecondsLimit > 86400 ||
		result.ArtifactBytesLimit < 1 || result.ArtifactBytesLimit > 100<<30 || result.WorkspaceServiceMemoryBytesLimit < 1 ||
		result.WorkspaceServiceMemoryBytesLimit > 2<<30 || result.WorkspaceServiceMilliCPULimit < 1 || result.WorkspaceServiceMilliCPULimit > 1500 ||
		result.WorkspaceComputeMilliCPULimit < 0 || result.WorkspaceComputeMilliCPULimit > 80000 || result.WorkspaceComputeMemoryMiBLimit < 0 || result.WorkspaceComputeMemoryMiBLimit > 163840 {
		return TenantQuotaLimits{}, errors.New("Hako API returned invalid Tenant quota limits")
	}
	return result, nil
}

// GetWorkspace retrieves a Workspace only if Tenant membership and the
// Workspace visibility policy permit access.
func GetWorkspace(ctx context.Context, baseURL, accessToken, tenantID, workspaceID string, client *http.Client) (WorkspaceView, error) {
	var result WorkspaceView
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID))
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return WorkspaceView{}, errors.New("Hako API returned an invalid Workspace response")
	}
	return result, nil
}

// GetServiceCatalog returns the API's currently available built-in service templates.
func GetServiceCatalog(ctx context.Context, baseURL, accessToken string, client *http.Client) (ServiceCatalogResult, error) {
	var result ServiceCatalogResult
	endpoint, err := apiEndpoint(baseURL, "/v1/service-catalog")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return ServiceCatalogResult{}, errors.New("Hako API returned an invalid Service Catalog response")
	}
	return result, nil
}

// ListWorkspaceServices returns catalog instances visible to the caller.
func ListWorkspaceServices(ctx context.Context, baseURL, accessToken, tenantID, workspaceID string, client *http.Client) (WorkspaceServiceListResult, error) {
	var result WorkspaceServiceListResult
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/services")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return WorkspaceServiceListResult{}, errors.New("Hako API returned an invalid Workspace Service list response")
	}
	return result, nil
}

// GetWorkspaceServiceResourceUsage retrieves configured Service reservations
// for a Workspace the caller is authorized to see; values are not telemetry.
func GetWorkspaceServiceResourceUsage(ctx context.Context, baseURL, accessToken, tenantID, workspaceID string, client *http.Client) (WorkspaceServiceResourceUsage, error) {
	var result WorkspaceServiceResourceUsage
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/services/usage")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.MemoryBytesUsed < 0 || result.MemoryBytesLimit < 1 ||
		result.MilliCPUUsed < 0 || result.MilliCPULimit < 1 {
		return WorkspaceServiceResourceUsage{}, errors.New("Hako API returned invalid Workspace Service resource usage")
	}
	return result, nil
}

// AddWorkspaceService requests a desired-running catalog Service. Retrying
// the same Workspace/name/template is idempotent; no runtime is launched by
// this metadata API until the Service executor is enabled.
func AddWorkspaceService(ctx context.Context, baseURL, accessToken, tenantID, workspaceID string, add AddWorkspaceServiceRequest, client *http.Client) (domain.Service, error) {
	var result domain.Service
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	if strings.TrimSpace(add.CatalogName) == "" || len(add.CatalogName) > 64 || strings.ContainsAny(add.CatalogName, "\r\n\x00") {
		return result, errors.New("catalog name is required")
	}
	if add.Name != "" && !servicecatalog.ValidateInstanceName(add.Name) {
		return result, errors.New("service name must start with a lowercase letter and contain only lowercase letters, numbers, or hyphens (max 32 characters)")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/services")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(add)
	if err != nil {
		return result, fmt.Errorf("encode Service request: %w", err)
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return domain.Service{}, errors.New("Hako API returned an invalid Workspace Service response")
	}
	return result, nil
}

// RequestWorkspaceServiceAction records a desired state transition. A 202
// response means the request is accepted, not that MicroVM execution finished.
func RequestWorkspaceServiceAction(ctx context.Context, baseURL, accessToken, tenantID, workspaceID, serviceName, action string, client *http.Client) (domain.Service, error) {
	var result domain.Service
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) || !servicecatalog.ValidateInstanceName(serviceName) {
		return result, errors.New("Tenant, Workspace, or Service name is invalid")
	}
	if action != "start" && action != "stop" && action != "remove" && action != "rotate-credentials" {
		return result, errors.New("Service action must be start, stop, remove, or rotate-credentials")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/services/"+url.PathEscape(serviceName)+"/actions")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Action string `json:"action"`
	}{Action: action})
	if err != nil {
		return result, fmt.Errorf("encode Service action: %w", err)
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return domain.Service{}, errors.New("Hako API returned an invalid Workspace Service response")
	}
	return result, nil
}

// RequestWorkspaceAction submits an asynchronous suspend, resume, or delete
// request. Reuse idempotencyKey when retrying the same logical request.
func RequestWorkspaceAction(ctx context.Context, baseURL, accessToken, tenantID, workspaceID, action, idempotencyKey string, client *http.Client) (WorkspaceActionResult, error) {
	var result WorkspaceActionResult
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	if action != "suspend" && action != "resume" && action != "delete" {
		return result, errors.New("action must be suspend, resume, or delete")
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return result, errors.New("a valid idempotency key is required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/actions")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Action string `json:"action"`
	}{Action: action})
	if err != nil {
		return result, fmt.Errorf("encode Workspace action: %w", err)
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return WorkspaceActionResult{}, errors.New("Hako API returned an invalid Workspace action response")
	}
	return result, nil
}

// CreateWorkspaceSession obtains a short-lived Gateway ticket after the API
// has applied Tenant and Workspace visibility checks. Treat Ticket as a secret
// and never log it.
func CreateWorkspaceSession(ctx context.Context, baseURL, accessToken, tenantID, workspaceID string, permissions []workspacesession.Permission, client *http.Client) (WorkspaceSession, error) {
	var result WorkspaceSession
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) {
		return result, errors.New("Tenant and Workspace IDs must contain only letters, numbers, underscores, or hyphens")
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	if len(permissions) > 0 {
		if err := workspacesession.ValidatePermissions(permissions); err != nil {
			return result, errors.New("Workspace session permissions are invalid")
		}
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/sessions")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Permissions []workspacesession.Permission `json:"permissions,omitempty"`
	}{Permissions: permissions})
	if err != nil {
		return result, fmt.Errorf("encode Workspace session request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || decoder.Decode(new(any)) != io.EOF ||
		!validOpaqueID(string(result.SessionID)) || result.Ticket == "" || len(result.Ticket) > 8192 || result.ExpiresAt.IsZero() ||
		workspacesession.ValidatePermissions(result.Permissions) != nil {
		return WorkspaceSession{}, errors.New("Hako API returned an invalid Workspace Session response")
	}
	return result, nil
}

// RevokeWorkspaceSession removes the persisted ticket record. Gateways must
// check that record at connection establishment and reject revoked sessions.
func RevokeWorkspaceSession(ctx context.Context, baseURL, accessToken, tenantID, workspaceID, sessionID string, client *http.Client) error {
	if !validTenantID(tenantID) || !validOpaqueID(workspaceID) || len(sessionID) != 48 {
		return errors.New("Tenant, Workspace, or Session ID is invalid")
	}
	if strings.TrimSpace(accessToken) == "" {
		return errors.New("access token is required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workspaces/"+url.PathEscape(workspaceID)+"/sessions/"+url.PathEscape(sessionID))
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return readAPIError(response)
	}
	return nil
}

// CreateWorkloadRun requests an immutable Job Run and its transactional schedule event.
func CreateWorkloadRun(ctx context.Context, baseURL, accessToken, tenantID, idempotencyKey string, create WorkloadRunCreateRequest, client *http.Client) (domain.WorkloadRun, error) {
	var result domain.WorkloadRun
	if !validTenantID(tenantID) || create.GitHubInstallationID <= 0 || create.GitHubRepositoryID <= 0 ||
		!validCommitSHA(create.CommitSHA) || create.TimeoutSeconds < 0 || create.TimeoutSeconds > 24*60*60 {
		return result, errors.New("Tenant, active GitHub Installation/Repository, immutable commit SHA, and timeout are invalid")
	}
	if err := idempotency.ValidateKey(idempotencyKey); err != nil {
		return result, err
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs")
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(create)
	if err != nil {
		return result, fmt.Errorf("encode Workload Run request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil || result.ID == "" || string(result.TenantID) != tenantID {
		return domain.WorkloadRun{}, errors.New("Hako API returned an invalid Workload Run creation response")
	}
	return result, nil
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !(char >= '0' && char <= '9') && !(char >= 'a' && char <= 'f') {
			return false
		}
	}
	return true
}

// ListWorkloadRuns retrieves one bounded page of Runs visible to the caller.
func ListWorkloadRuns(ctx context.Context, baseURL, accessToken, tenantID string, limit, offset int, client *http.Client) (WorkloadRunListResult, error) {
	var result WorkloadRunListResult
	if !validTenantID(tenantID) {
		return result, errors.New("tenant ID must contain only letters, numbers, underscores, or hyphens")
	}
	if limit < 1 || limit > 100 || offset < 0 || offset > 1000000 {
		return result, errors.New("limit must be 1-100 and offset must be 0-1000000")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs")
	if err != nil {
		return result, err
	}
	parsed, err := url.Parse(endpoint)
	if err != nil {
		return result, errors.New("invalid Hako API endpoint")
	}
	query := parsed.Query()
	query.Set("limit", strconv.Itoa(limit))
	query.Set("offset", strconv.Itoa(offset))
	parsed.RawQuery = query.Encode()
	response, err := doAPIRequest(ctx, parsed.String(), accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return WorkloadRunListResult{}, errors.New("Hako API returned an invalid Workload Run list response")
	}
	return result, nil
}

// GetWorkloadRun retrieves a Tenant-scoped Run if the caller's role/requester
// visibility permits access.
func GetWorkloadRun(ctx context.Context, baseURL, accessToken, tenantID, runID string, client *http.Client) (domain.WorkloadRun, error) {
	var result domain.WorkloadRun
	if !validTenantID(tenantID) || !validOpaqueID(runID) {
		return result, errors.New("Tenant and Workload Run IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs/"+url.PathEscape(runID))
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return domain.WorkloadRun{}, errors.New("Hako API returned an invalid Workload Run response")
	}
	return result, nil
}

// GetWorkloadAgentPublication retrieves safe publication state for a Run
// visible to the caller. The response contains no GitHub token or artifact URI.
func GetWorkloadAgentPublication(ctx context.Context, baseURL, accessToken, tenantID, runID string,
	client *http.Client) (WorkloadAgentPublicationStatus, error) {
	var result WorkloadAgentPublicationStatus
	if !validTenantID(tenantID) || !validOpaqueID(runID) {
		return result, errors.New("Tenant and Workload Run IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs/"+url.PathEscape(runID)+"/agent-publication")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.TenantID != tenantID || result.WorkloadRunID != runID || result.BaseBranch == "" || result.BranchName == "" ||
		result.CreatedAt.IsZero() || result.UpdatedAt.IsZero() ||
		(result.State == workloads.AgentProposalPublicationRequested && (result.BranchCommitSHA != "" || result.PullRequestNumber != 0)) ||
		(result.State == workloads.AgentProposalPublicationBranchCreated && (!validCommitSHA(result.BranchCommitSHA) || result.PullRequestNumber != 0)) ||
		(result.State == workloads.AgentProposalPublicationPullRequestCreated && (!validCommitSHA(result.BranchCommitSHA) || result.PullRequestNumber < 1)) ||
		(result.State != workloads.AgentProposalPublicationRequested && result.State != workloads.AgentProposalPublicationBranchCreated &&
			result.State != workloads.AgentProposalPublicationPullRequestCreated) {
		return WorkloadAgentPublicationStatus{}, errors.New("Hako API returned an invalid Agent publication status")
	}
	return result, nil
}

// SubmitWorkloadAgentPublication submits or retries the latest Agent proposal
// through the authenticated Control Plane. Repeating this request is safe.
func SubmitWorkloadAgentPublication(ctx context.Context, baseURL, accessToken, tenantID, runID string,
	client *http.Client) (AgentProposalPublicationSubmission, error) {
	var result AgentProposalPublicationSubmission
	if !validTenantID(tenantID) || !validOpaqueID(runID) {
		return result, errors.New("Tenant and Workload Run IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs/"+url.PathEscape(runID)+"/agent-publication/publish")
	if err != nil {
		return result, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	response, err := doAPIRequestWithBody(requestCtx, http.MethodPost, endpoint, accessToken, nil, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if (response.StatusCode != http.StatusOK && response.StatusCode != http.StatusAccepted) || response.Header.Get("Cache-Control") != "no-store" {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		result.TenantID != tenantID || result.WorkloadRunID != runID ||
		(response.StatusCode == http.StatusAccepted && (result.State != "verification_queued" || result.BaseBranch != "" || result.BranchName != "" || result.BranchCommitSHA != "" || result.PullRequestNumber != 0)) ||
		(response.StatusCode == http.StatusOK && (result.BaseBranch == "" || result.BranchName == "" ||
			(result.State != "branch_created" && result.State != "pull_request_created") ||
			(result.State == "branch_created" && (!validCommitSHA(result.BranchCommitSHA) || result.PullRequestNumber != 0)) ||
			(result.State == "pull_request_created" && (!validCommitSHA(result.BranchCommitSHA) || result.PullRequestNumber < 1)))) {
		return AgentProposalPublicationSubmission{}, errors.New("Hako API returned an invalid Agent publication result")
	}
	return result, nil
}

// CreateWorkloadArtifactDownloadTicket requests a short-lived download
// capability after the API rechecks the caller's Run visibility.
func CreateWorkloadArtifactDownloadTicket(ctx context.Context, baseURL, accessToken, tenantID, runID, artifactID string,
	client *http.Client) (WorkloadArtifactDownloadTicket, error) {
	var result WorkloadArtifactDownloadTicket
	if !validTenantID(tenantID) || !validOpaqueID(runID) || !validOpaqueID(artifactID) {
		return result, errors.New("Tenant, Workload Run, and artifact IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs/"+url.PathEscape(runID)+
		"/artifacts/"+url.PathEscape(artifactID)+"/download-ticket")
	if err != nil {
		return result, err
	}
	response, err := doAPIRequestWithBody(ctx, http.MethodPost, endpoint, accessToken, nil, client)
	if err != nil {
		return result, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		strings.TrimSpace(result.Ticket) == "" || result.ExpiresAt.Before(time.Now().Add(-30*time.Second)) ||
		result.ExpiresAt.After(time.Now().Add(workloadartifactaccess.TicketMaxTTL+30*time.Second)) ||
		result.SizeBytes < 0 || result.SizeBytes > workloadartifactaccess.MaxArtifactSize || len(result.SHA256) != 64 ||
		!validArtifactKind(result.Kind) {
		return WorkloadArtifactDownloadTicket{}, errors.New("Hako API returned an invalid Workload artifact ticket")
	}
	if _, err := hex.DecodeString(result.SHA256); err != nil || strings.ToLower(result.SHA256) != result.SHA256 {
		return WorkloadArtifactDownloadTicket{}, errors.New("Hako API returned an invalid Workload artifact digest")
	}
	return result, nil
}

func validArtifactKind(kind string) bool {
	switch kind {
	case "logs", "test_report", "coverage", "sbom", "binary", "container_image", "agent_changes", "other":
		return true
	default:
		return false
	}
}

// CancelWorkloadRun requests idempotent asynchronous cancellation.
func CancelWorkloadRun(ctx context.Context, baseURL, accessToken, tenantID, runID string, client *http.Client) (domain.WorkloadRun, error) {
	var result domain.WorkloadRun
	if !validTenantID(tenantID) || !validOpaqueID(runID) {
		return result, errors.New("Tenant and Workload Run IDs must contain only letters, numbers, underscores, or hyphens")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/workload-runs/"+url.PathEscape(runID)+"/cancel")
	if err != nil {
		return result, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return result, fmt.Errorf("create Hako API request: %w", err)
	}
	if strings.TrimSpace(accessToken) == "" {
		return result, errors.New("access token is required")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	client = safeAPIClient(client)
	response, err := client.Do(request)
	if err != nil {
		return result, fmt.Errorf("call Hako API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusAccepted {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return domain.WorkloadRun{}, errors.New("Hako API returned an invalid Workload Run cancellation response")
	}
	return result, nil
}

func readAPIError(response *http.Response) error {
	var wire struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&wire)
	return &APIError{StatusCode: response.StatusCode, Code: wire.Error.Code, Message: wire.Error.Message}
}

func apiEndpoint(rawBaseURL, path string) (string, error) {
	base, err := url.Parse(strings.TrimSpace(rawBaseURL))
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" || (base.Path != "" && base.Path != "/") {
		return "", errors.New("HAKO_API_URL must be an API origin URL without a path, query, or fragment")
	}
	if base.Scheme != "https" && !(base.Scheme == "http" && isLoopbackHost(base.Hostname())) {
		return "", errors.New("Hako API URL must use HTTPS except for loopback development URLs")
	}
	base.Path = path
	return base.String(), nil
}

func doAPIRequest(ctx context.Context, endpoint, accessToken string, client *http.Client) (*http.Response, error) {
	return doAPIRequestWithBody(ctx, http.MethodGet, endpoint, accessToken, nil, client)
}

func doAPIRequestWithBody(ctx context.Context, method, endpoint, accessToken string, body []byte, client *http.Client) (*http.Response, error) {
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("access token is required")
	}
	client = safeAPIClient(client)
	request, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Hako API: %w", err)
	}
	return response, nil
}

func safeAPIClient(client *http.Client) *http.Client {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func validTenantID(tenantID string) bool {
	if tenantID == "" || len(tenantID) > 128 {
		return false
	}
	for _, r := range tenantID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}

func validOpaqueID(id string) bool {
	return validTenantID(id)
}

func isLoopbackHost(host string) bool {
	return host == "localhost" || host == "127.0.0.1" || host == "::1"
}
