package apigwlambda

import (
	"context"
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"github.com/aws/aws-lambda-go/events"
)

func TestHandleAdaptsHTTPAPIEventAndResponse(t *testing.T) {
	handler := New(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_1/workspaces" || r.URL.RawQuery != "limit=2" {
			t.Errorf("unexpected adapted request: %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer token" {
			t.Errorf("Authorization header = %q, want Bearer token", got)
		}
		if got := r.Header.Get("Cookie"); got != "a=b" {
			t.Errorf("Cookie header = %q, want a=b", got)
		}
		body := make([]byte, 2)
		if _, err := r.Body.Read(body); err != nil || string(body) != "{}" {
			t.Errorf("request body = %q, error = %v", body, err)
		}
		w.Header().Add("Set-Cookie", "session=one; Secure")
		w.Header().Add("Set-Cookie", "preference=two; Secure")
		w.Header().Add("X-Trace", "one")
		w.Header().Add("X-Trace", "two")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"status":"accepted"}`))
	}))
	event := events.APIGatewayV2HTTPRequest{
		Version: "2.0", RawPath: "/v1/tenants/tenant_1/workspaces", RawQueryString: "limit=2",
		Headers: map[string]string{"authorization": "Bearer token", "host": "api.example.test"},
		Cookies: []string{"a=b"}, Body: base64.StdEncoding.EncodeToString([]byte("{}")), IsBase64Encoded: true,
		RequestContext: events.APIGatewayV2HTTPRequestContext{
			DomainName: "api.example.test",
			HTTP:       events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: http.MethodPost, SourceIP: "192.0.2.10"},
		},
	}
	response, err := handler.Handle(context.Background(), event)
	if err != nil {
		t.Fatalf("handle API Gateway event: %v", err)
	}
	if response.StatusCode != http.StatusAccepted || response.Body != `{"status":"accepted"}` || response.IsBase64Encoded {
		t.Fatalf("unexpected API Gateway response: %+v", response)
	}
	if strings.Join(response.Cookies, "|") != "session=one; Secure|preference=two; Secure" {
		t.Fatalf("Set-Cookie headers were not preserved as API Gateway cookies: %+v", response.Cookies)
	}
	if response.Headers["X-Trace"] != "one,two" {
		t.Fatalf("repeated response header was not joined: %+v", response.Headers)
	}
}

func TestHandleRejectsInvalidEventAndOversizedBody(t *testing.T) {
	handler := New(http.NotFoundHandler())
	if _, err := handler.Handle(context.Background(), events.APIGatewayV2HTTPRequest{Version: "1.0"}); err == nil {
		t.Fatal("payload version other than 2.0 should be rejected")
	}
	if _, err := handler.Handle(context.Background(), events.APIGatewayV2HTTPRequest{
		Version: "2.0", RawPath: "/health", RequestContext: events.APIGatewayV2HTTPRequestContext{HTTP: events.APIGatewayV2HTTPRequestContextHTTPDescription{Method: http.MethodPost}},
		Body: strings.Repeat("x", maxEventBodyBytes+1),
	}); err != nil {
		t.Fatalf("oversized request should return an HTTP response, not an invocation error: %v", err)
	}
}
