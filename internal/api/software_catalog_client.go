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
	"strings"

	"github.com/mo3789530/hako/internal/softwarecatalog"
)

type SoftwareCatalogListResult struct {
	Items []softwarecatalog.Service `json:"items"`
}

func ListSoftwareCatalog(ctx context.Context, baseURL, accessToken, tenantID string, client *http.Client) (SoftwareCatalogListResult, error) {
	var result SoftwareCatalogListResult
	if ctx == nil || strings.TrimSpace(tenantID) == "" || strings.TrimSpace(accessToken) == "" {
		return result, errors.New("Tenant ID and access token are required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(strings.TrimSpace(tenantID))+"/software-catalog")
	if err != nil {
		return result, errors.New("invalid Hako API URL")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return result, errors.New("create Software Catalog request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, errors.New("Software Catalog request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, fmt.Errorf("Software Catalog request failed with HTTP %d", response.StatusCode)
	}
	if err := decodeSoftwareCatalogJSON(response, &result); err != nil {
		return SoftwareCatalogListResult{}, errors.New("decode Software Catalog response")
	}
	for _, item := range result.Items {
		if item.TenantID != strings.TrimSpace(tenantID) || softwarecatalog.Validate(item) != nil || !validSoftwareCatalogRepository(item.Repository) {
			return SoftwareCatalogListResult{}, errors.New("Software Catalog response contains invalid service metadata")
		}
	}
	return result, nil
}

func UpsertSoftwareCatalogService(ctx context.Context, baseURL, accessToken string, item softwarecatalog.Service, client *http.Client) (softwarecatalog.Service, error) {
	if ctx == nil || strings.TrimSpace(accessToken) == "" || softwarecatalog.Validate(item) != nil {
		return softwarecatalog.Service{}, errors.New("valid Software Catalog service and access token are required")
	}
	endpoint, err := apiEndpoint(baseURL, "/v1/tenants/"+url.PathEscape(item.TenantID)+"/software-catalog/"+url.PathEscape(item.ServiceID))
	if err != nil {
		return softwarecatalog.Service{}, errors.New("invalid Hako API URL")
	}
	body, err := json.Marshal(struct {
		OwnerTeam          string   `json:"owner_team"`
		InstallationID     int64    `json:"installation_id"`
		GitHubRepositoryID int64    `json:"github_repository_id"`
		Runtime            string   `json:"runtime"`
		Language           string   `json:"language"`
		Dependencies       []string `json:"dependencies"`
		ProductionURL      string   `json:"production_url,omitempty"`
	}{item.OwnerTeam, item.InstallationID, item.GitHubRepositoryID, item.Runtime, item.Language, item.Dependencies, item.ProductionURL})
	if err != nil {
		return softwarecatalog.Service{}, errors.New("encode Software Catalog service")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return softwarecatalog.Service{}, errors.New("create Software Catalog request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return softwarecatalog.Service{}, errors.New("Software Catalog request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return softwarecatalog.Service{}, fmt.Errorf("Software Catalog request failed with HTTP %d", response.StatusCode)
	}
	var result softwarecatalog.Service
	if err := decodeSoftwareCatalogJSON(response, &result); err != nil {
		return softwarecatalog.Service{}, errors.New("decode Software Catalog response")
	}
	if result.TenantID != item.TenantID || result.ServiceID != item.ServiceID || softwarecatalog.Validate(result) != nil || !validSoftwareCatalogRepository(result.Repository) {
		return softwarecatalog.Service{}, errors.New("Software Catalog response identity mismatch")
	}
	return result, nil
}

func validSoftwareCatalogRepository(value string) bool {
	if strings.Count(value, "/") != 1 {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." {
			return false
		}
		for _, r := range part {
			if !(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && !(r >= '0' && r <= '9') && r != '-' && r != '_' && r != '.' {
				return false
			}
		}
	}
	return true
}

func decodeSoftwareCatalogJSON(response *http.Response, target any) error {
	if response == nil || response.Body == nil {
		return errors.New("missing Software Catalog response body")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAPIResponseSize+1))
	if err != nil || len(data) > maxAPIResponseSize {
		return errors.New("Software Catalog response exceeds its size limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return errors.New("Software Catalog response has trailing JSON")
	}
	return nil
}
