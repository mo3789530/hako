package githubwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const testSecret = "test-webhook-secret"
const testDeliveryID = "a1b2c3d4-e5f6-4789-8abc-def012345678"
const validPushWebhookPayload = `{"ref":"refs/heads/main","after":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}}}`
const validPullRequestWebhookPayload = `{"action":"opened","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}},"pull_request":{"number":17,"state":"open","head":{"ref":"feature/add","sha":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},"base":{"ref":"main"}}}`

type fakeInbox struct {
	inserted bool
	err      error
	calls    int
}

func (f *fakeInbox) Record(context.Context, VerifiedDelivery, time.Time) (bool, error) {
	f.calls++
	return f.inserted, f.err
}

func signedHeaders(body []byte, event, deliveryID string) http.Header {
	mac := hmac.New(sha256.New, []byte(testSecret))
	_, _ = mac.Write(body)
	headers := make(http.Header)
	headers.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	headers.Set("X-GitHub-Event", event)
	headers.Set("X-GitHub-Delivery", deliveryID)
	return headers
}

func TestVerifyUsesRawBodyAndReturnsAllowlistedEvent(t *testing.T) {
	body := []byte(`{"action":"opened","repository":{"id":42}}`)
	got, err := Verify(signedHeaders(body, "pull_request", testDeliveryID), body, []byte(testSecret), DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	if got.DeliveryID != testDeliveryID || got.Event != "pull_request" || got.Action != "opened" || string(got.Payload) != string(body) {
		t.Fatalf("verified delivery = %+v", got)
	}

	// Whitespace changes the raw bytes and therefore must invalidate the MAC,
	// even though the JSON value would decode to the same object.
	if _, err := Verify(signedHeaders(body, "pull_request", testDeliveryID), append(body, ' '), []byte(testSecret), DefaultMaxBodyBytes); err == nil {
		t.Fatal("signature for original bytes must not authenticate modified raw body")
	}
}

func TestVerifyRejectsInvalidSignatureHeadersPayloadAndEvents(t *testing.T) {
	validBody := []byte(`{"action":"queued"}`)
	tests := []struct {
		name   string
		body   []byte
		event  string
		change func(http.Header)
	}{
		{name: "unknown event", body: []byte(`{}`), event: "issues"},
		{name: "unsupported action", body: []byte(`{"action":"edited"}`), event: "pull_request"},
		{name: "invalid json", body: []byte(`not-json`), event: "workflow_job"},
		{name: "trailing json", body: []byte(`{} {}`), event: "push"},
		{name: "invalid delivery id", body: []byte(`{}`), event: "push", change: func(h http.Header) { h.Set("X-GitHub-Delivery", "delivery-1") }},
		{name: "missing signature", body: validBody, event: "workflow_job", change: func(h http.Header) { h.Del("X-Hub-Signature-256") }},
		{name: "malformed signature", body: validBody, event: "workflow_job", change: func(h http.Header) { h.Set("X-Hub-Signature-256", "sha256=xyz") }},
		{name: "empty secret", body: validBody, event: "workflow_job", change: func(http.Header) {}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			body := test.body
			headers := signedHeaders(body, test.event, testDeliveryID)
			if test.change != nil {
				test.change(headers)
			}
			secret := []byte(testSecret)
			if test.name == "empty secret" {
				secret = nil
			}
			if _, err := Verify(headers, body, secret, DefaultMaxBodyBytes); err == nil {
				t.Fatal("Verify() unexpectedly accepted invalid delivery")
			}
		})
	}
}

func TestVerifyRejectsOversizedBodyAndInvalidActionType(t *testing.T) {
	oversized := []byte(`{"action":"opened"}`)
	if _, err := Verify(signedHeaders(oversized, "pull_request", testDeliveryID), oversized, []byte(testSecret), int64(len(oversized)-1)); err == nil {
		t.Fatal("oversized payload was accepted")
	}
	body := []byte(`{"action":42}`)
	if _, err := Verify(signedHeaders(body, "pull_request", testDeliveryID), body, []byte(testSecret), 100); err == nil {
		t.Fatal("non-string action was accepted")
	}
}

