//go:build linux

package workspaceagent

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestWorkspaceServiceSupervisorFenceSurvivesAgentChildRestart(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity := WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"}
	directory := filepath.Join(t.TempDir(), "fences")
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	newGuard := func() *WorkspaceServiceSupervisorGuard {
		t.Helper()
		guard, err := NewWorkspaceServiceSupervisorGuard(identity, map[string]ed25519.PublicKey{"rp_key": public}, directory,
			func() time.Time { return now })
		if err != nil {
			t.Fatal(err)
		}
		return guard
	}
	issue := func(command domain.WorkspaceServiceCommand) string {
		t.Helper()
		token, _, err := (&ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private, TTL: time.Minute,
			Now: func() time.Time { return now }}).Issue(command, identity.MicroVMID)
		if err != nil {
			t.Fatal(err)
		}
		return token
	}
	request := func(guard *WorkspaceServiceSupervisorGuard, command domain.WorkspaceServiceCommand, token string) (int, error) {
		t.Helper()
		body, err := json.Marshal(command)
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/services/reconcile", io.NopCloser(bytes.NewReader(body)))
		req.ContentLength = int64(len(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+token)
		return guard.Authorize(req)
	}

	first := serviceCommand(t, "redis", 1, "running")
	if status, err := request(newGuard(), first, issue(first)); err != nil || status != 0 {
		t.Fatalf("authorize initial revision: status=%d err=%v", status, err)
	}
	second := serviceCommand(t, "redis", 2, "stopped")
	if status, err := request(newGuard(), second, issue(second)); err != nil || status != 0 {
		t.Fatalf("authorize latest revision: status=%d err=%v", status, err)
	}
	statePath := filepath.Join(directory, "ws_1.json")
	if info, err := os.Stat(statePath); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("fence state permissions = %v, err=%v; want 0600", info, err)
	}

	// A new guard models the privileged init process re-launching the Agent
	// child after a crash. A stale start and an already-used ticket stay fenced.
	restarted := newGuard()
	if status, err := request(restarted, first, issue(first)); err == nil || status != http.StatusConflict {
		t.Fatalf("stale start after child restart = status %d err=%v", status, err)
	}
	latestToken := issue(second)
	if status, err := request(restarted, second, latestToken); err != nil || status != 0 {
		t.Fatalf("same-revision retry with a fresh capability: status=%d err=%v", status, err)
	}
	if status, err := request(newGuard(), second, latestToken); err == nil || status != http.StatusUnauthorized {
		t.Fatalf("replayed capability after child restart = status %d err=%v", status, err)
	}
}

func TestWorkspaceServiceSupervisorFenceResetsForNewMicroVMIdentity(t *testing.T) {
	public, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "fences")
	first := WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"}
	guard, err := NewWorkspaceServiceSupervisorGuard(first, map[string]ed25519.PublicKey{"rp_key": public}, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	guard.state.Services["svc_1"] = workspaceServiceFenceEntry{Revision: 9, Digest: strings.Repeat("a", 64)}
	if err := writeWorkspaceServiceFence(guard.path, guard.state); err != nil {
		t.Fatal(err)
	}
	second := first
	second.MicroVMID = "microvm_2"
	newGuard, err := NewWorkspaceServiceSupervisorGuard(second, map[string]ed25519.PublicKey{"rp_key": public}, directory, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(newGuard.state.Services) != 0 || len(newGuard.state.Capabilities) != 0 {
		t.Fatalf("new MicroVM inherited old runtime fence: %+v", newGuard.state)
	}
}
