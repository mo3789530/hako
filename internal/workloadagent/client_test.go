package workloadagent

import (
	"context"
	"crypto/sha256"
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func response(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Hako-Agent-Protocol": []string{ProtocolVersion}}, Body: io.NopCloser(strings.NewReader(body))}
}

func testIdentity() workloadjournal.Command {
	return workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 2, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
}

func TestClientStartUsesVersionedScopedEndpointAndDoesNotLeakTokenIntoBody(t *testing.T) {
	id := testIdentity()
	var called bool
	client := NewClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		called = true
		if r.Method != http.MethodPut || r.URL.Host != "microvm_1.lambda-microvm.ap-northeast-1.on.aws" || r.URL.Path != "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1" {
			t.Fatalf("unexpected request target: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion || r.Header.Get("X-aws-proxy-auth") != "short-lived-secret" ||
			r.Header.Get("X-aws-proxy-port") != "8080" || r.Header.Get("Authorization") != "Bearer signed-agent-ticket" {
			t.Fatal("protocol or AWS/application authentication headers missing")
		}
		data, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "short-lived-secret") || strings.Contains(string(data), "signed-agent-ticket") || !strings.Contains(string(data), "source_digest") {
			t.Fatal("credential leaked into protocol payload or request missing metadata")
		}
		return response(http.StatusNoContent, ""), nil
	}))
	err := client.Start(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "short-lived-secret", "signed-agent-ticket", StartRequest{
		Identity: IdentityFromCommand(id), SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: time.Now().Add(time.Hour),
	})
	if err != nil || !called {
		t.Fatalf("Start() = %v, called=%v", err, called)
	}
}

func TestClientStatusRejectsWrongIdentityAndUnknownVersion(t *testing.T) {
	id := testIdentity()
	tests := []struct {
		name string
		resp *http.Response
	}{
		{name: "cross identity", resp: response(http.StatusOK, `{"identity":{"tenant_id":"other","workload_run_id":"run_1","attempt_id":"attempt_1","generation":2,"resource_plane_id":"rp_1","idempotency_token":"idem_1"},"state":"running"}`)},
		{name: "unsupported version", resp: func() *http.Response {
			r := response(http.StatusOK, `{"identity":{"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":2,"resource_plane_id":"rp_1","idempotency_token":"idem_1"},"state":"running"}`)
			r.Header.Set("X-Hako-Agent-Protocol", "2")
			return r
		}()},
		{name: "oversized response", resp: response(http.StatusOK, strings.Repeat("x", maxProtocolBody+1))},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) { return tt.resp, nil }))
			if _, err := client.Status(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token", "ticket", id); !errors.Is(err, ErrProtocolResponse) {
				t.Fatalf("Status() error = %v", err)
			}
		})
	}
}

func TestClientResultRequiresMatchingIdentityAndValidMetadata(t *testing.T) {
	id := testIdentity()
	body := `{"identity":{"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":2,"resource_plane_id":"rp_1","idempotency_token":"idem_1"},"result":{"schema_version":1,"tenant_id":"tenant_1","workload_run_id":"run_1","attempt_id":"attempt_1","generation":2,"resource_plane_id":"rp_1","commit_sha":"` + strings.Repeat("b", 40) + `","status":"succeeded","exit_code":0,"completed_at":"2026-10-12T00:00:00Z"}}`
	client := NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(http.StatusOK, body), nil }))
	result, err := client.Result(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token", "ticket", id)
	if err != nil || result.Status != domain.WorkloadSucceeded || result.AttemptID != id.AttemptID {
		t.Fatalf("Result() = %+v, %v", result, err)
	}

	bad := strings.Replace(body, `"attempt_id":"attempt_1","generation":2,"resource_plane_id":"rp_1","commit_sha"`, `"attempt_id":"another","generation":2,"resource_plane_id":"rp_1","commit_sha"`, 1)
	client = NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) { return response(http.StatusOK, bad), nil }))
	if _, err := client.Result(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token", "ticket", id); !errors.Is(err, ErrProtocolResponse) {
		t.Fatalf("cross-attempt result accepted: %v", err)
	}
}

