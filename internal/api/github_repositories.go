package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	previewstore "github.com/mo3789530/hako/internal/store/previews"
)

type GitHubRepositoryFactoryResult struct {
	Repository githubregistry.Repository `json:"repository"`
}

type GitHubRepositoryPreviewResult struct {
	Repository     githubregistry.Repository `json:"repository"`
	RuntimeEnabled bool                      `json:"runtime_enabled"`
}

type GitHubRepositoryPreviewsResult struct {
	Previews       []previewstore.RepositoryPreview `json:"previews"`
	RuntimeEnabled bool                             `json:"runtime_enabled"`
}

type GitHubRepositoryActionsRunnerResult struct {
	Repository githubregistry.Repository `json:"repository"`
}

type GitHubRepositoryActionsRunnerWorkflowPolicyResult struct {
	Policy githubregistry.ActionsRunnerWorkflowPolicy `json:"workflow_policy"`
}

type GitHubRepositoryAgentWritePolicyResult struct {
	Policy           agenttask.WritePolicy `json:"write_policy"`
	ExecutionEnabled bool                  `json:"execution_enabled"`
}

type GitHubRepositoryAgentModelAccessResult struct {
	Access           githubregistry.AgentModelAccess `json:"agent_model_access"`
	ExecutionEnabled bool                            `json:"execution_enabled"`
}

