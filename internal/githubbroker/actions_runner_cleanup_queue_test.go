package githubbroker

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestActionsRunnerCleanupQueueValidatesBoundedRequests(t *testing.T) {
	lease := &cleanupLeaseIdentity{IntegrationID: "integration_cleanup", WorkflowJobID: 123, RunAttempt: 1,
		ClaimID: "cleanup-claim-1", AttemptCount: 1, LeaseExpires: time.Now().Add(time.Minute)}
	for _, test := range []struct {
		name, action string
		input        cleanupQueueRequest
		valid        bool
	}{
		{"claim", "claim", cleanupQueueRequest{ClaimID: "cleanup-claim-1", LeaseSeconds: 60}, true},
		{"short lease", "claim", cleanupQueueRequest{ClaimID: "cleanup-claim-1", LeaseSeconds: 4}, false},
		{"long lease", "claim", cleanupQueueRequest{ClaimID: "cleanup-claim-1", LeaseSeconds: 601}, false},
		{"ambiguous claim", "claim", cleanupQueueRequest{ClaimID: "cleanup-claim-1", LeaseSeconds: 60, Lease: lease}, false},
		{"complete", "complete", cleanupQueueRequest{Lease: lease}, true},
		{"missing lease", "complete", cleanupQueueRequest{}, false},
		{"ambiguous complete", "complete", cleanupQueueRequest{Lease: lease, ErrorCode: "provider_unavailable"}, false},
		{"retry", "retry", cleanupQueueRequest{Lease: lease, RetryAfterSeconds: 15, ErrorCode: "microvm_terminate_failed"}, true},
		{"unbounded retry", "retry", cleanupQueueRequest{Lease: lease, RetryAfterSeconds: 901, ErrorCode: "microvm_terminate_failed"}, false},
		{"private diagnostic", "retry", cleanupQueueRequest{Lease: lease, RetryAfterSeconds: 15, ErrorCode: "sensitive provider output"}, false},
		{"unknown action", "unknown", cleanupQueueRequest{Lease: lease}, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validCleanupQueueRequest(test.action, test.input); got != test.valid {
				t.Fatalf("valid=%v want=%v", got, test.valid)
			}
		})
	}
}

type cleanupQueueTransport func(*http.Request) (*http.Response, error)

func (f cleanupQueueTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestActionsRunnerCleanupQueueClientRejectsUnsafeResponses(t *testing.T) {
	for _, test := range []struct {
		name    string
		body    string
		noStore bool
	}{
		{"missing lease", `{"applied":true}`, true},
		{"missing outcome", `{}`, true},
		{"null response", `null`, true},
		{"unknown field", `{"applied":false,"unknown":true}`, true},
		{"trailing value", `{"applied":false}{}`, true},
		{"cacheable response", `{"applied":false}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			client, err := NewHTTPActionsRunnerCleanupQueueClient("https://broker.invalid", "rp-tokyo-01", &http.Client{Transport: cleanupQueueTransport(func(r *http.Request) (*http.Response, error) {
				if r.URL.Path != cleanupQueuePath+"claim" {
					t.Errorf("unexpected path %s", r.URL.Path)
				}
				rec := httptest.NewRecorder()
				if test.noStore {
					rec.Header().Set("Cache-Control", "no-store")
				}
				_, _ = rec.Write([]byte(test.body))
				return rec.Result(), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			if _, _, err := client.Claim(context.Background(), "rp-tokyo-01", "cleanup-claim-1", time.Now(), time.Minute); err == nil {
				t.Fatal("unsafe broker response accepted")
			}
		})
	}
}
