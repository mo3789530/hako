package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"

	"github.com/mo3789530/hako/internal/modelcredentialbroker"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var microVMIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,127}$`)

type ActionsRunnerModelCredentialRequest struct {
	IntegrationID   string `json:"integration_id"`
	WorkflowJobID   int64  `json:"workflow_job_id"`
	RunAttempt      int64  `json:"run_attempt"`
	ClaimID         string `json:"claim_id"`
	LambdaMicroVMID string `json:"lambda_microvm_id"`
}

type ActionsRunnerModelCredentialIssuer interface {
	Issue(context.Context, string, ActionsRunnerModelCredentialRequest) ([]byte, bool, error)
}

type ActionsRunnerModelCredentialService struct {
	allowed  map[string]struct{}
	resolver *modelcredentialbroker.Resolver
}

func NewPostgresActionsRunnerModelCredentialService(resourcePlaneIDs []string, pool transaction.Beginner,
	policy transaction.Policy, reader modelcredentialbroker.SecretReader) (*ActionsRunnerModelCredentialService, error) {
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
	return &ActionsRunnerModelCredentialService{allowed: allowed, resolver: resolver}, nil
}

func (s *ActionsRunnerModelCredentialService) Issue(ctx context.Context, caller string, request ActionsRunnerModelCredentialRequest) ([]byte, bool, error) {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{
		IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: request.ClaimID,
	}) || !microVMIdentityPattern.MatchString(request.LambdaMicroVMID) {
		return nil, false, ErrActionsRunnerLaunchDenied
	}
	if _, allowed := s.allowed[caller]; !allowed {
		return nil, false, ErrActionsRunnerLaunchDenied
	}
	identity := modelcredentials.RunnerClaimIdentity{IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID,
		RunAttempt: request.RunAttempt, ClaimID: request.ClaimID, ResourcePlaneID: planeID, LambdaMicroVMID: request.LambdaMicroVMID}
	var secret []byte
	err = s.resolver.WithCredential(ctx, identity, func(_ context.Context, value []byte) error {
		secret = append([]byte(nil), value...)
		return nil
	})
	if errors.Is(err, modelcredentialbroker.ErrNotConfigured) {
		return nil, false, nil
	}
	if err != nil {
		clear(secret)
		return nil, false, ErrActionsRunnerLaunchDenied
	}
	return secret, true, nil
}

type HTTPActionsRunnerModelCredentialClient struct {
	base   string
	client *http.Client
}

func NewHTTPActionsRunnerModelCredentialClient(baseURL string, client *http.Client) (*HTTPActionsRunnerModelCredentialClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || client == nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPActionsRunnerModelCredentialClient{base: strings.TrimRight(base.String(), "/"), client: &copyClient}, nil
}

func (c *HTTPActionsRunnerModelCredentialClient) IssueModelCredential(ctx context.Context, request ActionsRunnerRegistrationRequest, microVMID string) ([]byte, error) {
	if c == nil || c.client == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 || !microVMIdentityPattern.MatchString(microVMID) {
		return nil, ErrActionsRunnerLaunchDenied
	}
	body, err := json.Marshal(ActionsRunnerModelCredentialRequest{IntegrationID: request.IntegrationID, WorkflowJobID: request.WorkflowJobID,
		RunAttempt: request.RunAttempt, ClaimID: request.ClaimID, LambdaMicroVMID: microVMID})
	if err != nil {
		return nil, ErrUnavailable
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/model-credential", bytes.NewReader(body))
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
	if resp.Header.Get("Cache-Control") != "no-store" {
		return nil, ErrActionsRunnerLaunchDenied
	}
	if resp.StatusCode == http.StatusNoContent {
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 12<<10 {
		return nil, ErrActionsRunnerLaunchDenied
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, (12<<10)+1))
	if err != nil || len(responseBody) > 12<<10 {
		clear(responseBody)
		return nil, ErrActionsRunnerLaunchDenied
	}
	defer clear(responseBody)
	var result struct {
		Credential []byte `json:"credential"`
	}
	decoder := json.NewDecoder(bytes.NewReader(responseBody))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&result) != nil || !validModelCredentialBytes(result.Credential) {
		clear(result.Credential)
		return nil, ErrActionsRunnerLaunchDenied
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		clear(result.Credential)
		return nil, ErrActionsRunnerLaunchDenied
	}
	return result.Credential, nil
}

func validModelCredentialBytes(value []byte) bool {
	if len(value) == 0 || len(value) > 8192 {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}
