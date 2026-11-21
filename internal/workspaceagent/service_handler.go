package workspaceagent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const maxWorkspaceServiceCommandBytes = 64 << 10

type WorkspaceServiceIdentity struct {
	TenantID        domain.TenantID
	WorkspaceID     domain.WorkspaceID
	ResourcePlaneID domain.ResourcePlaneID
	MicroVMID       string
}

type WorkspaceServiceHandler struct {
	identity   WorkspaceServiceIdentity
	keys       map[string][]byte
	reconciler *ServiceReconciler
	now        func() time.Time
	replay     ServiceCapabilityReplayGuard
}

func NewWorkspaceServiceHandler(identity WorkspaceServiceIdentity, keys map[string][]byte, engine ServiceEngine, now func() time.Time) (*WorkspaceServiceHandler, error) {
	if !agentIdentityPattern.MatchString(string(identity.TenantID)) || !agentIdentityPattern.MatchString(string(identity.WorkspaceID)) ||
		!agentIdentityPattern.MatchString(string(identity.ResourcePlaneID)) || !agentIdentityPattern.MatchString(identity.MicroVMID) || len(keys) == 0 || len(keys) > 32 {
		return nil, errors.New("Workspace Service Agent identity and trusted keys are required")
	}
	reconciler, err := NewServiceReconciler(identity.TenantID, identity.WorkspaceID, identity.ResourcePlaneID, engine)
	if err != nil {
		return nil, err
	}
	keyCopy := make(map[string][]byte, len(keys))
	for keyID, raw := range keys {
		if !agentIdentityPattern.MatchString(keyID) || len(raw) != 32 {
			return nil, errors.New("Workspace Service Agent trusted keys are invalid")
		}
		keyCopy[keyID] = append([]byte(nil), raw...)
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &WorkspaceServiceHandler{identity: identity, keys: keyCopy, reconciler: reconciler, now: now}, nil
}

func (h *WorkspaceServiceHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if h == nil || r == nil || r.URL == nil || r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery ||
		r.URL.Path != "/v1/workspaces/"+string(h.identity.WorkspaceID)+"/services/reconcile" ||
		r.ContentLength < 0 || r.ContentLength > maxWorkspaceServiceCommandBytes || len(r.TransferEncoding) != 0 {
		http.Error(w, "invalid Workspace Service request", http.StatusBadRequest)
		return
	}
	if contentType := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Type"))); contentType != "application/json" {
		http.Error(w, "invalid Workspace Service request", http.StatusBadRequest)
		return
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	if token == "" {
		http.Error(w, "invalid Workspace Service capability", http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWorkspaceServiceCommandBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxWorkspaceServiceCommandBytes {
		http.Error(w, "invalid Workspace Service request", http.StatusBadRequest)
		return
	}
	var request struct {
		Command domain.WorkspaceServiceCommand `json:"command"`
		Secrets map[string]string              `json:"secrets,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "invalid Workspace Service request", http.StatusBadRequest)
		return
	}
	command := request.Command
	expected := ServiceCapability{TenantID: h.identity.TenantID, WorkspaceID: h.identity.WorkspaceID,
		ResourcePlane: h.identity.ResourcePlaneID, MicroVMID: h.identity.MicroVMID, ServiceID: command.ServiceID,
		Revision: command.Revision, CredentialGeneration: serviceCommandCredentialGeneration(command),
		DesiredState: command.DesiredState, CatalogVersion: command.CatalogVersion}
	verified, err := VerifyServiceCapability(token, byteKeyMap(h.keys), expected, command, h.now())
	if err != nil || verified.TenantID != h.identity.TenantID || verified.WorkspaceID != h.identity.WorkspaceID || verified.ResourcePlane != h.identity.ResourcePlaneID {
		http.Error(w, "invalid Workspace Service capability", http.StatusUnauthorized)
		return
	}
	replayErr := h.replay.Consume(verified, h.now())
	if replayErr != nil {
		http.Error(w, "invalid Workspace Service capability", http.StatusUnauthorized)
		return
	}
	observation, err := h.reconciler.ReconcileWithSecrets(r.Context(), command, request.Secrets)
	if err != nil {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(observation)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(observation); err != nil {
		return
	}
}

func byteKeyMap(keys map[string][]byte) map[string]ed25519.PublicKey {
	result := make(map[string]ed25519.PublicKey, len(keys))
	for id, key := range keys {
		result[id] = ed25519.PublicKey(key)
	}
	return result
}

var _ http.Handler = (*WorkspaceServiceHandler)(nil)
