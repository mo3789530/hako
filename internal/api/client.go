package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/idempotency"
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

type WorkspaceActionResult struct {
	Workspace      WorkspaceView `json:"workspace"`
	OperationID    string        `json:"operation_id"`
	OperationType  string        `json:"operation_type"`
	OperationState string        `json:"operation_state"`
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
	if strings.TrimSpace(accessToken) == "" {
		return nil, errors.New("access token is required")
	}
	if client == nil {
		client = &http.Client{
			Timeout: 10 * time.Second,
		}
	} else {
		clientCopy := *client
		client = &clientCopy
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create Hako API request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := client.Do(request)
	if err != nil {
		return nil, fmt.Errorf("call Hako API: %w", err)
	}
	return response, nil
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