// SetGitHubRepositoryAgentWritePolicy stores an Owner/Admin-approved
// repository branch/PR write ceiling. It does not enable Agent execution.
func SetGitHubRepositoryAgentWritePolicy(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64,
	policy agenttask.WritePolicy, client *http.Client) (GitHubRepositoryAgentWritePolicyResult, error) {
	var result GitHubRepositoryAgentWritePolicyResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 || policy.Validate() != nil {
		return result, errors.New("valid Tenant, Installation, Repository, access token, and Agent write policy are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/agent-write-policy"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return result, errors.New("encode GitHub Repository Agent write policy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Repository Agent write policy request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository Agent write policy request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result) != nil ||
		result.ExecutionEnabled || !reflect.DeepEqual(result.Policy, policy) {
		return GitHubRepositoryAgentWritePolicyResult{}, errors.New("invalid GitHub Repository Agent write policy response")
	}
	return result, nil
}

// SetGitHubRepositoryAgentModelAccess configures a default-off, per-Repository
// gate for future Native Agent model credential use. It does not enable
// scheduling or return credential material.
func SetGitHubRepositoryAgentModelAccess(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64,
	enabled bool, client *http.Client) (GitHubRepositoryAgentModelAccessResult, error) {
	var result GitHubRepositoryAgentModelAccessResult
	if ctx == nil || !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/agent-model-access"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, _ := json.Marshal(repositoryFactoryRequest{Enabled: &enabled})
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Repository Agent model access request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository Agent model access request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || result.ExecutionEnabled || result.Access.Enabled != enabled ||
		(enabled && result.Access.EnabledAt == nil) || (!enabled && result.Access.EnabledAt != nil) {
		return GitHubRepositoryAgentModelAccessResult{}, errors.New("invalid GitHub Repository Agent model access response")
	}
	return result, nil
}

// SetGitHubRepositoryFactoryEnabled configures the opt-in default webhook
// verification Job for one Tenant-owned Installation repository.
func SetGitHubRepositoryFactoryEnabled(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64, enabled bool, client *http.Client) (GitHubRepositoryFactoryResult, error) {
	var result GitHubRepositoryFactoryResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/factory"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return result, errors.New("encode GitHub Repository factory request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Repository factory request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository factory request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		string(result.Repository.TenantID) != tenantID || result.Repository.InstallationID != installationID ||
		result.Repository.GitHubRepositoryID != repositoryID || result.Repository.FactoryEnabled != enabled {
		return GitHubRepositoryFactoryResult{}, errors.New("invalid GitHub Repository factory response")
	}
	return result, nil
}

// SetGitHubRepositoryPreviewEnabled manages Preview metadata opt-in. The
// response must keep RuntimeEnabled false until allocation and Gateway routing
// are implemented and deployed.
func SetGitHubRepositoryPreviewEnabled(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64, enabled bool, client *http.Client) (GitHubRepositoryPreviewResult, error) {
	var result GitHubRepositoryPreviewResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/preview"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return result, errors.New("encode GitHub Repository Preview request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Repository Preview request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository Preview request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil || result.RuntimeEnabled ||
		string(result.Repository.TenantID) != tenantID || result.Repository.InstallationID != installationID ||
		result.Repository.GitHubRepositoryID != repositoryID || result.Repository.PreviewEnabled != enabled {
		return GitHubRepositoryPreviewResult{}, errors.New("invalid GitHub Repository Preview response")
	}
	return result, nil
}

// ListGitHubRepositoryPreviews returns bounded lifecycle metadata for one
// Tenant-owned Repository. Preview runtime allocation remains disabled.
func ListGitHubRepositoryPreviews(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64, limit int, client *http.Client) (GitHubRepositoryPreviewsResult, error) {
	var result GitHubRepositoryPreviewsResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 || limit < 1 || limit > 100 {
		return result, errors.New("valid Tenant, Installation, Repository, access token, and limit between 1 and 100 are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/previews"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	endpoint += "?limit=" + strconv.Itoa(limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, errors.New("create GitHub Repository Previews request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository Previews request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result) != nil || result.RuntimeEnabled || len(result.Previews) > limit {
		return GitHubRepositoryPreviewsResult{}, errors.New("invalid GitHub Repository Previews response")
	}
	for _, item := range result.Previews {
		if string(item.TenantID) != tenantID || item.InstallationID != installationID || item.RepositoryID != repositoryID ||
			item.PullRequestNumber <= 0 || len(item.HeadSHA) != 40 || item.Generation <= 0 || (item.DesiredState != "present" && item.DesiredState != "absent") ||
			(item.State != "awaiting_runtime" && item.State != "cleanup_pending" && item.State != "cleaned") || item.UpdatedAt.IsZero() {
			return GitHubRepositoryPreviewsResult{}, errors.New("invalid GitHub Repository Preview entry")
		}
	}
	return result, nil
}

// SetGitHubRepositoryActionsRunnerEnabled configures the explicit per-repository
// opt-in for ephemeral GitHub Actions runners. It does not start a runner.
func SetGitHubRepositoryActionsRunnerEnabled(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64, enabled bool, client *http.Client) (GitHubRepositoryActionsRunnerResult, error) {
	var result GitHubRepositoryActionsRunnerResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/actions-runner"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(map[string]bool{"enabled": enabled})
	if err != nil {
		return result, errors.New("encode GitHub Repository Actions runner request")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Repository Actions runner request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Repository Actions runner request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil ||
		string(result.Repository.TenantID) != tenantID || result.Repository.InstallationID != installationID ||
		result.Repository.GitHubRepositoryID != repositoryID || result.Repository.ActionsRunnerEnabled != enabled {
		return GitHubRepositoryActionsRunnerResult{}, errors.New("invalid GitHub Repository Actions runner response")
	}
	return result, nil
}

func SetGitHubRepositoryActionsRunnerWorkflowPolicy(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64,
	policy githubregistry.ActionsRunnerWorkflowPolicy, client *http.Client) (GitHubRepositoryActionsRunnerWorkflowPolicyResult, error) {
	var result GitHubRepositoryActionsRunnerWorkflowPolicyResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/actions-runner/workflow-policy"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return result, errors.New("encode GitHub Actions runner workflow policy")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub Actions runner workflow policy request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Actions runner workflow policy request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !sameRunnerWorkflowPolicy(result.Policy, policy) {
		return GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("invalid GitHub Actions runner workflow policy response")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("invalid GitHub Actions runner workflow policy response")
	}
	return result, nil
}

func GetGitHubRepositoryActionsRunnerWorkflowPolicy(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID int64, client *http.Client) (GitHubRepositoryActionsRunnerWorkflowPolicyResult, error) {
	var result GitHubRepositoryActionsRunnerWorkflowPolicyResult
	if !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 {
		return result, errors.New("valid Tenant, Installation, Repository, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/actions-runner/workflow-policy"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, errors.New("create GitHub Actions runner workflow policy request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub Actions runner workflow policy request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil || !githubregistry.ValidActionsRunnerWorkflowPolicy(result.Policy) {
		return GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("invalid GitHub Actions runner workflow policy response")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("invalid GitHub Actions runner workflow policy response")
	}
	return result, nil
}

func sameRunnerWorkflowPolicy(left, right githubregistry.ActionsRunnerWorkflowPolicy) bool {
	return left.WorkflowPath == right.WorkflowPath && equalPolicyValues(left.AllowedRefs, right.AllowedRefs) &&
		equalPolicyValues(left.AllowedEvents, right.AllowedEvents) && equalPolicyValues(left.RequiredLabels, right.RequiredLabels) &&
		equalPolicyValues(left.AllowedLabels, right.AllowedLabels)
}

func equalPolicyValues(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
