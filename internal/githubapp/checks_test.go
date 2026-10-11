package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func validCheckRequest() CheckRunRequest {
	started := time.Date(2026, 10, 11, 1, 2, 3, 0, time.UTC)
	return CheckRunRequest{
		Name: "hako/test", ExternalID: "run_123", HeadSHA: strings.Repeat("a", 40),
		Status: CheckInProgress, StartedAt: started,
		Output: CheckOutput{Title: "Tests running", Summary: "Unit tests are in progress."},
	}
}

func checkResponse(request CheckRunRequest, id int64) CheckRun {
	return CheckRun{ID: id, Name: request.Name, ExternalID: request.ExternalID, HeadSHA: request.HeadSHA, Status: request.Status, Conclusion: request.Conclusion}
}

func checkToken(now time.Time) InstallationToken {
	return InstallationToken{value: "synthetic-check-token", expiresAt: now.Add(10 * time.Minute)}
}

func newTestChecksClient(t *testing.T, handler http.Handler) (*ChecksClient, time.Time) {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := NewChecksClient(server.Client())
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	client.apiURL = server.URL
	client.now = func() time.Time { return now }
	return client, now
}

func TestEnsureCheckRunCreatesThenUpdatesExactRun(t *testing.T) {
	request := validCheckRequest()
	var creates, updates int
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer synthetic-check-token" {
			t.Error("request did not use the scoped installation token")
		}
		if r.Method == http.MethodGet {
			if r.URL.Path != "/repos/acme/api/check-runs" {
				t.Errorf("unexpected lookup endpoint: %s", r.URL.String())
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		var body CheckRunRequest
		if json.NewDecoder(r.Body).Decode(&body) != nil || body.ExternalID != request.ExternalID || body.HeadSHA != request.HeadSHA {
			t.Error("request body did not retain Run identity")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			if r.URL.Path != "/repos/acme/api/check-runs" {
				t.Errorf("unexpected create endpoint: %s", r.URL.String())
			}
			creates++
			_ = json.NewEncoder(w).Encode(checkResponse(body, 123))
			return
		}
		if r.Method == http.MethodPatch && r.URL.Path == "/repos/acme/api/check-runs/123" {
			updates++
			_ = json.NewEncoder(w).Encode(checkResponse(body, 123))
			return
		}
		t.Errorf("unexpected method/path: %s %s", r.Method, r.URL.Path)
	}))
	repository := RepositoryAccess{Owner: "acme", Name: "api"}
	created, err := client.EnsureCheckRun(context.Background(), checkToken(now), repository, 0, request)
	if err != nil || created.ID != 123 {
		t.Fatalf("create Check failed: %+v, %v", created, err)
	}
	request.Status = CheckCompleted
	request.Conclusion = "success"
	completed := request.StartedAt.Add(time.Minute)
	request.CompletedAt = &completed
	request.Output = CheckOutput{Title: "Tests passed", Summary: "All tests passed."}
	updated, err := client.EnsureCheckRun(context.Background(), checkToken(now), repository, created.ID, request)
	if err != nil || updated.ID != created.ID || updated.Status != CheckCompleted || updated.Conclusion != "success" {
		t.Fatalf("update Check failed: %+v, %v", updated, err)
	}
	if creates != 1 || updates != 1 {
		t.Fatalf("expected one create and one update, got creates=%d updates=%d", creates, updates)
	}
}

func TestEnsureCheckRunRecoversCreateBeforePersistence(t *testing.T) {
	request := validCheckRequest()
	var updated bool
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodGet:
			if r.URL.Query().Get("check_name") != request.Name || r.URL.Query().Get("head_sha") != request.HeadSHA {
				t.Error("recovery lookup is not scoped to Check name and immutable SHA")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []CheckRun{checkResponse(request, 987)}})
		case http.MethodPatch:
			updated = r.URL.Path == "/repos/acme/api/check-runs/987"
			_ = json.NewEncoder(w).Encode(checkResponse(request, 987))
		default:
			t.Errorf("unexpected create during recovery: %s", r.Method)
		}
	}))
	result, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme", Name: "api"}, 0, request)
	if err != nil || result.ID != 987 || !updated {
		t.Fatalf("did not recover and update existing Check: %+v, %v", result, err)
	}
}

