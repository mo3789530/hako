package githubwebhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type tenantResolverFunc func(context.Context, string) (WebhookCredential, error)

func (f tenantResolverFunc) ResolveWebhookCredential(ctx context.Context, id string) (WebhookCredential, error) {
	return f(ctx, id)
}

type tenantDeliveryAuthorizerFunc func(context.Context, string, int64, RepositoryEvent) error

func (f tenantDeliveryAuthorizerFunc) AuthorizeWebhookDelivery(ctx context.Context, id string, generation int64, event RepositoryEvent) error {
	return f(ctx, id, generation, event)
}

type tenantInboxFunc func(context.Context, VerifiedDelivery, time.Time) (bool, error)

func (f tenantInboxFunc) Record(ctx context.Context, delivery VerifiedDelivery, at time.Time) (bool, error) {
	return f(ctx, delivery, at)
}

type tenantLimiterFunc func(context.Context, string, time.Time) (bool, error)

func (f tenantLimiterFunc) AllowWebhookAttempt(ctx context.Context, id string, now time.Time) (bool, error) {
	return f(ctx, id, now)
}

func signWebhook(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestTenantWebhookVerifiesBeforeAuthorizationAndScopesDelivery(t *testing.T) {
	const integrationID = "integration_tenant_a"
	secret := []byte("tenant-a-webhook-secret")
	body := []byte(validPushWebhookPayload)
	resolverCalls, authCalls, inboxCalls := 0, 0, 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(_ context.Context, route string) (WebhookCredential, error) {
		resolverCalls++
		if route != integrationID {
			t.Fatalf("resolver got untrusted route %q", route)
		}
		return WebhookCredential{IntegrationID: route, Generation: 7, CurrentSecret: secret}, nil
	}), tenantDeliveryAuthorizerFunc(func(_ context.Context, route string, generation int64, event RepositoryEvent) error {
		authCalls++
		if route != integrationID || generation != 7 || event.InstallationID != 12 {
			t.Fatalf("unexpected authorization identity: %s %d %+v", route, generation, event)
		}
		return nil
	}), tenantInboxFunc(func(_ context.Context, delivery VerifiedDelivery, _ time.Time) (bool, error) {
		inboxCalls++
		if delivery.IntegrationID != integrationID || delivery.CredentialGeneration != 7 || delivery.DeliveryID != testDeliveryID {
			t.Fatalf("delivery lacks trusted integration identity: %+v", delivery)
		}
		return true, nil
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/"+integrationID+"/webhook", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Set("X-Hub-Signature-256", signWebhook(secret, body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 202 || resolverCalls != 1 || authCalls != 1 || inboxCalls != 1 {
		t.Fatalf("request status/calls = %d/%d/%d/%d", response.Code, resolverCalls, authCalls, inboxCalls)
	}
}

func TestTenantWebhookRejectsInvalidSignatureBeforeTenantAuthorization(t *testing.T) {
	secret := []byte("tenant-a-webhook-secret")
	authCalls := 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(_ context.Context, route string) (WebhookCredential, error) {
		return WebhookCredential{IntegrationID: route, Generation: 1, CurrentSecret: secret}, nil
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { authCalls++; return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) {
		t.Fatal("invalid signature reached inbox")
		return false, nil
	}), 0)
	if err != nil {
		t.Fatal(err)
	}
	body := []byte(validPushWebhookPayload)
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader(string(body)))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Set("X-Hub-Signature-256", signWebhook([]byte("wrong-secret"), body))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 401 || authCalls != 0 {
		t.Fatalf("invalid HMAC reached Tenant authorization: status=%d auth=%d", response.Code, authCalls)
	}
}

func TestTenantWebhookAllowsPreviousSecretOnlyDuringGrace(t *testing.T) {
	previous := []byte("old-secret")
	body := []byte(validPushWebhookPayload)
	for _, tc := range []struct {
		name  string
		until time.Time
		want  int
	}{
		{name: "in grace", until: time.Now().Add(time.Minute), want: 202},
		{name: "expired", until: time.Now().Add(-time.Second), want: 401},
	} {
		t.Run(tc.name, func(t *testing.T) {
			handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(_ context.Context, route string) (WebhookCredential, error) {
				return WebhookCredential{IntegrationID: route, Generation: 2, CurrentSecret: []byte("new-secret"), PreviousSecret: previous, PreviousUntil: tc.until}, nil
			}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return true, nil }), 0)
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader(string(body)))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-GitHub-Event", "push")
			request.Header.Set("X-GitHub-Delivery", testDeliveryID)
			request.Header.Set("X-Hub-Signature-256", signWebhook(previous, body))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want {
				t.Fatalf("status=%d want=%d", response.Code, tc.want)
			}
		})
	}
}

