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
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var ErrActionsRunnerLaunchDenied = errors.New("GitHub Actions runner launch denied")

type ActionsRunnerLaunchCommand struct {
	ResourcePlaneID string          `json:"resource_plane_id"`
	IntegrationID   string          `json:"integration_id"`
	TenantID        domain.TenantID `json:"tenant_id"`
	Installation    int64           `json:"installation_id"`
	Repository      int64           `json:"repository_id"`
	WorkflowJobID   int64           `json:"workflow_job_id"`
	WorkflowRunID   int64           `json:"workflow_run_id"`
	RunAttempt      int64           `json:"run_attempt"`
	WorkflowName    string          `json:"workflow_name"`
	JobName         string          `json:"job_name"`
	HeadBranch      string          `json:"head_branch"`
	HeadSHA         string          `json:"head_sha"`
	Labels          []string        `json:"labels"`
	ClaimID         string          `json:"claim_id"`
	LeaseExpires    time.Time       `json:"lease_expires_at"`
	Owner           string          `json:"repository_owner"`
	Name            string          `json:"repository_name"`
	State           string          `json:"state"`
}

type ActionsRunnerLaunchStarter func(context.Context, string, ActionsRunnerRegistrationRequest) (ActionsRunnerLaunchCommand, error)

type ActionsRunnerLaunchService struct {
	allowed    map[string]struct{}
	start      ActionsRunnerLaunchStarter
	preflight  ActionsRunnerLaunchPreflight
	startProof VerifiedActionsRunnerLaunchStarter
	clock      func() time.Time
}

type HTTPActionsRunnerLaunchClient struct {
	base            string
	resourcePlaneID string
	client          *http.Client
}

func NewHTTPActionsRunnerLaunchClient(baseURL, resourcePlaneID string, client *http.Client) (*HTTPActionsRunnerLaunchClient, error) {
	base, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil || client == nil || !resourcePlaneIDPattern.MatchString(resourcePlaneID) || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPActionsRunnerLaunchClient{base: strings.TrimRight(base.String(), "/"), resourcePlaneID: resourcePlaneID, client: &copyClient}, nil
}

