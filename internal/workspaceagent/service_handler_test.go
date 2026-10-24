package workspaceagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestWorkspaceServiceHandlerRequiresSignedOneUseCapability(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	engine := &serviceEngineFake{healthy: true}
	now := time.Date(2026, 10, 18, 12, 0, 0, 0, time.UTC)
	handler, err := NewWorkspaceServiceHandler(WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"},
		map[string][]byte{"rp_key": public}, engine, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "redis", 1, "running")
	issuer := &ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private, TTL: time.Minute, Now: func() time.Time { return now }}
	token, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	request := func(body domain.WorkspaceServiceCommand, bearer string) *httptest.ResponseRecorder {
		data, err := json.Marshal(struct {
			Command domain.WorkspaceServiceCommand `json:"command"`
		}{Command: body})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/services/reconcile", strings.NewReader(string(data)))
		req.Header.Set("Content-Type", "application/json")
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	response := request(command, token)
	var observation ServiceObservation
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &observation) != nil || observation.Revision != 1 || observation.ObservedRevision != 1 || observation.ObservedState != "healthy" || engine.ensureCalls != 1 {
		t.Fatalf("valid Service capability response = %d %s, engine calls=%d", response.Code, response.Body.String(), engine.ensureCalls)
	}
	if replay := request(command, token); replay.Code != http.StatusUnauthorized || engine.ensureCalls != 1 {
		t.Fatalf("replayed capability was not rejected: %d %s calls=%d", replay.Code, replay.Body.String(), engine.ensureCalls)
	}
	if missing := request(command, ""); missing.Code != http.StatusUnauthorized {
		t.Fatalf("missing capability status = %d", missing.Code)
	}
}

func TestWorkspaceServiceHandlerBindsCapabilityToConfigAndMicroVM(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	engine := &serviceEngineFake{healthy: true}
	handler, err := NewWorkspaceServiceHandler(WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"},
		map[string][]byte{"rp_key": public}, engine, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	issuer := &ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private, Now: func() time.Time { return now }}
	command := serviceCommand(t, "redis", 1, "running")
	wrongVMToken, _, err := issuer.Issue(command, "microvm_2")
	if err != nil {
		t.Fatal(err)
	}
	changed := command
	changed.DesiredState = "stopped"
	if response := serviceCapabilityRequest(t, handler, changed, wrongVMToken); response.Code != http.StatusUnauthorized {
		t.Fatalf("capability for another VM/configuration status = %d %s", response.Code, response.Body.String())
	}
	changedGeneration := command
	changedGeneration.CredentialGeneration = 2
	generationToken, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	if response := serviceCapabilityRequest(t, handler, changedGeneration, generationToken); response.Code != http.StatusUnauthorized {
		t.Fatalf("capability for another credential generation status = %d %s", response.Code, response.Body.String())
	}
	validToken, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	changedConfig := command
	changedConfig.Config = []byte(`{"changed":true}`)
	if response := serviceCapabilityRequest(t, handler, changedConfig, validToken); response.Code != http.StatusUnauthorized || engine.ensureCalls != 0 {
		t.Fatalf("modified config passed capability verification: %d %s calls=%d", response.Code, response.Body.String(), engine.ensureCalls)
	}
}

func TestWorkspaceServiceHandlerDoesNotExposeEngineError(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	engine := &serviceEngineFake{err: errors.New("secret-bearing container runtime error")}
	handler, err := NewWorkspaceServiceHandler(WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"},
		map[string][]byte{"rp_key": public}, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "redis", 1, "running")
	issuer := &ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private}
	token, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	response := serviceCapabilityRequest(t, handler, command, token)
	if response.Code != http.StatusServiceUnavailable || strings.Contains(response.Body.String(), "secret-bearing") || strings.Contains(response.Body.String(), "container runtime") {
		t.Fatalf("runtime error leaked through Service API: %d %s", response.Code, response.Body.String())
	}
}

func TestWorkspaceServiceHandlerDeliversGeneratedSecretWithoutReturningIt(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	engine := &serviceEngineFake{healthy: true}
	handler, err := NewWorkspaceServiceHandler(WorkspaceServiceIdentity{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", MicroVMID: "microvm_1"},
		map[string][]byte{"rp_key": public}, engine, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "postgres", 1, "running")
	secret := "0123456789012345678901234567890123456789012"
	issuer := &ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private}
	token, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(struct {
		Command domain.WorkspaceServiceCommand `json:"command"`
		Secrets map[string]string              `json:"secrets"`
	}{Command: command, Secrets: map[string]string{"POSTGRES_PASSWORD": secret}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/services/reconcile", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK || engine.secrets["POSTGRES_PASSWORD"] != secret || strings.Contains(response.Body.String(), secret) {
		t.Fatalf("generated secret handler response leaked or failed: status=%d body=%s engine=%v", response.Code, response.Body.String(), engine.secrets)
	}
}

func TestWorkspaceShellAgentRoutesServiceCommandsOnlyAfterRuntimeBinding(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	engine := &serviceEngineFake{healthy: true}
	agent := &WorkspaceShellAgent{keys: map[string]ed25519.PublicKey{"rp_key": public}, identity: AgentTicket{
		TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlane: "rp_1", MicroVMID: "microvm_1"}}
	command := serviceCommand(t, "redis", 1, "running")
	issuer := &ServiceCapabilityIssuer{KeyID: "rp_key", PrivateKey: private}
	token, _, err := issuer.Issue(command, "microvm_1")
	if err != nil {
		t.Fatal(err)
	}
	response := serviceCapabilityRequest(t, agent, command, token)
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unbound Agent Service endpoint status = %d %s", response.Code, response.Body.String())
	}
	if err := agent.BindWorkspaceServiceRuntime(engine); err != nil {
		t.Fatal(err)
	}
	response = serviceCapabilityRequest(t, agent, command, token)
	if response.Code != http.StatusOK || engine.ensureCalls != 1 {
		t.Fatalf("bound Agent Service endpoint = %d %s, ensure calls=%d", response.Code, response.Body.String(), engine.ensureCalls)
	}
	foreign := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_other/services/reconcile", strings.NewReader("{}"))
	foreignResponse := httptest.NewRecorder()
	agent.ServeHTTP(foreignResponse, foreign)
	if foreignResponse.Code != http.StatusNotFound || engine.ensureCalls != 1 {
		t.Fatalf("foreign Workspace route was not rejected: %d calls=%d", foreignResponse.Code, engine.ensureCalls)
	}
}

func serviceCapabilityRequest(t *testing.T, handler http.Handler, command domain.WorkspaceServiceCommand, token string) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(struct {
		Command domain.WorkspaceServiceCommand `json:"command"`
	}{Command: command})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/workspaces/ws_1/services/reconcile", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer "+token)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}
