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

type tenantDeliveryAuthorizerFunc func(context.Context, string, int64, int64) error

func (f tenantDeliveryAuthorizerFunc) AuthorizeWebhookDelivery(ctx context.Context, id string, generation, installation int64) error {
	return f(ctx, id, generation, installation)
}

type tenantInboxFunc func(context.Context, VerifiedDelivery, time.Time) (bool, error)

func (f tenantInboxFunc) Record(ctx context.Context, delivery VerifiedDelivery, at time.Time) (bool, error) {
	return f(ctx, delivery, at)
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
	}), tenantDeliveryAuthorizerFunc(func(_ context.Context, route string, generation, installation int64) error {
		authCalls++
		if route != integrationID || generation != 7 || installation != 12 {
			t.Fatalf("unexpected authorization identity: %s %d %d", route, generation, installation)
		}
		return nil
	}), tenantInboxFunc(func(_ context.Context, delivery VerifiedDelivery, _ time.Time) (bool, error) {
		inboxCalls++
		if delivery.IntegrationID != integrationID || delivery.DeliveryID != testDeliveryID {
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
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, int64) error { authCalls++; return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) {
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
			}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, int64) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return true, nil }), 0)
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
	}), tenantDeliveryAuthorizerFunc(func(context.Context, string, int64, int64) error { return nil }), tenantInboxFunc(func(context.Context, VerifiedDelivery, time.Time) (bool, error) { return true, nil }), 0)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest("POST", "/v1/integrations/github/apps/unknown_route/webhook", strings.NewReader("{}"))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != 404 || strings.Contains(response.Body.String(), "secret detail") {
		t.Fatalf("route leaked resolver state: %d %s", response.Code, response.Body.String())
	}
}
