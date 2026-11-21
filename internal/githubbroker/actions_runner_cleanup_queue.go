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

var ErrCleanupQueueUnavailable = errors.New("Actions runner cleanup queue unavailable")

const cleanupQueuePath = "/v1/actions-runner/cleanup-queue/"

type cleanupLeaseIdentity struct {
	IntegrationID string    `json:"integration_id"`
	WorkflowJobID int64     `json:"workflow_job_id"`
	RunAttempt    int64     `json:"run_attempt"`
	ClaimID       string    `json:"claim_id"`
	AttemptCount  int       `json:"attempt_count"`
	LeaseExpires  time.Time `json:"lease_expires_at"`
}

type cleanupQueueRequest struct {
	ClaimID           string                `json:"claim_id,omitempty"`
	LeaseSeconds      int64                 `json:"lease_seconds,omitempty"`
	Lease             *cleanupLeaseIdentity `json:"lease,omitempty"`
	RetryAfterSeconds int64                 `json:"retry_after_seconds,omitempty"`
	ErrorCode         string                `json:"error_code,omitempty"`
}

type cleanupQueueResponse struct {
	Applied bool                                   `json:"applied"`
	Job     *githubwebhook.ActionsRunnerCleanupJob `json:"job,omitempty"`
}

type ActionsRunnerCleanupQueueService struct {
	pool    transaction.Beginner
	policy  transaction.Policy
	allowed map[string]struct{}
}

func NewActionsRunnerCleanupQueueService(pool transaction.Beginner, policy transaction.Policy, planeIDs []string) (*ActionsRunnerCleanupQueueService, error) {
	if pool == nil || len(planeIDs) == 0 {
		return nil, ErrCleanupQueueUnavailable
	}
	allowed := make(map[string]struct{}, len(planeIDs))
	for _, plane := range planeIDs {
		caller, err := ResourcePlaneCallerID(plane)
		if err != nil {
			return nil, ErrCleanupQueueUnavailable
		}
		if _, exists := allowed[caller]; exists {
			return nil, ErrCleanupQueueUnavailable
		}
		allowed[caller] = struct{}{}
	}
	return &ActionsRunnerCleanupQueueService{pool: pool, policy: policy, allowed: allowed}, nil
}

