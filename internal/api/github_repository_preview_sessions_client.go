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

	"github.com/mo3789530/hako/internal/workspacesession"
)

func CreateRepositoryPreviewSession(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID, pullRequestNumber int64,
	permissions []workspacesession.Permission, client *http.Client) (RepositoryPreviewSession, error) {
	var result RepositoryPreviewSession
	if ctx == nil || !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 || pullRequestNumber <= 0 {
		return result, errors.New("valid Tenant, Preview identity, and access token are required")
	}
	if len(permissions) > 0 && workspacesession.ValidatePermissions(permissions) != nil {
		return result, errors.New("Preview session permissions are invalid")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/previews/" + strconv.FormatInt(pullRequestNumber, 10) + "/sessions"
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return result, err
	}
	body, err := json.Marshal(struct {
		Permissions []workspacesession.Permission `json:"permissions,omitempty"`
	}{Permissions: permissions})
	if err != nil {
		return result, errors.New("encode Preview session request")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return result, errors.New("create Preview session request")
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	request.Header.Set("Content-Type", "application/json")
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return result, errors.New("Preview session request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		return result, readAPIError(response)
	}
	decoder := json.NewDecoder(io.LimitReader(response.Body, maxAPIResponseSize))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || decoder.Decode(new(any)) != io.EOF ||
		!validPreviewSessionID(string(result.SessionID)) || !validPreviewRuntimeWorkspaceID(string(result.WorkspaceID)) || result.Ticket == "" || len(result.Ticket) > 8192 ||
		!result.ExpiresAt.After(time.Now()) || result.ExpiresAt.After(time.Now().Add(workspacesession.TicketMaxTTL+time.Minute)) ||
		workspacesession.ValidatePermissions(result.Permissions) != nil {
		return RepositoryPreviewSession{}, errors.New("Hako API returned an invalid Preview session response")
	}
	return result, nil
}

func validPreviewSessionID(value string) bool {
	if len(value) != 48 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 24
}

func validPreviewRuntimeWorkspaceID(value string) bool {
	const prefix = "prvws_"
	if len(value) != len(prefix)+32 || !strings.HasPrefix(value, prefix) {
		return false
	}
	decoded, err := hex.DecodeString(strings.TrimPrefix(value, prefix))
	return err == nil && len(decoded) == 16
}

func RevokeRepositoryPreviewSession(ctx context.Context, baseURL, accessToken, tenantID string, installationID, repositoryID, pullRequestNumber int64,
	sessionID string, client *http.Client) error {
	if ctx == nil || !validTenantID(tenantID) || strings.TrimSpace(accessToken) == "" || installationID <= 0 || repositoryID <= 0 || pullRequestNumber <= 0 ||
		len(sessionID) != 48 {
		return errors.New("valid Tenant, Preview identity, Session ID, and access token are required")
	}
	path := "/v1/tenants/" + url.PathEscape(tenantID) + "/github/installations/" + strconv.FormatInt(installationID, 10) +
		"/repositories/" + strconv.FormatInt(repositoryID, 10) + "/previews/" + strconv.FormatInt(pullRequestNumber, 10) + "/sessions/" + url.PathEscape(sessionID)
	endpoint, err := apiEndpoint(baseURL, path)
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete, endpoint, nil)
	if err != nil {
		return fmt.Errorf("create Preview session revoke request: %w", err)
	}
	request.Header.Set("Authorization", "Bearer "+accessToken)
	response, err := safeAPIClient(client).Do(request)
	if err != nil {
		return errors.New("Preview session revoke request failed")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent {
		return readAPIError(response)
	}
	return nil
}
