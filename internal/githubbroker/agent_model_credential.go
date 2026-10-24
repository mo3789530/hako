package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/mo3789530/hako/internal/modelcredentialbroker"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const agentModelCredentialRoute = "/v1/agent/model-credential"

type AgentModelCredentialIssuer interface {
	Issue(context.Context, string, modelcredentials.AgentRunIdentity) ([]byte, error)
}

type AgentModelCredentialService struct {
	allowed  map[string]struct{}
	resolver *modelcredentialbroker.Resolver
}

func NewPostgresAgentModelCredentialService(resourcePlaneIDs []string, pool transaction.Beginner,
	policy transaction.Policy, reader modelcredentialbroker.SecretReader) (*AgentModelCredentialService, error) {
	if len(resourcePlaneIDs) == 0 {
		return nil, ErrUnavailable
	}
	resolver, err := modelcredentialbroker.New(pool, policy, reader)
	if err != nil {
		return nil, ErrUnavailable
	}
	allowed := make(map[string]struct{}, len(resourcePlaneIDs))
	for _, planeID := range resourcePlaneIDs {
		caller, err := ResourcePlaneCallerID(planeID)
		if err != nil {
			return nil, ErrUnavailable
		}
		if _, duplicate := allowed[caller]; duplicate {
			return nil, ErrUnavailable
		}
		allowed[caller] = struct{}{}
	}
	return &AgentModelCredentialService{allowed: allowed, resolver: resolver}, nil
}

func (s *AgentModelCredentialService) Issue(ctx context.Context, caller string, identity modelcredentials.AgentRunIdentity) ([]byte, error) {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || !validAgentCredentialIdentity(identity) || string(identity.ResourcePlaneID) != planeID {
		return nil, ErrAgentTaskUnavailable
	}
	if _, ok := s.allowed[caller]; !ok {
		return nil, ErrAgentTaskUnavailable
	}
	var credential []byte
	err = s.resolver.WithAgentRunCredential(ctx, identity, func(_ context.Context, value []byte) error {
		credential = append([]byte(nil), value...)
		return nil
	})
	if err != nil {
		clear(credential)
		return nil, ErrAgentTaskUnavailable
	}
	return credential, nil
}

func validAgentCredentialIdentity(identity modelcredentials.AgentRunIdentity) bool {
	return validAgentTaskIdentity(AgentTaskIdentity{TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID,
		AttemptID: identity.AttemptID, Generation: identity.Generation, ResourcePlaneID: identity.ResourcePlaneID})
}

// WithAgentModelCredential exposes model key material only to an allowlisted,
// verified Resource Plane mTLS caller. Responses are bounded and non-cacheable.
func WithAgentModelCredential(next http.Handler, issuer AgentModelCredentialIssuer) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentModelCredentialRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		caller, err := authenticatedResourcePlaneCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		identity, ok := decodeAgentModelCredentialRequest(r)
		if issuer == nil || !ok {
			writeError(w, http.StatusBadRequest)
			return
		}
		credential, err := issuer.Issue(r.Context(), caller, identity)
		if err != nil || !validModelCredentialBytes(credential) {
			clear(credential)
			writeError(w, http.StatusForbidden)
			return
		}
		defer clear(credential)
		body, err := json.Marshal(struct {
			Credential []byte `json:"credential"`
		}{Credential: credential})
		if err != nil || len(body) > 12<<10 {
			clear(body)
			writeError(w, http.StatusInternalServerError)
			return
		}
		defer clear(body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(body)
	})
}

func decodeAgentModelCredentialRequest(r *http.Request) (modelcredentials.AgentRunIdentity, bool) {
	var identity modelcredentials.AgentRunIdentity
	if r == nil || r.Body == nil || r.ContentLength > 2048 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
		return identity, false
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
	if err != nil || len(body) > 2048 {
		clear(body)
		return identity, false
	}
	defer clear(body)
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&identity) != nil || !validAgentCredentialIdentity(identity) {
		return modelcredentials.AgentRunIdentity{}, false
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return modelcredentials.AgentRunIdentity{}, false
	}
	return identity, true
}

type HTTPAgentModelCredentialClient struct {
	base   string
	client *http.Client
}

func NewHTTPAgentModelCredentialClient(baseURL string, client *http.Client) (*HTTPAgentModelCredentialClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || client == nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPAgentModelCredentialClient{base: strings.TrimRight(base.String(), "/"), client: &copyClient}, nil
}

func (c *HTTPAgentModelCredentialClient) Issue(ctx context.Context, identity modelcredentials.AgentRunIdentity) ([]byte, error) {
	if c == nil || c.client == nil || ctx == nil || !validAgentCredentialIdentity(identity) {
		return nil, ErrAgentTaskUnavailable
	}
	body, err := json.Marshal(identity)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+agentModelCredentialRoute, bytes.NewReader(body))
	if err != nil {
		return nil, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.Header.Get("Cache-Control") != "no-store" || resp.StatusCode != http.StatusOK || resp.ContentLength > 12<<10 {
		return nil, ErrAgentTaskUnavailable
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, (12<<10)+1))
	if err != nil || len(responseBody) > 12<<10 {
		clear(responseBody)
		return nil, ErrAgentTaskUnavailable
	}
	defer clear(responseBody)
	var result struct {
		Credential []byte `json:"credential"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !validModelCredentialBytes(result.Credential) {
		clear(result.Credential)
		return nil, ErrAgentTaskUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		clear(result.Credential)
		return nil, ErrAgentTaskUnavailable
	}
	return result.Credential, nil
}
