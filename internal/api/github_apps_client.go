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
	"time"

	"github.com/mo3789530/hako/internal/idempotency"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

type GitHubAppMetadataRequest struct {
	Action           string
	TenantID         string
	IntegrationID    string
	Draft            githubregistry.AppDraftRequest
	IdempotencyKey   string
	ExpectedRevision int64
	Limit, Offset    int
}

type GitHubAppSetupResult struct {
	SetupURL  string    `json:"setup_url"`
	ExpiresAt time.Time `json:"expires_at"`
}

func BeginTenantGitHubAppSetup(ctx context.Context, baseURL, accessToken, tenantID, integrationID string, client *http.Client) (GitHubAppSetupResult, error) {
	var result GitHubAppSetupResult
	if !validTenantID(tenantID) || !validOpaqueID(integrationID) || strings.TrimSpace(accessToken) == "" {
		return result, errors.New("valid Tenant, integration and access token are required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(tenantID)+"/github/apps/"+url.PathEscape(integrationID)+"/setup")
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader([]byte(`{}`)))
	if err != nil {
		return result, errors.New("create GitHub App setup request failed")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub App setup request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil || !validGitHubSetupURL(result.SetupURL, "/apps/") || result.ExpiresAt.IsZero() {
		return GitHubAppSetupResult{}, errors.New("invalid GitHub App setup response")
	}
	return result, nil
}

func ManageGitHubApp(ctx context.Context, baseURL, accessToken string, input GitHubAppMetadataRequest, client *http.Client) (GitHubAppMetadataResult, error) {
	var result GitHubAppMetadataResult
	if !validTenantID(input.TenantID) || accessToken == "" {
		return result, errors.New("valid Tenant and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(input.TenantID) + "/github/apps"
	method := http.MethodGet
	var body []byte
	switch input.Action {
	case "list":
		if input.Limit == 0 {
			input.Limit = 50
		}
		if input.Limit < 1 || input.Limit > 100 || input.Offset < 0 || input.Offset > 10000 {
			return result, errors.New("invalid App list pagination")
		}
	case "add":
		if idempotency.ValidateKey(input.IdempotencyKey) != nil {
			return result, errors.New("valid idempotency key required")
		}
		request, err := githubregistry.NormalizeAppDraft(input.Draft)
		if err != nil {
			return result, errors.New("valid App metadata and approved binding required")
		}
		body, _ = json.Marshal(request)
		method = http.MethodPost
	case "show", "disable", "verify", "select-default":
		if !validOpaqueID(input.IntegrationID) {
			return result, errors.New("valid App integration ID required")
		}
		path += "/" + url.PathEscape(input.IntegrationID)
		if input.Action != "show" {
			if input.ExpectedRevision < 1 {
				return result, errors.New("positive expected revision required")
			}
			path += "/" + input.Action
			method = http.MethodPost
			body, _ = json.Marshal(map[string]int64{"expected_revision": input.ExpectedRevision})
		}
	default:
		return result, errors.New("unsupported GitHub App metadata action")
	}
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	if input.Action == "list" {
		endpoint += "?limit=" + strconv.Itoa(input.Limit) + "&offset=" + strconv.Itoa(input.Offset)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub App metadata request failed")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if input.Action == "add" {
		req.Header.Set("Idempotency-Key", input.IdempotencyKey)
	}
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("GitHub App metadata request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK && response.StatusCode != http.StatusCreated {
		return result, readAPIError(response)
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize)).Decode(&result); err != nil {
		return GitHubAppMetadataResult{}, errors.New("invalid GitHub App metadata response")
	}
	return result, nil
}
