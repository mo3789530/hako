package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

type ModelCredentialCommand struct {
	Action             string
	TenantID           string
	Provider           string
	Purpose            string
	BindingID          string
	SecretVersionID    string
	ExpectedGeneration int64
}

func ManageModelCredential(ctx context.Context, baseURL, accessToken string, command ModelCredentialCommand, client *http.Client) (ModelCredentialResult, error) {
	var result ModelCredentialResult
	if ctx == nil || !validTenantID(command.TenantID) || strings.TrimSpace(accessToken) == "" {
		return result, errors.New("valid Tenant and access token are required")
	}
	if command.Provider == "" && command.Action != "list" {
		command.Provider = modelcredentials.ProviderAnthropic
	}
	if command.Action != "list" && strings.TrimSpace(command.Purpose) == "" {
		command.Purpose = modelcredentials.PurposeClaudeAction
	}
	path := "/v1/tenants/" + url.PathEscape(command.TenantID) + "/model-credentials"
	method := http.MethodGet
	var body []byte
	switch command.Action {
	case "list":
	case "add":
		if command.Provider != modelcredentials.ProviderAnthropic || !modelcredentials.ValidPurpose(command.Purpose) || strings.TrimSpace(command.BindingID) == "" || !modelcredentials.ValidSecretVersionID(command.SecretVersionID) {
			return result, errors.New("add requires an approved binding and a 32-character Secret version ID")
		}
		method = http.MethodPost
		body, _ = json.Marshal(ModelCredentialRequest{Provider: command.Provider, Purpose: command.Purpose, BindingID: command.BindingID, SecretVersionID: command.SecretVersionID})
	case "rotate":
		if command.Provider != modelcredentials.ProviderAnthropic || !modelcredentials.ValidPurpose(command.Purpose) || command.ExpectedGeneration <= 0 || !modelcredentials.ValidSecretVersionID(command.SecretVersionID) {
			return result, errors.New("rotate requires a 32-character Secret version ID and positive --generation")
		}
		path += "/" + url.PathEscape(command.Provider) + "/rotate"
		method = http.MethodPost
		body, _ = json.Marshal(ModelCredentialRotateRequest{Purpose: command.Purpose, SecretVersionID: command.SecretVersionID, ExpectedGeneration: command.ExpectedGeneration})
	case "disable":
		if command.Provider != modelcredentials.ProviderAnthropic || !modelcredentials.ValidPurpose(command.Purpose) || command.ExpectedGeneration <= 0 {
			return result, errors.New("disable requires positive --generation")
		}
		path += "/" + url.PathEscape(command.Provider) + "/disable"
		method = http.MethodPost
		body, _ = json.Marshal(ModelCredentialDisableRequest{Purpose: command.Purpose, ExpectedGeneration: command.ExpectedGeneration})
	default:
		return result, errors.New("supported model credential actions: list, add, rotate, disable")
	}
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create model credential request")
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	if len(body) > 0 {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := safeAPIClient(client).Do(req)
	if err != nil {
		return result, errors.New("model credential request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return result, readAPIError(response)
	}
	if response.Header.Get("Cache-Control") != "no-store" {
		return result, errors.New("model credential response is cacheable")
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return ModelCredentialResult{}, errors.New("decode model credential response")
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ModelCredentialResult{}, errors.New("model credential response contains trailing data")
	}
	if command.Action == "list" {
		for _, item := range result.Items {
			if string(item.TenantID) != command.TenantID || item.Provider == "" || item.ID == "" || !modelcredentials.ValidPurpose(item.Purpose) {
				return ModelCredentialResult{}, errors.New("model credential response identity mismatch")
			}
		}
	} else if result.Credential == nil || string(result.Credential.TenantID) != command.TenantID || result.Credential.Provider != command.Provider ||
		result.Credential.Purpose != command.Purpose ||
		result.Credential.CredentialGeneration <= 0 || result.Credential.Status == "" {
		return ModelCredentialResult{}, errors.New("model credential response identity mismatch")
	}
	return result, nil
}
