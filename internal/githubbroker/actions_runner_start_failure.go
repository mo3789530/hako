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
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerStartupFailureDenied = errors.New("GitHub Actions runner startup failure report denied")

type ActionsRunnerStartupFailureRecorder func(context.Context, string, ActionsRunnerRegistrationRequest) error

type ActionsRunnerStartupFailureService struct {
	allowed map[string]struct{}
	record  ActionsRunnerStartupFailureRecorder
}

type HTTPActionsRunnerStartupFailureClient struct {
	base   string
	client *http.Client
}

func NewHTTPActionsRunnerStartupFailureClient(baseURL string, client *http.Client) (*HTTPActionsRunnerStartupFailureClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || client == nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPActionsRunnerStartupFailureClient{base: strings.TrimRight(base.String(), "/"), client: &copyClient}, nil
}

func (c *HTTPActionsRunnerStartupFailureClient) Fail(ctx context.Context, request ActionsRunnerRegistrationRequest) error {
	if c == nil || c.client == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
		return ErrActionsRunnerStartupFailureDenied
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/startup-failed", bytes.NewReader(body))
	if err != nil {
		return ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Cache-Control", "no-store")
	resp, err := c.client.Do(req)
	if err != nil {
		return ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 1024 || resp.Header.Get("Cache-Control") != "no-store" {
		return ErrActionsRunnerStartupFailureDenied
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, 1024))
	decoder.DisallowUnknownFields()
	var decoded struct{}
	if decoder.Decode(&decoded) != nil {
		return ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ErrUnavailable
	}
	return nil
}

func NewActionsRunnerStartupFailureService(resourcePlaneIDs []string, record ActionsRunnerStartupFailureRecorder) (*ActionsRunnerStartupFailureService, error) {
	if len(resourcePlaneIDs) == 0 || record == nil {
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
	return &ActionsRunnerStartupFailureService{allowed: allowed, record: record}, nil
}

func (s *ActionsRunnerStartupFailureService) Fail(ctx context.Context, caller string, request ActionsRunnerRegistrationRequest) error {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || request.RunAttempt <= 0 || !validActionsRunnerRegistrationRequest(request) {
		return ErrActionsRunnerStartupFailureDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return ErrActionsRunnerStartupFailureDenied
	}
	if err := s.record(ctx, planeID, request); err != nil {
		return ErrActionsRunnerStartupFailureDenied
	}
	return nil
}

// NewPostgresActionsRunnerStartupFailureRecorder atomically transitions only
// the exact live starting claim owned by this Plane and inserts its cleanup
// record. Replays succeed only when the same Plane's cleanup outbox already
// contains the job attempt.
func NewPostgresActionsRunnerStartupFailureRecorder(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerStartupFailureRecorder {
	return func(ctx context.Context, planeID string, request ActionsRunnerRegistrationRequest) error {
		if pool == nil || ctx == nil || request.RunAttempt <= 0 || !resourcePlaneIDPattern.MatchString(planeID) || !validActionsRunnerRegistrationRequest(request) {
			return ErrActionsRunnerStartupFailureDenied
		}
		_, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
			now := time.Now().UTC()
			tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='failed',claim_id=NULL,lease_expires_at=NULL,updated_at=$5
				WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='starting' AND claim_id=$4
				AND resource_plane_id=$6 AND lease_expires_at>$5`, request.IntegrationID, request.WorkflowJobID, request.RunAttempt,
				request.ClaimID, now, planeID)
			if err != nil {
				return struct{}{}, err
			}
			if tag.RowsAffected() == 1 {
				_, err = tx.Exec(ctx, `INSERT INTO github_actions_runner_cleanup_outbox
					(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,reason,state,available_at,created_at,updated_at)
					SELECT integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,'failed','pending',$4,$4,$4
					FROM github_actions_runner_jobs WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='failed' AND resource_plane_id=$5
					ON CONFLICT (integration_id,workflow_job_id,run_attempt) DO NOTHING`, request.IntegrationID, request.WorkflowJobID, request.RunAttempt, now, planeID)
				return struct{}{}, err
			}
			var exists bool
			err = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM github_actions_runner_cleanup_outbox
				WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND resource_plane_id=$4 AND reason='failed')`,
				request.IntegrationID, request.WorkflowJobID, request.RunAttempt, planeID).Scan(&exists)
			if err != nil || !exists {
				return struct{}{}, ErrActionsRunnerStartupFailureDenied
			}
			return struct{}{}, nil
		})
		if err != nil {
			return ErrActionsRunnerStartupFailureDenied
		}
		return nil
	}
}
