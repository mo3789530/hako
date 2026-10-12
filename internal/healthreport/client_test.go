package healthreport

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

type roundTripFunc func(*http.Request) (*http.Response, error)

func (fn roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func TestClientSendsSignedReportAndSupportsIdempotentRetry(t *testing.T) {
	key := []byte("health-report-key-material-32-bytes")
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	var requests int
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		requests++
		body, err := io.ReadAll(request.Body)
		if err != nil {
			t.Fatal(err)
		}
		if request.Method != http.MethodPost || request.URL.Path != "/v1/internal/resource-planes/rp-east-01/health" {
			t.Fatalf("request target = %s %s", request.Method, request.URL.Path)
		}
		timestamp, reportID := request.Header.Get("X-Hako-Timestamp"), request.Header.Get("X-Hako-Report-ID")
		if timestamp != "1791766923" || reportID != "report-123" {
			t.Fatalf("report identity headers = timestamp %q, report %q", timestamp, reportID)
		}
		if sign(key, request.Method, request.URL.EscapedPath(), timestamp, reportID, body) != request.Header.Get("X-Hako-Signature") {
			t.Fatal("request HMAC signature is invalid")
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"accepted":true}`)), Request: request}, nil
	})
	client, err := NewClient(ClientConfig{
		Endpoint: "https://control.hako.example", ResourcePlane: "rp-east-01", Key: key,
		HTTPClient: &http.Client{Transport: transport}, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	result := Result{Status: "healthy", Reason: "all configured capability probes passed", ObservedAt: now}
	if err := client.Send(context.Background(), "report-123", result); err != nil {
		t.Fatalf("first report send: %v", err)
	}
	if err := client.Send(context.Background(), "report-123", result); err != nil {
		t.Fatalf("idempotent retry: %v", err)
	}
	if requests != 2 {
		t.Fatalf("request count = %d, want 2", requests)
	}
}

func TestClientTreatsReporterOutageAsErrorWithoutLeakingResponse(t *testing.T) {
	client, err := NewClient(ClientConfig{
		Endpoint: "https://control.hako.example", ResourcePlane: "rp-east-01", Key: []byte("health-report-key-material-32-bytes"),
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusServiceUnavailable, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("private proxy details")), Request: request}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Send(context.Background(), "report-outage", Result{Status: "unhealthy", Reason: "failed capability probes: control-plane", ObservedAt: time.Now().UTC()})
	if err == nil || !strings.Contains(err.Error(), "503") || strings.Contains(err.Error(), "private proxy details") {
		t.Fatalf("reporter outage error = %v", err)
	}
}

func TestClientRejectsUnsafeConfigurationAndRedirects(t *testing.T) {
	key := []byte("health-report-key-material-32-bytes")
	for _, endpoint := range []string{"http://control.hako.example", "https://user:pass@control.hako.example", "https://control.hako.example/path"} {
		if _, err := NewClient(ClientConfig{Endpoint: endpoint, ResourcePlane: "rp-east-01", Key: key}); err == nil {
			t.Errorf("NewClient(%q) accepted an unsafe endpoint", endpoint)
		}
	}
	client, err := NewClient(ClientConfig{
		Endpoint: "https://control.hako.example", ResourcePlane: "rp-east-01", Key: key,
		HTTPClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: http.StatusTemporaryRedirect, Header: http.Header{"Location": []string{"https://attacker.example/collect"}}, Body: io.NopCloser(strings.NewReader("")), Request: request}, nil
		})},
	})
	if err != nil {
		t.Fatal(err)
	}
	err = client.Send(context.Background(), "report-redirect", Result{Status: "healthy", Reason: "ok", ObservedAt: time.Now().UTC()})
	if err == nil || !strings.Contains(err.Error(), "307") {
		t.Fatalf("redirect response error = %v", err)
	}
}