func TestReadBounded(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("payload"))
	body, err := ReadBounded(request, 7)
	if err != nil || string(body) != "payload" {
		t.Fatalf("ReadBounded() = %q, %v", body, err)
	}
	request = httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader("too large"))
	if _, err := ReadBounded(request, 3); err == nil {
		t.Fatal("oversized body was accepted")
	}
}

func TestNormalizeBuildsVersionedHakoEvents(t *testing.T) {
	const sha = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tests := []struct {
		name     string
		event    string
		payload  string
		wantType string
		check    func(RepositoryEvent) bool
	}{
		{
			name: "push", event: "push",
			payload:  `{"ref":"refs/heads/main","after":"` + sha + `","installation":{"id":12},"repository":{"id":42,"name":"api","default_branch":"main","owner":{"login":"acme"}},"sender":{"login":"dev"}}`,
			wantType: "repository.push",
			check: func(got RepositoryEvent) bool {
				return got.Repository != nil && got.Repository.GitHubID == 42 && got.Ref == "refs/heads/main" && got.CommitSHA == sha && got.Actor == "dev"
			},
		},
		{
			name: "pull request", event: "pull_request",
			payload:  `{"action":"opened","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}},"pull_request":{"number":9,"state":"open","head":{"ref":"feature/add","sha":"` + sha + `"},"base":{"ref":"main"}}}`,
			wantType: "repository.pull_request.opened",
			check: func(got RepositoryEvent) bool {
				return got.PullRequest != nil && got.PullRequest.Number == 9 && got.CommitSHA == sha
			},
		},
		{
			name: "workflow job", event: "workflow_job",
			payload:  `{"action":"queued","installation":{"id":12},"repository":{"id":42,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":5,"run_id":4,"name":"test","head_branch":"main","head_sha":"` + sha + `","status":"queued"}}`,
			wantType: "repository.workflow_job.queued",
			check: func(got RepositoryEvent) bool {
				return got.WorkflowJob != nil && got.WorkflowJob.ID == 5 && got.WorkflowJob.RunID == 4
			},
		},
		{
			name: "installation", event: "installation",
			payload:  `{"action":"created","installation":{"id":12,"account":{"login":"acme"}}}`,
			wantType: "github.installation.created",
			check:    func(got RepositoryEvent) bool { return got.InstallationID == 12 && got.Actor == "acme" },
		},
		{
			name: "installation repositories", event: "installation_repositories",
			payload:  `{"action":"added","installation":{"id":12},"repositories_added":[{"id":42,"name":"api","owner":{"login":"acme"}}]}`,
			wantType: "github.installation_repositories.added",
			check: func(got RepositoryEvent) bool {
				return got.InstallationID == 12 && len(got.RepositoriesAdded) == 1 && got.RepositoriesAdded[0].GitHubID == 42
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			action := ""
			if test.name == "pull request" || test.name == "workflow job" || test.name == "installation" || test.name == "installation repositories" {
				action = map[string]string{"pull request": "opened", "workflow job": "queued", "installation": "created", "installation repositories": "added"}[test.name]
			}
			got, err := Normalize(VerifiedDelivery{DeliveryID: testDeliveryID, Event: test.event, Action: action, Payload: []byte(test.payload)})
			if err != nil {
				t.Fatal(err)
			}
			if got.SchemaVersion != RepositoryEventSchemaVersion || got.DeliveryID != testDeliveryID || got.Type != test.wantType || !test.check(got) {
				t.Fatalf("normalized event = %+v", got)
			}
		})
	}
}