func TestClientLogsValidatesTicketRouteAndResponseIntegrity(t *testing.T) {
	id := testIdentity()
	data := []byte("redacted test output\n")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	client := NewClient(roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.Path != "/v1/tenants/tenant_1/runs/run_1/attempts/attempt_1/generations/2/resource-planes/rp_1/logs" {
			t.Fatalf("unexpected logs request target: %s %s", r.Method, r.URL)
		}
		if r.Header.Get("X-Hako-Agent-Protocol") != ProtocolVersion || r.Header.Get("Authorization") != "Bearer logs-ticket" ||
			r.Header.Get("X-aws-proxy-auth") != "short-lived-secret" || r.Header.Get("X-aws-proxy-port") != "8080" {
			t.Fatal("log request is missing scoped authentication headers")
		}
		header := make(http.Header)
		header.Set("Content-Type", "text/plain; charset=utf-8")
		header.Set("X-Hako-Agent-Protocol", ProtocolVersion)
		header.Set("X-Hako-Log-SHA256", digest)
		return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: int64(len(data)), Body: io.NopCloser(strings.NewReader(string(data)))}, nil
	}))
	logs, gotDigest, err := client.Logs(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "short-lived-secret", "logs-ticket", id)
	if err != nil || string(logs) != string(data) || gotDigest != digest {
		t.Fatalf("Logs() = %q digest=%q err=%v", logs, gotDigest, err)
	}
}

func TestClientLogsRejectsUntrustedResponseMetadataAndContent(t *testing.T) {
	id := testIdentity()
	data := []byte("redacted test output\n")
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	tests := []struct {
		name          string
		contentType   string
		protocol      string
		body          []byte
		digest        string
		contentLength int64
	}{
		{name: "wrong media type", contentType: "application/octet-stream", protocol: ProtocolVersion, body: data, digest: digest, contentLength: int64(len(data))},
		{name: "unsupported protocol", contentType: "text/plain", protocol: "2", body: data, digest: digest, contentLength: int64(len(data))},
		{name: "digest mismatch", contentType: "text/plain", protocol: ProtocolVersion, body: data, digest: strings.Repeat("a", 64), contentLength: int64(len(data))},
		{name: "content length mismatch", contentType: "text/plain", protocol: ProtocolVersion, body: data, digest: digest, contentLength: int64(len(data) + 1)},
		{name: "oversized body", contentType: "text/plain", protocol: ProtocolVersion, body: make([]byte, maxAgentLogBytes+1), digest: digest, contentLength: maxAgentLogBytes + 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			client := NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
				header := make(http.Header)
				header.Set("Content-Type", tc.contentType)
				header.Set("X-Hako-Agent-Protocol", tc.protocol)
				header.Set("X-Hako-Log-SHA256", tc.digest)
				return &http.Response{StatusCode: http.StatusOK, Header: header, ContentLength: tc.contentLength, Body: io.NopCloser(strings.NewReader(string(tc.body)))}, nil
			}))
			if _, _, err := client.Logs(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token", "ticket", id); !errors.Is(err, ErrProtocolResponse) {
				t.Fatalf("Logs() accepted invalid response: %v", err)
			}
		})
	}
}

func TestClientRejectsUnsafeEndpointsAndTokenHeaders(t *testing.T) {
	client := NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unsafe request reached transport")
		return nil, nil
	}))
	id := testIdentity()
	for _, tc := range []struct{ endpoint, token string }{
		{"http://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token"},
		{"https://attacker.example", "token"},
		{"https://microvm_1.lambda-microvm.ap-northeast-1.on.aws?redirect=1", "token"},
		{"https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token\r\nX-Evil: yes"},
	} {
		if _, err := client.Status(context.Background(), tc.endpoint, tc.token, "ticket", id); !errors.Is(err, ErrProtocolRequest) {
			t.Fatalf("unsafe endpoint/token accepted: endpoint=%q err=%v", tc.endpoint, err)
		}
	}
}

func TestClientDoesNotReturnTransportErrorText(t *testing.T) {
	client := NewClient(roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("upstream leaked credential secret-value")
	}))
	_, err := client.Status(context.Background(), "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", "token", "ticket", testIdentity())
	if !errors.Is(err, ErrAgentTransport) || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("transport error leaked unsafe detail: %v", err)
	}
}
