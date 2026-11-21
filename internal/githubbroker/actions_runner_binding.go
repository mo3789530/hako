package githubbroker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerBindingDenied = errors.New("GitHub Actions runner binding denied")

type ActionsRunnerBindingRequest struct {
	IntegrationID string `json:"integration_id"`
	WorkflowJobID int64  `json:"workflow_job_id"`
	RunAttempt    int64  `json:"run_attempt"`
	ClaimID       string `json:"claim_id"`
	MicroVMID     string `json:"microvm_id"`
}

type ActionsRunnerBinder func(context.Context, string, ActionsRunnerBindingRequest) (bool, error)

type ActionsRunnerBindingService struct {
	allowed map[string]struct{}
	bind    ActionsRunnerBinder
}

func NewActionsRunnerBindingService(resourcePlaneIDs []string, bind ActionsRunnerBinder) (*ActionsRunnerBindingService, error) {
	if bind == nil || len(resourcePlaneIDs) == 0 {
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
	return &ActionsRunnerBindingService{allowed: allowed, bind: bind}, nil
}

func NewPostgresActionsRunnerBinder(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerBinder {
	return func(ctx context.Context, planeID string, request ActionsRunnerBindingRequest) (bool, error) {
		if pool == nil || ctx == nil || !resourcePlaneIDPattern.MatchString(planeID) || !validActionsRunnerBindingRequest(request) {
			return false, ErrActionsRunnerBindingDenied
		}
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return githubwebhook.BindActionsRunnerMicroVM(ctx, tx, request.IntegrationID, request.WorkflowJobID, request.RunAttempt,
				request.ClaimID, planeID, request.MicroVMID, time.Now().UTC())
		})
	}
}

func (s *ActionsRunnerBindingService) Bind(ctx context.Context, caller string, request ActionsRunnerBindingRequest) error {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || !validActionsRunnerBindingRequest(request) {
		return ErrActionsRunnerBindingDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return ErrActionsRunnerBindingDenied
	}
	bound, err := s.bind(ctx, planeID, request)
	if err != nil || !bound {
		return ErrActionsRunnerBindingDenied
	}
	return nil
}

func validActionsRunnerBindingRequest(request ActionsRunnerBindingRequest) bool {
	if !validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{IntegrationID: request.IntegrationID,
		WorkflowJobID: request.WorkflowJobID, RunAttempt: request.RunAttempt, ClaimID: request.ClaimID}) || request.RunAttempt <= 0 ||
		len(request.MicroVMID) == 0 || len(request.MicroVMID) > 256 || strings.TrimSpace(request.MicroVMID) != request.MicroVMID {
		return false
	}
	for _, char := range request.MicroVMID {
		if char < 0x21 || char == 0x7f {
			return false
		}
	}
	return true
}

type HTTPActionsRunnerBindingClient struct {
	base            string
	resourcePlaneID string
	client          *http.Client
}

func NewHTTPActionsRunnerBindingClient(baseURL, resourcePlaneID string, client *http.Client) (*HTTPActionsRunnerBindingClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if client == nil || err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		!resourcePlaneIDPattern.MatchString(resourcePlaneID) {
		return nil, ErrUnavailable
	}
	return &HTTPActionsRunnerBindingClient{base: strings.TrimRight(base.String(), "/"), resourcePlaneID: resourcePlaneID, client: client}, nil
}

func (c *HTTPActionsRunnerBindingClient) Bind(ctx context.Context, job githubwebhook.ActionsRunnerJob, microVMID string) error {
	if c == nil || c.client == nil || ctx == nil || job.State != "starting" || !job.LeaseExpires.After(time.Now().UTC()) ||
		!validActionsRunnerBindingRequest(ActionsRunnerBindingRequest{IntegrationID: job.IntegrationID, WorkflowJobID: job.JobID,
			RunAttempt: job.RunAttempt, ClaimID: job.ClaimID, MicroVMID: microVMID}) {
		return ErrActionsRunnerBindingDenied
	}
	body, err := json.Marshal(ActionsRunnerBindingRequest{IntegrationID: job.IntegrationID, WorkflowJobID: job.JobID,
		RunAttempt: job.RunAttempt, ClaimID: job.ClaimID, MicroVMID: microVMID})
	if err != nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/bind", bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 1024 || resp.Header.Get("Cache-Control") != "no-store" {
		return ErrActionsRunnerBindingDenied
	}
	var decoded struct{}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1024))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&decoded) != nil {
		return ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ErrUnavailable
	}
	return nil
}