func TestTenantWebhookHidesUnknownRoutesAndResolverErrors(t *testing.T) {
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(context.Context, string) (WebhookCredential, error) {
		return WebhookCredential{}, errors.New("secret detail")
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return true, nil }), 0)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/unknown_route/webhook", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Set("X-Hub-Signature-256", signWebhook([]byte("unknown-route-secret"), []byte("{}")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 404 || strings.Contains(response.Body.String(), "secret detail") {
		t.Fatalf("route leaked resolver state: %d %s", response.Code, response.Body.String())
	}
}

func TestTenantWebhookRejectsMalformedTransportBeforeSecretRead(t *testing.T) {
	resolverCalls := 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(_ context.Context, route string) (WebhookCredential, error) {
		resolverCalls++
		return WebhookCredential{IntegrationID: route, Generation: 1, CurrentSecret: []byte("secret")}, nil
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return true, nil }), 8)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, contentType, body string
		want                    int
	}{
		{name: "wrong content type", contentType: "text/plain", body: "x", want: 415},
		{name: "oversized", contentType: "application/json", body: strings.Repeat("x", 9), want: 400},
	} {
		t.Run(tc.name, func(t *testing.T) {
			request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader(tc.body))
			request.Header.Set("Content-Type", tc.contentType)
			if tc.name == "oversized" {
				request.Header.Set("X-GitHub-Event", "push")
				request.Header.Set("X-GitHub-Delivery", testDeliveryID)
				request.Header.Set("X-Hub-Signature-256", signWebhook([]byte("secret"), []byte(tc.body)))
			}
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, request)
			if response.Code != tc.want || resolverCalls != 0 {
				t.Fatalf("status=%d resolverCalls=%d", response.Code, resolverCalls)
			}
		})
	}
}

func TestTenantWebhookLimiterFailureFailsClosedBeforeSecretResolution(t *testing.T) {
	resolverCalls := 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(context.Context, string) (WebhookCredential, error) {
		resolverCalls++
		return WebhookCredential{}, nil
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return false, nil }), 0, tenantLimiterFunc(func(context.Context, string, time.Time) (bool, error) {
		return false, errors.New("database detail must not leak")
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := validPushWebhookPayload
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Set("X-Hub-Signature-256", signWebhook([]byte("secret"), []byte(body)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 503 || resolverCalls != 0 || strings.Contains(response.Body.String(), "database detail") {
		t.Fatalf("status=%d resolver=%d body=%q", response.Code, resolverCalls, response.Body.String())
	}
}

func TestTenantWebhookRateLimitRunsBeforeSecretResolution(t *testing.T) {
	resolverCalls, limitCalls := 0, 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(context.Context, string) (WebhookCredential, error) {
		resolverCalls++
		return WebhookCredential{}, errors.New("must not resolve")
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return false, nil }), 0, tenantLimiterFunc(func(_ context.Context, id string, _ time.Time) (bool, error) {
		limitCalls++
		if id != "integration_tenant_a" {
			t.Fatalf("unexpected integration %q", id)
		}
		return false, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	body := validPushWebhookPayload
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Set("X-Hub-Signature-256", signWebhook([]byte("secret"), []byte(body)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 429 || response.Header().Get("Retry-After") != "60" || resolverCalls != 0 || limitCalls != 1 {
		t.Fatalf("status=%d limit=%d resolver=%d", response.Code, limitCalls, resolverCalls)
	}
}

func TestTenantWebhookRejectsMalformedHeadersBeforeLimiter(t *testing.T) {
	limitCalls := 0
	handler, err := NewTenantHTTPHandler(tenantResolverFunc(func(context.Context, string) (WebhookCredential, error) {
		t.Fatal("malformed headers resolved secret")
		return WebhookCredential{}, nil
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, RepositoryEvent) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return false, nil }), 0, tenantLimiterFunc(func(context.Context, string, time.Time) (bool, error) { limitCalls++; return true, nil }))
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/integration_tenant_a/webhook", strings.NewReader("{}"))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-GitHub-Event", "push")
	request.Header.Set("X-GitHub-Delivery", testDeliveryID)
	request.Header.Add("X-Hub-Signature-256", signWebhook([]byte("secret"), []byte("{}")))
	request.Header.Add("X-Hub-Signature-256", signWebhook([]byte("secret"), []byte("{}")))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 400 || limitCalls != 0 {
		t.Fatalf("status=%d limiter=%d", response.Code, limitCalls)
	}
}
