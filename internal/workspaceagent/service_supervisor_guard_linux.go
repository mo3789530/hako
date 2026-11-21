//go:build linux

package workspaceagent

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

const defaultWorkspaceServiceFenceDirectory = "/run/hako/workspace-service-fences"

type workspaceServiceFenceIdentity struct {
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkspaceID     domain.WorkspaceID     `json:"workspace_id"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
	MicroVMID       string                 `json:"microvm_id"`
}

type workspaceServiceFenceEntry struct {
	Revision int64  `json:"revision"`
	Digest   string `json:"digest"`
}

type workspaceServiceFenceState struct {
	Version      int                                             `json:"version"`
	Identity     workspaceServiceFenceIdentity                   `json:"identity"`
	Services     map[domain.ServiceID]workspaceServiceFenceEntry `json:"services"`
	Capabilities map[string]int64                                `json:"capabilities"`
}

// WorkspaceServiceSupervisorGuard is owned by the privileged image init
// process, not the Workspace UID. Its atomic state file survives an Agent
// child restart for the lifetime of the MicroVM and cannot be edited through
// the mounted Workspace filesystem.
type WorkspaceServiceSupervisorGuard struct {
	mu       sync.Mutex
	identity workspaceServiceFenceIdentity
	keys     map[string]ed25519.PublicKey
	path     string
	state    workspaceServiceFenceState
	now      func() time.Time
}

func NewWorkspaceServiceSupervisorGuard(identity WorkspaceServiceIdentity, trustedKeys map[string]ed25519.PublicKey, directory string, now func() time.Time) (*WorkspaceServiceSupervisorGuard, error) {
	if !agentIdentityPattern.MatchString(string(identity.TenantID)) || !agentIdentityPattern.MatchString(string(identity.WorkspaceID)) ||
		!agentIdentityPattern.MatchString(string(identity.ResourcePlaneID)) || !agentIdentityPattern.MatchString(identity.MicroVMID) ||
		len(trustedKeys) == 0 || len(trustedKeys) > 32 {
		return nil, errors.New("Workspace Service supervisor identity and keys are required")
	}
	directory = strings.TrimSpace(directory)
	if directory == "" {
		directory = defaultWorkspaceServiceFenceDirectory
	}
	if !filepath.IsAbs(directory) || directory == "/" || strings.Contains(directory, "..") {
		return nil, errors.New("Workspace Service fence directory is invalid")
	}
	keyCopy := make(map[string]ed25519.PublicKey, len(trustedKeys))
	for keyID, key := range trustedKeys {
		if !agentIdentityPattern.MatchString(keyID) || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("Workspace Service supervisor trusted key is invalid")
		}
		keyCopy[keyID] = append(ed25519.PublicKey(nil), key...)
	}
	if now == nil {
		now = time.Now
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, errors.New("prepare Workspace Service fence directory")
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("Workspace Service fence directory must be a private real directory")
	}
	identityValue := workspaceServiceFenceIdentity{TenantID: identity.TenantID, WorkspaceID: identity.WorkspaceID,
		ResourcePlaneID: identity.ResourcePlaneID, MicroVMID: identity.MicroVMID}
	path := filepath.Join(directory, string(identity.WorkspaceID)+".json")
	state := workspaceServiceFenceState{Version: 1, Identity: identityValue,
		Services: make(map[domain.ServiceID]workspaceServiceFenceEntry), Capabilities: make(map[string]int64)}
	if err := readWorkspaceServiceFence(path, &state); err != nil {
		return nil, err
	}
	if state.Identity != identityValue {
		state = workspaceServiceFenceState{Version: 1, Identity: identityValue,
			Services: make(map[domain.ServiceID]workspaceServiceFenceEntry), Capabilities: make(map[string]int64)}
	}
	return &WorkspaceServiceSupervisorGuard{identity: identityValue, keys: keyCopy, path: path, state: state, now: now}, nil
}

// Authorize verifies the guest capability and durably advances its replay and
// revision fences before the request can reach the unprivileged Agent child.
func (g *WorkspaceServiceSupervisorGuard) Authorize(r *http.Request) (int, error) {
	if g == nil || r == nil || r.URL == nil || r.Method != http.MethodPost || r.URL.RawQuery != "" || r.URL.ForceQuery ||
		r.URL.Path != "/v1/workspaces/"+string(g.identity.WorkspaceID)+"/services/reconcile" ||
		r.ContentLength < 0 || r.ContentLength > maxWorkspaceServiceCommandBytes || len(r.TransferEncoding) != 0 ||
		strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Type"))) != "application/json" {
		return http.StatusBadRequest, errors.New("invalid Workspace Service request")
	}
	token := bearerAgentTicket(r.Header.Get("Authorization"))
	if token == "" {
		return http.StatusUnauthorized, errors.New("invalid Workspace Service capability")
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWorkspaceServiceCommandBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxWorkspaceServiceCommandBytes {
		return http.StatusBadRequest, errors.New("invalid Workspace Service request")
	}
	var command domain.WorkspaceServiceCommand
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil || decoder.Decode(new(any)) != io.EOF {
		return http.StatusBadRequest, errors.New("invalid Workspace Service request")
	}
	if _, err := validatedServiceTemplate(command); err != nil {
		return http.StatusBadRequest, errors.New("invalid Workspace Service command")
	}
	expected := ServiceCapability{TenantID: g.identity.TenantID, WorkspaceID: g.identity.WorkspaceID,
		ResourcePlane: g.identity.ResourcePlaneID, MicroVMID: g.identity.MicroVMID,
		ServiceID: command.ServiceID, Revision: command.Revision, CredentialGeneration: serviceCommandCredentialGeneration(command), DesiredState: command.DesiredState,
		CatalogVersion: command.CatalogVersion}
	claims, err := VerifyServiceCapability(token, g.keys, expected, command, g.now())
	if err != nil {
		return http.StatusUnauthorized, errors.New("invalid Workspace Service capability")
	}
	digest, err := serviceCommandDigest(command)
	if err != nil {
		return http.StatusBadRequest, errors.New("invalid Workspace Service command")
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for id, expiry := range g.state.Capabilities {
		if !now.Before(time.Unix(expiry, 0)) {
			delete(g.state.Capabilities, id)
		}
	}
	if _, exists := g.state.Capabilities[claims.ID]; exists || len(g.state.Capabilities) >= maxAgentReplayTickets {
		return http.StatusUnauthorized, errors.New("Workspace Service capability replay rejected")
	}
	entry, existed := g.state.Services[command.ServiceID]
	previousEntry := entry
	if existed && (command.Revision < entry.Revision || (command.Revision == entry.Revision && entry.Digest != digestString(digest))) {
		return http.StatusConflict, errors.New("stale or conflicting Workspace Service revision")
	}
	if !existed || command.Revision > entry.Revision {
		entry = workspaceServiceFenceEntry{Revision: command.Revision, Digest: digestString(digest)}
		g.state.Services[command.ServiceID] = entry
	}
	g.state.Capabilities[claims.ID] = claims.ExpiresAt
	if err := writeWorkspaceServiceFence(g.path, g.state); err != nil {
		delete(g.state.Capabilities, claims.ID)
		if existed {
			g.state.Services[command.ServiceID] = previousEntry
		} else {
			delete(g.state.Services, command.ServiceID)
		}
		return http.StatusServiceUnavailable, errors.New("persist Workspace Service revision fence")
	}
	r.Body = io.NopCloser(bytes.NewReader(body))
	return 0, nil
}

func digestString(digest [32]byte) string {
	return fmt.Sprintf("%x", digest[:])
}

func readWorkspaceServiceFence(path string, state *workspaceServiceFenceState) error {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return errors.New("open Workspace Service fence state")
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return errors.New("Workspace Service fence state is invalid")
	}
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	if decoder.Decode(state) != nil || decoder.Decode(new(any)) != io.EOF || state.Version != 1 ||
		state.Services == nil || state.Capabilities == nil || len(state.Services) > 4096 || len(state.Capabilities) > maxAgentReplayTickets {
		return errors.New("Workspace Service fence state is invalid")
	}
	return nil
}

func writeWorkspaceServiceFence(path string, state workspaceServiceFenceState) error {
	data, err := json.Marshal(state)
	if err != nil || len(data) > 1<<20 {
		return errors.New("Workspace Service fence state exceeds limit")
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".service-fence-*")
	if err != nil {
		return errors.New("create Workspace Service fence state")
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0600); err != nil {
		_ = temp.Close()
		return errors.New("secure Workspace Service fence state")
	}
	if _, err := temp.Write(data); err != nil {
		_ = temp.Close()
		return errors.New("write Workspace Service fence state")
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return errors.New("sync Workspace Service fence state")
	}
	if err := temp.Close(); err != nil {
		return errors.New("close Workspace Service fence state")
	}
	if err := os.Rename(tempPath, path); err != nil {
		return errors.New("publish Workspace Service fence state")
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return errors.New("open Workspace Service fence directory")
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errors.New("sync Workspace Service fence directory")
	}
	return nil
}
