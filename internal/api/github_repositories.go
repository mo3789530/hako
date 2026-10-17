package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/mo3789530/hako/internal/store/githubregistry"
)

type GitHubRepositoryFactoryResult struct {
	Repository githubregistry.Repository `json:"repository"`
}

type GitHubRepositoryActionsRunnerResult struct {
	Repository githubregistry.Repository `json:"repository"`
}

type GitHubRepositoryActionsRunnerWorkflowPolicyResult struct {
	Policy githubregistry.ActionsRunnerWorkflowPolicy `json:"workflow_policy"`
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
