package githubwebhook

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testSecret = "test-webhook-secret"
const testDeliveryID = "a1b2c3d4-e5f6-4789-8abc-def012345678"

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
