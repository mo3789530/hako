package workloadcontroller

import (
	"context"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type metadataRoundTripFunc func(*http.Request) (*http.Response, error)

func (f metadataRoundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return f(request)
}

func TestControlPlaneMetadataClientSignsBoundRequestAndValidatesResponse(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	key := []byte(strings.Repeat("k", 32))
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 2,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	var gotRequest *http.Request
	transport := metadataRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		gotRequest = request
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{
			"schema_version":3,"kind":"job","tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":2,
			"resource_plane_id":"rp_1","github_installation_id":71,"github_repository_id":81,"github_owner":"acme","github_repository":"api",
			"github_integration_id":"integration_1","github_config_revision":3,"github_credential_generation":5,
			"commit_sha":"` + strings.Repeat("a", 40) + `","ref":"refs/heads/main","trigger_event":"push","trigger_branch":"main","runtime_class":"standard","trust_level":"untrusted",
			"requested_at":"2026-10-12T12:00:00Z",
			"deadline":"2026-10-12T13:00:00Z"}`))}, nil
	})
	client, err := NewControlPlaneMetadataClient(ControlPlaneMetadataConfig{Endpoint: "https://control.hako.test", ResourcePlaneID: "rp_1",
		HMACKey: key, Transport: transport, Now: func() time.Time { return now }, Random: strings.NewReader(strings.Repeat("n", 16))})
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := client.Resolve(context.Background(), id)
	if err != nil || metadata.GitHubOwner != "acme" || metadata.GitHubIntegrationID != "integration_1" || metadata.GitHubCredentialGen != 5 || metadata.TriggerEvent != "push" || metadata.TriggerBranch != "main" {
		t.Fatalf("trusted metadata = %+v, err=%v", metadata, err)
	}
	if gotRequest == nil || gotRequest.Method != http.MethodGet || gotRequest.URL.Path != "/v1/internal/resource-planes/rp_1/tenants/tenant_1/workloads/run_1/attempts/attempt_1/generations/2/specification" ||
		gotRequest.URL.RawQuery != "" || gotRequest.Body != nil || gotRequest.Header.Get("X-Hako-Timestamp") != "1791806400" ||
		gotRequest.Header.Get("X-Hako-Request-ID") != "req_"+hex.EncodeToString([]byte(strings.Repeat("n", 16))) {
		t.Fatalf("unexpected signed metadata request: %+v", gotRequest)
	}
	if signature := gotRequest.Header.Get("X-Hako-Signature"); signature != metadataSignature(key, http.MethodGet, gotRequest.URL.EscapedPath(), "1791806400", gotRequest.Header.Get("X-Hako-Request-ID")) {
		t.Fatalf("request signature = %q", signature)
	}
	if client.client.Timeout <= 0 || client.client.CheckRedirect == nil {
		t.Fatal("metadata client does not enforce timeout and redirect refusal")
	}
}

func TestControlPlaneMetadataClientRejectsInvalidOrForeignMetadata(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	key := []byte(strings.Repeat("k", 32))
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "not found", status: http.StatusNotFound, body: `{}`},
		{name: "foreign attempt", status: http.StatusOK, body: `{"schema_version":1,"tenant_id":"tenant_other"}`},
		{name: "unknown field", status: http.StatusOK, body: `{"schema_version":1,"tenant_id":"tenant_1","unexpected":true}`},
		{name: "oversized response", status: http.StatusOK, body: strings.Repeat("x", maxMetadataResponse+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := metadataRoundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(tc.body))}, nil
			})
			client, err := NewControlPlaneMetadataClient(ControlPlaneMetadataConfig{Endpoint: "https://control.hako.test", ResourcePlaneID: "rp_1",
				HMACKey: key, Transport: transport, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := client.Resolve(context.Background(), id); !errors.Is(err, ErrExecutionMetadata) {
				t.Fatalf("invalid metadata response error = %v", err)
			}
		})
	}
	client, err := NewControlPlaneMetadataClient(ControlPlaneMetadataConfig{Endpoint: "https://control.hako.test", ResourcePlaneID: "rp_1", HMACKey: key})
	if err != nil {
		t.Fatal(err)
	}
	foreign := id
	foreign.ResourcePlaneID = "rp_2"
	if _, err := client.Resolve(context.Background(), foreign); !errors.Is(err, ErrExecutionMetadata) {
		t.Fatal("cross-Resource-Plane command was accepted")
	}
}

func TestNewControlPlaneMetadataClientRequiresHTTPSAndDedicatedKey(t *testing.T) {
	for _, config := range []ControlPlaneMetadataConfig{
		{Endpoint: "http://control.hako.test", ResourcePlaneID: "rp_1", HMACKey: []byte(strings.Repeat("k", 32))},
		{Endpoint: "https://control.hako.test/path", ResourcePlaneID: "rp_1", HMACKey: []byte(strings.Repeat("k", 32))},
		{Endpoint: "https://control.hako.test", ResourcePlaneID: "rp_1", HMACKey: []byte("short")},
	} {
		if _, err := NewControlPlaneMetadataClient(config); !errors.Is(err, ErrExecutionMetadata) {
			t.Fatalf("invalid metadata client configuration accepted: %+v err=%v", config, err)
		}
	}
}

func TestValidExecutionMetadataRequiresImmutableRunInterval(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	valid := ControlPlaneExecutionMetadata{SchemaVersion: 1, TenantID: domain.TenantID("tenant_1"), WorkloadRunID: domain.WorkloadRunID("run_1"),
		AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: domain.ResourcePlaneID("rp_1"), GitHubInstallationID: 1,
		GitHubRepositoryID: 2, GitHubOwner: "acme", GitHubRepository: "api", CommitSHA: strings.Repeat("a", 40),
		RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, RequestedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	if !validExecutionMetadata(valid, now) {
		t.Fatal("valid immutable Run interval was rejected")
	}
	agent := valid
	agent.SchemaVersion, agent.Kind, agent.TriggerEvent, agent.TriggerBranch = workloadSpecSchemaV3, domain.WorkloadAgent, "issue_comment", "main"
	if !validExecutionMetadata(agent, now) {
		t.Fatal("valid Issue Agent metadata was rejected")
	}
	for _, mutate := range []func(*ControlPlaneExecutionMetadata){
		func(m *ControlPlaneExecutionMetadata) { m.TriggerEvent = "push" },
		func(m *ControlPlaneExecutionMetadata) { m.Kind = domain.WorkloadPreview },
	} {
		candidate := agent
		mutate(&candidate)
		if validExecutionMetadata(candidate, now) {
			t.Fatalf("invalid Agent execution metadata was accepted: %+v", candidate)
		}
	}
	for _, mutate := range []func(*ControlPlaneExecutionMetadata){
		func(m *ControlPlaneExecutionMetadata) { m.RequestedAt = time.Time{} },
		func(m *ControlPlaneExecutionMetadata) { m.RequestedAt = now.Add(time.Minute) },
		func(m *ControlPlaneExecutionMetadata) { m.Deadline = m.RequestedAt },
		func(m *ControlPlaneExecutionMetadata) { m.Deadline = m.RequestedAt.Add(24*time.Hour + time.Second) },
	} {
		candidate := valid
		mutate(&candidate)
		if validExecutionMetadata(candidate, now) {
			t.Fatalf("invalid immutable Run interval was accepted: %+v", candidate)
		}
	}
}
