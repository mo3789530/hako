package githubwebhook

import (
	"context"
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"strings"
	"time"
)

// WebhookCredential is resolved by the opaque integration route. It contains
// only webhook-purpose material; implementations must not read App/OAuth keys.
type WebhookCredential struct {
	IntegrationID  string
	Generation     int64
	CurrentSecret  []byte
	PreviousSecret []byte
	PreviousUntil  time.Time
}

func (WebhookCredential) String() string     { return "Tenant GitHub App webhook credential [REDACTED]" }
func (c WebhookCredential) GoString() string { return c.String() }
func (WebhookCredential) MarshalJSON() ([]byte, error) {
	return json.Marshal("[REDACTED Tenant GitHub App webhook credential]")
}

type WebhookCredentialResolver interface {
	ResolveWebhookCredential(context.Context, string) (WebhookCredential, error)
}

// WebhookDeliveryAuthorizer checks route generation and Installation binding
// after HMAC verification and before any Tenant-specific inbox mutation.
type WebhookDeliveryAuthorizer interface {
	AuthorizeWebhookDelivery(context.Context, string, int64, RepositoryEvent) error
}

type TenantInbox interface {
	Record(context.Context, VerifiedDelivery, time.Time) (bool, error)
}

// WebhookAttemptLimiter is shared across API instances and must update its
// per-integration budget atomically before resolving any Secret.
type WebhookAttemptLimiter interface {
	AllowWebhookAttempt(context.Context, string, time.Time) (bool, error)
}

// TenantHTTPHandler serves an opaque per-integration route. It never searches
// all Tenant secrets: exactly one route candidate is resolved, and untrusted
// body identity is ignored until that candidate's HMAC passes.
type TenantHTTPHandler struct {
	resolver     WebhookCredentialResolver
	authorizer   WebhookDeliveryAuthorizer
	inbox        TenantInbox
	maxBodyBytes int64
	limiter      WebhookAttemptLimiter
	now          func() time.Time
}

func NewTenantHTTPHandler(resolver WebhookCredentialResolver, authorizer WebhookDeliveryAuthorizer, inbox TenantInbox, maxBodyBytes int64, limiters ...WebhookAttemptLimiter) (*TenantHTTPHandler, error) {
	if resolver == nil || authorizer == nil || inbox == nil {
		return nil, errors.New("Tenant webhook resolver, authorizer, and inbox are required")
	}
	if maxBodyBytes <= 0 {
		maxBodyBytes = DefaultMaxBodyBytes
	}
	var limiter WebhookAttemptLimiter
	if len(limiters) > 0 {
		limiter = limiters[0]
	}
	return &TenantHTTPHandler{resolver: resolver, authorizer: authorizer, inbox: inbox, maxBodyBytes: maxBodyBytes, limiter: limiter, now: time.Now}, nil
}

func (h *TenantHTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	const prefix = "/v1/integrations/github/apps/"
	const suffix = "/webhook"
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	path := r.URL.Path
	if !strings.HasPrefix(path, prefix) || !strings.HasSuffix(path, suffix) {
		http.NotFound(w, r)
		return
	}
	routeID := strings.TrimSuffix(strings.TrimPrefix(path, prefix), suffix)
	if !validRouteID(routeID) {
		http.NotFound(w, r)
		return
	}
	contentTypes := r.Header.Values("Content-Type")
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if len(contentTypes) != 1 || err != nil || mediaType != "application/json" {
		http.Error(w, "content type must be application/json", http.StatusUnsupportedMediaType)
		return
	}
	if err := ValidateWebhookHeaders(r.Header); err != nil {
		http.Error(w, "invalid webhook request", http.StatusBadRequest)
		return
	}
	body, err := ReadBounded(r, h.maxBodyBytes)
	if err != nil {
		http.Error(w, "invalid webhook request", http.StatusBadRequest)
		return
	}
	if h.limiter != nil {
		allowed, err := h.limiter.AllowWebhookAttempt(r.Context(), routeID, h.now().UTC())
		if err != nil {
			http.Error(w, "webhook temporarily unavailable", http.StatusServiceUnavailable)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "webhook rate limit exceeded", http.StatusTooManyRequests)
			return
		}
	}
	credential, err := h.resolver.ResolveWebhookCredential(r.Context(), routeID)
	if err != nil || credential.IntegrationID != routeID || credential.Generation <= 0 || len(credential.CurrentSecret) == 0 {
		http.NotFound(w, r)
		return
	}
	verified, err := Verify(r.Header, body, credential.CurrentSecret, h.maxBodyBytes)
	if err != nil && len(credential.PreviousSecret) > 0 && h.now().Before(credential.PreviousUntil) {
		verified, err = Verify(r.Header, body, credential.PreviousSecret, h.maxBodyBytes)
	}
	if err != nil {
		http.Error(w, "invalid webhook request", http.StatusUnauthorized)
		return
	}
	normalized, err := Normalize(verified)
	if err != nil {
		http.Error(w, "unsupported or incomplete webhook event", http.StatusUnprocessableEntity)
		return
	}
	if err := h.authorizer.AuthorizeWebhookDelivery(r.Context(), routeID, credential.Generation, normalized); err != nil {
		http.Error(w, "invalid webhook request", http.StatusUnauthorized)
		return
	}
	verified.IntegrationID = routeID
	verified.CredentialGeneration = credential.Generation
	inserted, err := h.inbox.Record(r.Context(), verified, h.now().UTC())
	if errors.Is(err, ErrDeliveryIDConflict) {
		http.Error(w, "delivery ID conflict", http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, "webhook temporarily unavailable", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusAccepted)
	_ = json.NewEncoder(w).Encode(struct {
		DeliveryID string `json:"delivery_id"`
		Duplicate  bool   `json:"duplicate"`
	}{DeliveryID: verified.DeliveryID, Duplicate: !inserted})
}

func validRouteID(value string) bool {
	if len(value) < 8 || len(value) > 128 {
		return false
	}
	for _, r := range value {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