func TestNormalizeRejectsMissingOrMismatchedRequiredFields(t *testing.T) {
	tests := []VerifiedDelivery{
		{DeliveryID: testDeliveryID, Event: "push", Payload: []byte(`{"ref":"refs/heads/main"}`)},
		{DeliveryID: testDeliveryID, Event: "pull_request", Action: "opened", Payload: []byte(`{"action":"opened"}`)},
		{DeliveryID: testDeliveryID, Event: "installation", Action: "created", Payload: []byte(`{"action":"deleted","installation":{"id":12}}`)},
	}
	for index, delivery := range tests {
		if _, err := Normalize(delivery); err == nil {
			t.Fatalf("invalid delivery %d was accepted", index)
		}
	}
}

func TestHTTPHandlerPersistsAndAcknowledgesDuplicate(t *testing.T) {
	inbox := &fakeInbox{inserted: true}
	handler, err := NewHTTPHandler([]byte(testSecret), inbox, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(validPullRequestWebhookPayload)
	request := httptest.NewRequest(http.MethodPost, "/v1/integrations/github/webhook", strings.NewReader(string(body)))
	request.Header = signedHeaders(body, "pull_request", testDeliveryID)
	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate":false`) || inbox.calls != 1 {
		t.Fatalf("first response = %d %s, calls=%d", response.Code, response.Body.String(), inbox.calls)
	}
	inbox.inserted = false
	request = httptest.NewRequest(http.MethodPost, "/v1/integrations/github/webhook", strings.NewReader(string(body)))
	request.Header = signedHeaders(body, "pull_request", testDeliveryID)
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate":true`) || inbox.calls != 2 {
		t.Fatalf("duplicate response = %d %s, calls=%d", response.Code, response.Body.String(), inbox.calls)
	}
}

func TestHTTPHandlerRejectsInvalidRequestsAndRetriesStoreFailures(t *testing.T) {
	tests := []struct {
		name       string
		method     string
		content    string
		body       string
		storeErr   error
		wantStatus int
	}{
		{name: "method", method: http.MethodGet, content: "application/json", body: `{}`, wantStatus: http.StatusMethodNotAllowed},
		{name: "content type", method: http.MethodPost, content: "text/plain", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
		{name: "signature", method: http.MethodPost, content: "application/json", body: validPushWebhookPayload, wantStatus: http.StatusUnauthorized},
		{name: "database unavailable", method: http.MethodPost, content: "application/json", body: validPushWebhookPayload, storeErr: errors.New("db unavailable"), wantStatus: http.StatusServiceUnavailable},
		{name: "delivery collision", method: http.MethodPost, content: "application/json", body: validPushWebhookPayload, storeErr: ErrDeliveryIDConflict, wantStatus: http.StatusConflict},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			inbox := &fakeInbox{inserted: true, err: test.storeErr}
			handler, err := NewHTTPHandler([]byte(testSecret), inbox, 1024)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(test.method, "/webhook", strings.NewReader(test.body))
			request.Header.Set("Content-Type", test.content)
			if test.name != "signature" {
				request.Header = signedHeaders([]byte(test.body), "push", testDeliveryID)
				request.Header.Set("Content-Type", test.content)
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != test.wantStatus {
				t.Fatalf("status = %d, want %d: %s", response.Code, test.wantStatus, response.Body.String())
			}
		})
	}
	invalidEvent, _ := NewHTTPHandler([]byte(testSecret), &fakeInbox{}, 1024)
	invalidBody := []byte(`{"ref":"refs/heads/main"}`)
	invalidRequest := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(string(invalidBody)))
	invalidRequest.Header = signedHeaders(invalidBody, "push", testDeliveryID)
	invalidRequest.Header.Set("Content-Type", "application/json")
	invalidResponse := httptest.NewRecorder()
	invalidEvent.ServeHTTP(invalidResponse, invalidRequest)
	if invalidResponse.Code != http.StatusUnprocessableEntity {
		t.Fatalf("incomplete event status = %d, want 422", invalidResponse.Code)
	}
	oversized, _ := NewHTTPHandler([]byte(testSecret), &fakeInbox{}, 2)
	request := httptest.NewRequest(http.MethodPost, "/webhook", strings.NewReader(`{}`+" "))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	oversized.ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("oversized request status = %d", response.Code)
	}
}
