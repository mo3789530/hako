// Package githubwebhook validates the untrusted HTTP boundary for GitHub App
// webhook deliveries. Call Verify before parsing or persisting the payload.
package githubwebhook

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const DefaultMaxBodyBytes int64 = 1 << 20

var ErrInvalidDelivery = errors.New("invalid GitHub webhook delivery")

type VerifiedDelivery struct {
	DeliveryID string
	Event      string
	Action     string
	Payload    json.RawMessage
}

// Verify authenticates the exact raw request bytes before JSON decoding. It
// returns only allowlisted event/action pairs; callers must still authorize
// installation and repository ownership before creating work.
func Verify(headers http.Header, body []byte, secret []byte, maxBodyBytes int64) (VerifiedDelivery, error) {
	if len(secret) == 0 {
		return VerifiedDelivery{}, fmt.Errorf("%w: webhook secret is not configured", ErrInvalidDelivery)
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	if int64(len(body)) > maxBodyBytes {
		return VerifiedDelivery{}, fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidDelivery, maxBodyBytes)
	}
	if err := verifySignature(secret, body, headers.Get("X-Hub-Signature-256")); err != nil {
		return VerifiedDelivery{}, err
	}
	deliveryID := strings.TrimSpace(headers.Get("X-GitHub-Delivery"))
	if !validDeliveryID(deliveryID) {
		return VerifiedDelivery{}, fmt.Errorf("%w: X-GitHub-Delivery must be a UUID", ErrInvalidDelivery)
	}
	event := strings.TrimSpace(headers.Get("X-GitHub-Event"))
	if event == "" {
		return VerifiedDelivery{}, fmt.Errorf("%w: X-GitHub-Event is required", ErrInvalidDelivery)
	}
	var payload map[string]json.RawMessage
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(&payload); err != nil || payload == nil {
		return VerifiedDelivery{}, fmt.Errorf("%w: body must be one JSON object", ErrInvalidDelivery)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return VerifiedDelivery{}, fmt.Errorf("%w: body must contain exactly one JSON object", ErrInvalidDelivery)
	}
	action := ""
	if rawAction, exists := payload["action"]; exists {
		if err := json.Unmarshal(rawAction, &action); err != nil {
			return VerifiedDelivery{}, fmt.Errorf("%w: action must be a string", ErrInvalidDelivery)
		}
		action = strings.TrimSpace(action)
	}
	if !supportedEventAction(event, action) {
		return VerifiedDelivery{}, fmt.Errorf("%w: unsupported event/action %q/%q", ErrInvalidDelivery, event, action)
	}
	return VerifiedDelivery{DeliveryID: deliveryID, Event: event, Action: action, Payload: append(json.RawMessage(nil), body...)}, nil
}

func verifySignature(secret, body []byte, supplied string) error {
	if !strings.HasPrefix(supplied, "sha256=") || len(supplied) != len("sha256=")+sha256.Size*2 {
		return fmt.Errorf("%w: X-Hub-Signature-256 is missing or malformed", ErrInvalidDelivery)
	}
	signature, err := hex.DecodeString(strings.TrimPrefix(supplied, "sha256="))
	if err != nil {
		return fmt.Errorf("%w: X-Hub-Signature-256 is not valid hex", ErrInvalidDelivery)
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write(body)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return fmt.Errorf("%w: webhook signature does not match raw body", ErrInvalidDelivery)
	}
	return nil
}

func validDeliveryID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, char := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if char != '-' {
				return false
			}
			continue
		}
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}

func supportedEventAction(event, action string) bool {
	switch event {
	case "push":
		return action == ""
	case "installation":
		return oneOf(action, "created", "deleted", "suspend", "unsuspend")
	case "installation_repositories":
		return oneOf(action, "added", "removed")
	case "pull_request":
		return oneOf(action, "opened", "reopened", "synchronize", "closed")
	case "workflow_job":
		return oneOf(action, "queued", "in_progress", "completed")
	default:
		return false
	}
}

func oneOf(value string, choices ...string) bool {
	for _, choice := range choices {
		if value == choice {
			return true
		}
	}
	return false
}

// ReadBounded reads an HTTP body while enforcing the same limit as Verify. It
// must be called before handing the bytes to a JSON decoder.
func ReadBounded(request *http.Request, maxBodyBytes int64) ([]byte, error) {
	if request == nil || request.Body == nil {
		return nil, fmt.Errorf("%w: request body is required", ErrInvalidDelivery)
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBodyBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read GitHub webhook body: %w", err)
	}
	if int64(len(body)) > maxBodyBytes {
		return nil, fmt.Errorf("%w: body exceeds %d bytes", ErrInvalidDelivery, maxBodyBytes)
	}
	return body, nil
}