// WithActionsRunnerCleanupQueue adds private, mTLS-authenticated lease routes.
// The caller's Plane comes solely from its verified certificate identity.
func WithActionsRunnerCleanupQueue(next http.Handler, service *ActionsRunnerCleanupQueueService) http.Handler {
	if next == nil || service == nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeError(w, http.StatusServiceUnavailable) })
	}
	mux := http.NewServeMux()
	for _, action := range []string{"claim", "complete", "retry"} {
		mux.HandleFunc("POST "+cleanupQueuePath+action, func(w http.ResponseWriter, r *http.Request) {
			caller, err := authenticatedCleanupCaller(r, service.allowed)
			if err != nil {
				writeError(w, http.StatusUnauthorized)
				return
			}
			plane, err := resourcePlaneIDFromCaller(caller)
			if err != nil {
				writeError(w, http.StatusForbidden)
				return
			}
			var input cleanupQueueRequest
			if r.Body == nil || r.ContentLength > 2048 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeError(w, http.StatusBadRequest)
				return
			}
			data, err := io.ReadAll(io.LimitReader(r.Body, 2049))
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			if err != nil || len(data) > 2048 || decoder.Decode(&input) != nil || decoder.Decode(&struct{}{}) != io.EOF || !validCleanupQueueRequest(action, input) {
				writeError(w, http.StatusBadRequest)
				return
			}
			result, err := transaction.Within(r.Context(), service.pool, service.policy, func(ctx context.Context, tx pgx.Tx) (cleanupQueueResponse, error) {
				// The broker's clock governs both acquisition and mutation. No
				// caller timestamp can extend or resurrect an expired lease.
				now := time.Now().UTC()
				if action == "claim" {
					job, found, err := githubwebhook.ClaimNextActionsRunnerCleanup(ctx, tx, plane, input.ClaimID, now, time.Duration(input.LeaseSeconds)*time.Second)
					if err != nil || !found {
						return cleanupQueueResponse{}, err
					}
					return cleanupQueueResponse{Applied: true, Job: &job}, nil
				}
				lease := input.Lease
				var job githubwebhook.ActionsRunnerCleanupJob
				err := tx.QueryRow(ctx, `SELECT integration_id,tenant_id,resource_plane_id,installation_id,repository_id,workflow_job_id,run_attempt,
					runner_id,lambda_microvm_id,reason,state,claim_id,lease_expires_at,attempt_count,created_at
					FROM github_actions_runner_cleanup_outbox
					WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND claim_id=$4 AND attempt_count=$5
					AND lease_expires_at=$6 AND lease_expires_at>$7 AND state='claimed'
					AND (resource_plane_id=$8 OR (resource_plane_id IS NULL AND lambda_microvm_id IS NULL)) FOR UPDATE`,
					lease.IntegrationID, lease.WorkflowJobID, lease.RunAttempt, lease.ClaimID, lease.AttemptCount, lease.LeaseExpires.UTC(), now, plane).
					Scan(&job.IntegrationID, &job.TenantID, &job.ResourcePlaneID, &job.Installation, &job.Repository, &job.WorkflowJobID, &job.RunAttempt,
						&job.RunnerID, &job.LambdaMicroVMID, &job.Reason, &job.State, &job.ClaimID, &job.LeaseExpires, &job.AttemptCount, &job.CreatedAt)
				if errors.Is(err, pgx.ErrNoRows) {
					return cleanupQueueResponse{}, nil
				}
				if err != nil {
					return cleanupQueueResponse{}, err
				}
				now = time.Now().UTC()
				var applied bool
				if action == "complete" {
					applied, err = githubwebhook.CompleteActionsRunnerCleanup(ctx, tx, job, now)
				} else {
					applied, err = githubwebhook.RetryActionsRunnerCleanup(ctx, tx, job, now, now.Add(time.Duration(input.RetryAfterSeconds)*time.Second), input.ErrorCode)
				}
				return cleanupQueueResponse{Applied: applied}, err
			})
			if err != nil {
				writeError(w, http.StatusServiceUnavailable)
				return
			}
			w.Header().Set("Cache-Control", "no-store")
			writeResponse(w, result)
		})
	}
	mux.Handle("/", next)
	return mux
}

func validCleanupQueueRequest(action string, input cleanupQueueRequest) bool {
	if action == "claim" {
		return input.Lease == nil && input.RetryAfterSeconds == 0 && input.ErrorCode == "" && input.LeaseSeconds >= 5 && input.LeaseSeconds <= 600 &&
			validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{IntegrationID: "cleanup-queue", WorkflowJobID: 1, RunAttempt: 1, ClaimID: input.ClaimID})
	}
	if input.ClaimID != "" || input.LeaseSeconds != 0 || input.Lease == nil || input.Lease.AttemptCount < 1 || input.Lease.LeaseExpires.IsZero() || input.Lease.RunAttempt <= 0 ||
		!validActionsRunnerRegistrationRequest(ActionsRunnerRegistrationRequest{IntegrationID: input.Lease.IntegrationID, WorkflowJobID: input.Lease.WorkflowJobID, RunAttempt: input.Lease.RunAttempt, ClaimID: input.Lease.ClaimID}) {
		return false
	}
	if action == "complete" {
		return input.RetryAfterSeconds == 0 && input.ErrorCode == ""
	}
	if action != "retry" || input.RetryAfterSeconds < 1 || input.RetryAfterSeconds > 900 {
		return false
	}
	switch input.ErrorCode {
	case "provider_unavailable", "microvm_terminate_failed", "runner_deregister_failed":
		return true
	default:
		return false
	}
}

// HTTPActionsRunnerCleanupQueueClient implements the Resource Plane worker's
// queue interface without granting that worker any database access.
type HTTPActionsRunnerCleanupQueueClient struct {
	base, plane string
	client      *http.Client
}

