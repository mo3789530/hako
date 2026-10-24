package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/mo3789530/hako/internal/deploypolicy"
)

var softwareCatalogServiceIDPattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

type SoftwareCatalogDeployPolicyResult struct {
	ServiceID        string              `json:"service_id"`
	Policy           deploypolicy.Policy `json:"policy"`
	ExecutionEnabled bool                `json:"execution_enabled"`
}

type SoftwareCatalogDeployPolicyVerification struct {
	Valid            bool   `json:"valid"`
	ExecutionEnabled bool   `json:"execution_enabled"`
	Repository       string `json:"repository"`
	RepositoryID     string `json:"repository_id"`
	Ref              string `json:"ref"`
	Environment      string `json:"environment"`
	WorkflowRef      string `json:"workflow_ref"`
	EventName        string `json:"event_name"`
	RunID            string `json:"run_id"`
	RunAttempt       string `json:"run_attempt"`
}

func GetSoftwareCatalogDeployPolicy(ctx context.Context, baseURL, accessToken, tenantID, serviceID string, client *http.Client) (SoftwareCatalogDeployPolicyResult, error) {
	var result SoftwareCatalogDeployPolicyResult
	endpoint, err := softwareCatalogDeployPolicyEndpoint(baseURL, accessToken, tenantID, serviceID)
	if err != nil {
		return result, err
	}
	response, err := doAPIRequest(ctx, endpoint, accessToken, client)
	if err != nil {
		return result, errors.New("Software Catalog deploy policy request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := decodeSoftwareCatalogJSON(response, &result); err != nil || result.ServiceID != serviceID || result.ExecutionEnabled || result.Policy.Validate() != nil {
		return SoftwareCatalogDeployPolicyResult{}, errors.New("invalid Software Catalog deploy policy response")
	}
	return result, nil
}

func SetSoftwareCatalogDeployPolicy(ctx context.Context, baseURL, accessToken, tenantID, serviceID string,
	policy deploypolicy.Policy, client *http.Client) (SoftwareCatalogDeployPolicyResult, error) {
	var result SoftwareCatalogDeployPolicyResult
	endpoint, err := softwareCatalogDeployPolicyEndpoint(baseURL, accessToken, tenantID, serviceID)
	if err != nil {
		return result, err
	}
	if policy.Validate() != nil {
		return result, deploypolicy.ErrInvalid
	}
	body, err := json.Marshal(policy)
	if err != nil {
		return result, errors.New("encode Software Catalog deploy policy")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create Software Catalog deploy policy request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, errors.New("Software Catalog deploy policy request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := decodeSoftwareCatalogJSON(response, &result); err != nil || result.ServiceID != serviceID || result.ExecutionEnabled || result.Policy.Validate() != nil || result.Policy.Enabled != policy.Enabled {
		return SoftwareCatalogDeployPolicyResult{}, errors.New("invalid Software Catalog deploy policy response")
	}
	return result, nil
}

func VerifySoftwareCatalogDeployPolicy(ctx context.Context, baseURL, accessToken, tenantID, serviceID, oidcToken string, client *http.Client) (SoftwareCatalogDeployPolicyVerification, error) {
	var result SoftwareCatalogDeployPolicyVerification
	endpoint, err := softwareCatalogDeployPolicyEndpoint(baseURL, accessToken, tenantID, serviceID)
	if err != nil {
		return result, err
	}
	if len(oidcToken) == 0 || len(oidcToken) > 64<<10 || strings.TrimSpace(oidcToken) != oidcToken {
		return result, errors.New("bounded GitHub OIDC token is required")
	}
	endpoint = strings.TrimRight(endpoint, "/") + "/verify"
	body, err := json.Marshal(struct {
		Token string `json:"token"`
	}{Token: oidcToken})
	if err != nil {
		return result, errors.New("encode GitHub OIDC verification request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create GitHub OIDC verification request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, errors.New("GitHub OIDC verification request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if err := decodeSoftwareCatalogJSON(response, &result); err != nil || !result.Valid || result.ExecutionEnabled || result.Repository == "" || result.RepositoryID == "" || result.Ref == "" || result.Environment == "" || result.WorkflowRef == "" {
		return SoftwareCatalogDeployPolicyVerification{}, errors.New("invalid GitHub OIDC verification response")
	}
	return result, nil
}

func softwareCatalogDeployPolicyEndpoint(baseURL, accessToken, tenantID, serviceID string) (string, error) {
	tenantID, serviceID = strings.TrimSpace(tenantID), strings.TrimSpace(serviceID)
	if !validTenantID(tenantID) || !softwareCatalogServiceIDPattern.MatchString(serviceID) || strings.TrimSpace(accessToken) == "" {
		return "", errors.New("Tenant ID, service ID, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/software-catalog/" + url.PathEscape(serviceID) + "/deploy-policy"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return "", errors.New("invalid Hako API URL")
	}
	return endpoint, nil
}
