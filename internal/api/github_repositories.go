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