func NewHTTPActionsRunnerCleanupQueueClient(base, plane string, client *http.Client) (*HTTPActionsRunnerCleanupQueueClient, error) {
	u, err := url.Parse(base)
	if err != nil || client == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !resourcePlaneIDPattern.MatchString(plane) {
		return nil, ErrCleanupQueueUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPActionsRunnerCleanupQueueClient{base: strings.TrimRight(base, "/"), plane: plane, client: &copyClient}, nil
}

func (c *HTTPActionsRunnerCleanupQueueClient) call(ctx context.Context, action string, input cleanupQueueRequest) (cleanupQueueResponse, error) {
	if c == nil || c.client == nil || ctx == nil || !validCleanupQueueRequest(action, input) {
		return cleanupQueueResponse{}, ErrCleanupQueueUnavailable
	}
	data, err := json.Marshal(input)
	if err != nil {
		return cleanupQueueResponse{}, ErrCleanupQueueUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+cleanupQueuePath+action, bytes.NewReader(data))
	if err != nil {
		return cleanupQueueResponse{}, ErrCleanupQueueUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := c.client.Do(req)
	if err != nil {
		return cleanupQueueResponse{}, ErrCleanupQueueUnavailable
	}
	defer res.Body.Close()
	data, err = io.ReadAll(io.LimitReader(res.Body, 8193))
	var result struct {
		Applied *bool                                  `json:"applied"`
		Job     *githubwebhook.ActionsRunnerCleanupJob `json:"job,omitempty"`
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err != nil || len(data) > 8192 || res.StatusCode != http.StatusOK || res.Header.Get("Cache-Control") != "no-store" ||
		decoder.Decode(&result) != nil || decoder.Decode(&struct{}{}) != io.EOF || result.Applied == nil || (action != "claim" && result.Job != nil) {
		return cleanupQueueResponse{}, ErrCleanupQueueUnavailable
	}
	return cleanupQueueResponse{Applied: *result.Applied, Job: result.Job}, nil
}

func (c *HTTPActionsRunnerCleanupQueueClient) Claim(ctx context.Context, plane, claimID string, _ time.Time, lease time.Duration) (githubwebhook.ActionsRunnerCleanupJob, bool, error) {
	if c == nil || plane != c.plane || lease%time.Second != 0 {
		return githubwebhook.ActionsRunnerCleanupJob{}, false, ErrCleanupQueueUnavailable
	}
	result, err := c.call(ctx, "claim", cleanupQueueRequest{ClaimID: claimID, LeaseSeconds: int64(lease / time.Second)})
	if err != nil || !result.Applied {
		if result.Job != nil {
			err = ErrCleanupQueueUnavailable
		}
		return githubwebhook.ActionsRunnerCleanupJob{}, false, err
	}
	job := result.Job
	if job == nil || job.ClaimID != claimID || job.State != "claimed" || job.AttemptCount < 1 || job.TenantID == "" || job.Installation <= 0 || job.Repository <= 0 ||
		!validCleanupQueueRequest("complete", cleanupQueueRequest{Lease: cleanupLeaseFromJob(*job)}) ||
		(job.ResourcePlaneID != nil && *job.ResourcePlaneID != c.plane) || (job.LambdaMicroVMID != nil && job.ResourcePlaneID == nil) ||
		!job.LeaseExpires.After(time.Now()) || job.LeaseExpires.After(time.Now().Add(lease+5*time.Second)) {
		return githubwebhook.ActionsRunnerCleanupJob{}, false, ErrCleanupQueueUnavailable
	}
	return *job, true, nil
}

func cleanupLeaseFromJob(job githubwebhook.ActionsRunnerCleanupJob) *cleanupLeaseIdentity {
	return &cleanupLeaseIdentity{IntegrationID: job.IntegrationID, WorkflowJobID: job.WorkflowJobID, RunAttempt: job.RunAttempt,
		ClaimID: job.ClaimID, AttemptCount: job.AttemptCount, LeaseExpires: job.LeaseExpires}
}

func (c *HTTPActionsRunnerCleanupQueueClient) Complete(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob, _ time.Time) (bool, error) {
	result, err := c.call(ctx, "complete", cleanupQueueRequest{Lease: cleanupLeaseFromJob(job)})
	return result.Applied, err
}

func (c *HTTPActionsRunnerCleanupQueueClient) Retry(ctx context.Context, job githubwebhook.ActionsRunnerCleanupJob, now, retryAt time.Time, code string) (bool, error) {
	delay := retryAt.Sub(now)
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	result, err := c.call(ctx, "retry", cleanupQueueRequest{Lease: cleanupLeaseFromJob(job), RetryAfterSeconds: seconds, ErrorCode: code})
	return result.Applied, err
}