func (c *HTTPActionsRunnerLaunchClient) Start(ctx context.Context, request ActionsRunnerRegistrationRequest) (ActionsRunnerLaunchCommand, error) {
	if c == nil || c.client == nil || ctx == nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	body, err := json.Marshal(request)
	if err != nil {
		return ActionsRunnerLaunchCommand{}, ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+"/v1/actions-runner/launch", bytes.NewReader(body))
	if err != nil {
		return ActionsRunnerLaunchCommand{}, ErrUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		return ActionsRunnerLaunchCommand{}, ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > 16<<10 || resp.Header.Get("Cache-Control") != "no-store" {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	var command ActionsRunnerLaunchCommand
	decoder := json.NewDecoder(io.LimitReader(resp.Body, (16<<10)+1))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&command) != nil {
		return ActionsRunnerLaunchCommand{}, ErrUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF || !validActionsRunnerLaunchCommand(command, c.resourcePlaneID, request, time.Now().UTC()) {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	return command, nil
}

func NewActionsRunnerLaunchService(resourcePlaneIDs []string, start ActionsRunnerLaunchStarter) (*ActionsRunnerLaunchService, error) {
	if len(resourcePlaneIDs) == 0 || start == nil {
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
	return &ActionsRunnerLaunchService{allowed: allowed, start: start, clock: time.Now}, nil
}

func NewVerifiedActionsRunnerLaunchService(resourcePlaneIDs []string, preflight ActionsRunnerLaunchPreflight,
	start VerifiedActionsRunnerLaunchStarter) (*ActionsRunnerLaunchService, error) {
	if len(resourcePlaneIDs) == 0 || preflight == nil || start == nil {
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
	return &ActionsRunnerLaunchService{allowed: allowed, preflight: preflight, startProof: start, clock: time.Now}, nil
}

func (s *ActionsRunnerLaunchService) Start(ctx context.Context, caller string, request ActionsRunnerRegistrationRequest) (ActionsRunnerLaunchCommand, error) {
	planeID, err := resourcePlaneIDFromCaller(caller)
	if s == nil || ctx == nil || err != nil || !validActionsRunnerRegistrationRequest(request) || request.RunAttempt <= 0 {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	if _, ok := s.allowed[caller]; !ok {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	var command ActionsRunnerLaunchCommand
	if s.preflight != nil {
		proof, err := s.preflight(ctx, request)
		if err != nil {
			return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
		}
		command, err = s.startProof(ctx, planeID, request, proof)
	} else {
		command, err = s.start(ctx, planeID, request)
	}
	if err != nil || !validActionsRunnerLaunchCommand(command, planeID, request, s.clock()) {
		return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
	}
	return command, nil
}

func validActionsRunnerLaunchCommand(command ActionsRunnerLaunchCommand, planeID string, request ActionsRunnerRegistrationRequest, now time.Time) bool {
	return command.ResourcePlaneID == planeID && command.IntegrationID == request.IntegrationID && command.WorkflowJobID == request.WorkflowJobID &&
		command.RunAttempt == request.RunAttempt && command.ClaimID == request.ClaimID && command.TenantID != "" && command.Installation > 0 &&
		command.Repository > 0 && command.WorkflowRunID > 0 && command.State == "starting" && command.LeaseExpires.After(now) &&
		validActionsRunnerBranch(command.HeadBranch) && validCommitSHA(command.HeadSHA) && validActionsRunnerRepositoryPart(command.Owner) &&
		validActionsRunnerRepositoryPart(command.Name) && len(command.Labels) > 0 && len(command.Labels) <= 100
}

func NewPostgresActionsRunnerLaunchStarter(pool transaction.Beginner, policy transaction.Policy) ActionsRunnerLaunchStarter {
	return postgresActionsRunnerLaunchStarter(pool, policy, nil, false)
}

// NewPostgresVerifiedActionsRunnerLaunchStarter consumes a launch claim only
// when the exact policy snapshot verified by the provenance preflight is still
// current. A nil proof is valid only while the repository has no policy.
func NewPostgresVerifiedActionsRunnerLaunchStarter(pool transaction.Beginner, policy transaction.Policy) VerifiedActionsRunnerLaunchStarter {
	return func(ctx context.Context, planeID string, request ActionsRunnerRegistrationRequest, proof []byte) (ActionsRunnerLaunchCommand, error) {
		return postgresActionsRunnerLaunchStarter(pool, policy, proof, true)(ctx, planeID, request)
	}
}

func postgresActionsRunnerLaunchStarter(pool transaction.Beginner, policy transaction.Policy, proof []byte, verified bool) ActionsRunnerLaunchStarter {
	return func(ctx context.Context, planeID string, request ActionsRunnerRegistrationRequest) (ActionsRunnerLaunchCommand, error) {
		if pool == nil || ctx == nil || !resourcePlaneIDPattern.MatchString(planeID) || request.RunAttempt <= 0 || !validActionsRunnerRegistrationRequest(request) {
			return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
		}
		now := time.Now().UTC()
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (ActionsRunnerLaunchCommand, error) {
			var command ActionsRunnerLaunchCommand
			var labelsJSON []byte
			var state string
			var currentPlaneID *string
			var tokenIssuedAt *time.Time
			var workflowPolicyJSON []byte
			err := tx.QueryRow(ctx, `SELECT j.integration_id,j.tenant_id,j.installation_id,j.repository_id,j.workflow_job_id,j.workflow_run_id,j.run_attempt,
				j.workflow_name,j.job_name,j.head_branch,j.head_sha,j.labels,j.claim_id,j.lease_expires_at,r.owner_login,r.repository_name,
				j.state,j.resource_plane_id,j.registration_token_issued_at,r.actions_runner_workflow_policy
				FROM github_actions_runner_jobs j
				JOIN github_webhook_deliveries d ON d.integration_id=j.integration_id AND d.delivery_id=j.delivery_id
				JOIN tenant_github_integrations i ON i.id=j.integration_id AND i.tenant_id=j.tenant_id
				JOIN tenant_github_app_installation_bindings b ON b.tenant_id=i.tenant_id AND b.integration_id=i.id AND b.installation_id=j.installation_id
				JOIN tenant_github_installations x ON x.tenant_id=b.tenant_id AND x.installation_id=b.installation_id
				JOIN tenant_github_repositories r ON r.tenant_id=j.tenant_id AND r.installation_id=j.installation_id AND r.github_repository_id=j.repository_id
				WHERE j.integration_id=$1 AND j.workflow_job_id=$2 AND j.run_attempt=$3 AND j.claim_id=$4
				AND j.lease_expires_at>$5 AND ((j.state='claimed' AND j.resource_plane_id IS NULL AND j.lambda_microvm_id IS NULL) OR
				(j.state='starting' AND j.resource_plane_id=$6 AND j.registration_token_issued_at IS NULL))
				AND d.processing_status='processed' AND d.credential_generation=i.credential_generation
				AND i.status='active' AND x.status='active' AND x.suspended_at IS NULL
				AND r.actions_runner_enabled=TRUE AND r.actions_runner_enabled_at IS NOT NULL AND r.actions_runner_enabled_at<=d.received_at
				FOR UPDATE OF j`, request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now, planeID).
				Scan(&command.IntegrationID, &command.TenantID, &command.Installation, &command.Repository, &command.WorkflowJobID, &command.WorkflowRunID,
					&command.RunAttempt, &command.WorkflowName, &command.JobName, &command.HeadBranch, &command.HeadSHA, &labelsJSON, &command.ClaimID,
					&command.LeaseExpires, &command.Owner, &command.Name, &state, &currentPlaneID, &tokenIssuedAt, &workflowPolicyJSON)
			if err != nil || json.Unmarshal(labelsJSON, &command.Labels) != nil ||
				(!verified && len(workflowPolicyJSON) != 0) || (verified && !bytes.Equal(workflowPolicyJSON, proof)) {
				return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
			}
			if state == "claimed" {
				tag, err := tx.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='starting',resource_plane_id=$6,updated_at=$5
					WHERE integration_id=$1 AND workflow_job_id=$2 AND run_attempt=$3 AND state='claimed' AND claim_id=$4
					AND lease_expires_at>$5 AND resource_plane_id IS NULL AND lambda_microvm_id IS NULL`,
					request.IntegrationID, request.WorkflowJobID, request.RunAttempt, request.ClaimID, now, planeID)
				if err != nil || tag.RowsAffected() != 1 {
					return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
				}
			} else if state != "starting" || currentPlaneID == nil || *currentPlaneID != planeID || tokenIssuedAt != nil {
				return ActionsRunnerLaunchCommand{}, ErrActionsRunnerLaunchDenied
			}
			command.ResourcePlaneID, command.State = planeID, "starting"
			return command, nil
		})
	}
}

func validCommitSHA(value string) bool {
	if len(value) != 40 && len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f')) {
			return false
		}
	}
	return true
}

func validActionsRunnerBranch(value string) bool {
	if len(value) == 0 || len(value) > 255 || strings.TrimSpace(value) != value || strings.Contains(value, "..") || strings.Contains(value, "//") ||
		strings.HasSuffix(value, "/") || strings.HasSuffix(value, ".") || strings.Contains(value, "@{") {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}