func TestEnsureCheckRunRefusesCommitMismatch(t *testing.T) {
	request := validCheckRequest()
	wrong := checkResponse(request, 123)
	wrong.HeadSHA = strings.Repeat("b", 40)
	var mutations int
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			mutations++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": []CheckRun{wrong}})
	}))
	_, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme", Name: "api"}, 0, request)
	if err == nil || !strings.Contains(err.Error(), "ambiguous") || mutations != 0 {
		t.Fatalf("commit mismatch was not refused before mutation: %v (mutations=%d)", err, mutations)
	}
}

func TestEnsureCheckRunSafeRateLimitErrorAndRetryAt(t *testing.T) {
	request := validCheckRequest()
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = fmt.Fprint(w, "private upstream response containing a secret")
	}))
	_, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme", Name: "api"}, 123, request)
	var safe *CredentialError
	if !errors.As(err, &safe) || !safe.Retryable || !safe.RetryAt.Equal(now.Add(30*time.Second)) {
		t.Fatalf("rate limit was not classified for durable retry: %#v", err)
	}
	if strings.Contains(err.Error(), "private upstream") || strings.Contains(err.Error(), "secret") {
		t.Fatal("GitHub response body leaked in safe error")
	}
}

func TestEnsureCheckRunRefusesMismatchedGitHubResponse(t *testing.T) {
	request := validCheckRequest()
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = w.Write([]byte(`{"check_runs":[]}`))
			return
		}
		wrong := checkResponse(request, 456)
		wrong.HeadSHA = strings.Repeat("b", 40)
		_ = json.NewEncoder(w).Encode(wrong)
	}))
	_, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme", Name: "api"}, 0, request)
	if err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("mismatched Check API response was accepted: %v", err)
	}
}

func TestEnsureCheckRunPermissionFailureIsNotRetried(t *testing.T) {
	request := validCheckRequest()
	var calls int
	client, now := newTestChecksClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusForbidden)
		_, _ = fmt.Fprint(w, "permission details must not escape")
	}))
	_, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme", Name: "api"}, 123, request)
	var safe *CredentialError
	if !errors.As(err, &safe) || safe.Retryable || calls != 1 || strings.Contains(err.Error(), "permission details") {
		t.Fatalf("permission failure was retried or leaked: calls=%d err=%v", calls, err)
	}
}

func TestValidateCheckRequestBoundsAnnotationsAndPayload(t *testing.T) {
	request := validCheckRequest()
	request.Output.Annotations = []CheckAnnotation{{Path: "internal/api/handler.go", StartLine: 2, EndLine: 2, AnnotationLevel: "warning", Message: "Check this result."}}
	if err := validateCheckRequest(request); err != nil {
		t.Fatalf("valid annotation rejected: %v", err)
	}
	for _, path := range []string{"../secret", "a/../../secret", "/etc/passwd", "a//b"} {
		invalid := request
		invalid.Output.Annotations = []CheckAnnotation{{Path: path, StartLine: 1, EndLine: 1, AnnotationLevel: "failure", Message: "bad"}}
		if err := validateCheckRequest(invalid); err == nil {
			t.Errorf("unsafe annotation path accepted: %q", path)
		}
	}
	tooMany := request
	tooMany.Output.Annotations = make([]CheckAnnotation, maxCheckAnnotations+1)
	if err := validateCheckRequest(tooMany); err == nil {
		t.Fatal("annotation count limit was not enforced")
	}
	tooLarge := request
	tooLarge.Output.Summary = strings.Repeat("x", maxCheckPayloadBytes+1)
	if err := validateCheckRequest(tooLarge); err == nil {
		t.Fatal("summary byte limit was not enforced")
	}
}

func TestEnsureCheckRunRejectsExpiredTokenAndInvalidRepository(t *testing.T) {
	client, now := newTestChecksClient(t, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("invalid request must not reach GitHub")
	}))
	request := validCheckRequest()
	if _, err := client.EnsureCheckRun(context.Background(), checkToken(now.Add(-time.Hour)), RepositoryAccess{Owner: "acme", Name: "api"}, 1, request); err == nil {
		t.Fatal("expired Check token was accepted")
	}
	if _, err := client.EnsureCheckRun(context.Background(), checkToken(now), RepositoryAccess{Owner: "acme/other", Name: "api"}, 1, request); err == nil {
		t.Fatal("untrusted owner path was accepted")
	}
}
