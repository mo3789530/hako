package workspaceruntime

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const maxWorkspaceServiceDesiredStateBytes = 64 << 10

// SignedWorkspaceServiceCommandResolver fetches the current Control Plane
// desired state before each Guest call. A queued command is a wake-up signal,
// not authority to replay an obsolete Service revision after MicroVM recovery.
type SignedWorkspaceServiceCommandResolver struct {
	endpoint string
	planeID  domain.ResourcePlaneID
	key      []byte
	client   *http.Client
	now      func() time.Time
	newID    func() ([]byte, error)
}

func NewSignedWorkspaceServiceCommandResolver(endpoint string, planeID domain.ResourcePlaneID, key []byte, client *http.Client, now func() time.Time) (*SignedWorkspaceServiceCommandResolver, error) {
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil ||
		(parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.Fragment != "" || planeID == "" || len(key) < 32 {
		return nil, errors.New("Workspace Service desired-state endpoint, Plane, and HMAC key are required")
	}
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &SignedWorkspaceServiceCommandResolver{endpoint: strings.TrimRight(endpoint, "/"), planeID: planeID,
		key: append([]byte(nil), key...), client: noRedirectClient(client), now: now,
		newID: func() ([]byte, error) { value := make([]byte, 24); _, err := rand.Read(value); return value, err }}, nil
}

func (r *SignedWorkspaceServiceCommandResolver) Resolve(ctx context.Context, trigger domain.WorkspaceServiceCommand) (domain.WorkspaceServiceCommand, error) {
	if r == nil || ctx == nil || trigger.ResourcePlaneID != r.planeID || !validWorkspaceServiceCommand(trigger) {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	lookup := struct {
		TenantID    domain.TenantID    `json:"tenant_id"`
		WorkspaceID domain.WorkspaceID `json:"workspace_id"`
	}{TenantID: trigger.TenantID, WorkspaceID: trigger.WorkspaceID}
	body, err := json.Marshal(lookup)
	if err != nil || len(body) > 2<<10 {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	rawID, err := r.newID()
	if err != nil || len(rawID) != 24 {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	requestID := hex.EncodeToString(rawID)
	timestamp := fmt.Sprint(r.now().UTC().Unix())
	path := "/v1/internal/resource-planes/" + url.PathEscape(string(r.planeID)) + "/workspace-services/" +
		url.PathEscape(string(trigger.ServiceID)) + "/desired-state"
	bodyHash := sha256.Sum256(body)
	message := strings.Join([]string{http.MethodPost, path, timestamp, requestID, hex.EncodeToString(bodyHash[:])}, "\n")
	mac := hmac.New(sha256.New, r.key)
	_, _ = mac.Write([]byte(message))
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, r.endpoint+path, bytes.NewReader(body))
	if err != nil {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Hako-Timestamp", timestamp)
	request.Header.Set("X-Hako-Report-ID", requestID)
	request.Header.Set("X-Hako-Signature", hex.EncodeToString(mac.Sum(nil)))
	response, err := r.client.Do(request)
	if err != nil {
		return domain.WorkspaceServiceCommand{}, err
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, maxWorkspaceServiceDesiredStateBytes+1))
	if err != nil || len(responseBody) == 0 || len(responseBody) > maxWorkspaceServiceDesiredStateBytes {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	if response.StatusCode != http.StatusOK {
		return domain.WorkspaceServiceCommand{}, fmt.Errorf("Control Plane desired-state lookup returned HTTP %d", response.StatusCode)
	}
	var current domain.WorkspaceServiceCommand
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&current) != nil || decoder.Decode(new(any)) != io.EOF || !validWorkspaceServiceCommand(current) ||
		current.TenantID != trigger.TenantID || current.WorkspaceID != trigger.WorkspaceID ||
		current.ResourcePlaneID != trigger.ResourcePlaneID || current.ServiceID != trigger.ServiceID ||
		current.Revision < trigger.Revision || current.CatalogVersion != trigger.CatalogVersion || !bytes.Equal(current.Config, trigger.Config) ||
		(current.Revision == trigger.Revision && current.DesiredState != trigger.DesiredState) {
		return domain.WorkspaceServiceCommand{}, ErrWorkspaceServiceDelivery
	}
	return current, nil
}
