package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/authz"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/audit"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

// WorkloadAgentPublicationStatus deliberately omits artifact storage refs,
// credentials, verifier execution metadata, and raw GitHub responses.
type WorkloadAgentPublicationStatus struct {
	TenantID          string                                  `json:"tenant_id"`
	WorkloadRunID     string                                  `json:"workload_run_id"`
	State             workloads.AgentProposalPublicationState `json:"state"`
	BaseBranch        string                                  `json:"base_branch"`
	BranchName        string                                  `json:"branch_name"`
	BranchCommitSHA   string                                  `json:"branch_commit_sha,omitempty"`
	PullRequestNumber int64                                   `json:"pull_request_number,omitempty"`
	CreatedAt         time.Time                               `json:"created_at"`
	UpdatedAt         time.Time                               `json:"updated_at"`
}

type AgentProposalPublicationSubmission struct {
	TenantID          string `json:"tenant_id"`
	WorkloadRunID     string `json:"workload_run_id"`
	State             string `json:"state"`
	BaseBranch        string `json:"base_branch"`
	BranchName        string `json:"branch_name"`
	BranchCommitSHA   string `json:"branch_commit_sha,omitempty"`
	PullRequestNumber int64  `json:"pull_request_number,omitempty"`
	Recovered         bool   `json:"recovered"`
}

func getWorkloadAgentPublication(pool transaction.Beginner) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil {
			return databaseUnavailable(c)
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		runID := domain.WorkloadRunID(strings.TrimSpace(c.Param("run_id")))
		if !workloadArtifactAccessID.MatchString(string(tenantID)) || !workloadArtifactAccessID.MatchString(string(runID)) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_workload_run", Message: "Workload Run identity is invalid"}})
		}
		result, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (WorkloadAgentPublicationStatus, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return WorkloadAgentPublicationStatus{}, err
			}
			run, err := workloads.Get(ctx, tx, tenantID, runID)
			if err != nil || run.Kind != domain.WorkloadAgent ||
				(membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && run.RequestedBy != user.ID) {
				return WorkloadAgentPublicationStatus{}, workloads.ErrNotFound
			}
			publication, err := workloads.GetLatestAgentProposalPublication(ctx, tx, tenantID, runID)
			if err != nil {
				return WorkloadAgentPublicationStatus{}, err
			}
			return WorkloadAgentPublicationStatus{TenantID: string(tenantID), WorkloadRunID: string(runID),
				State: publication.State, BaseBranch: publication.DefaultBranch, BranchName: publication.BranchName,
				BranchCommitSHA: publication.BranchCommitSHA, PullRequestNumber: publication.PullRequestNumber,
				CreatedAt: publication.CreatedAt, UpdatedAt: publication.UpdatedAt}, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, workloads.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Agent publication not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, result)
	}
}

func publishWorkloadAgentProposal(pool transaction.Beginner, submitter AgentProposalPublicationSubmitter) echo.HandlerFunc {
	return func(c *echo.Context) error {
		user, ok := HakoUserFromContext(c)
		if !ok || pool == nil || submitter == nil {
			return databaseUnavailable(c)
		}
		if c.Request().ContentLength > 0 {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Publication request must not contain a body"}})
		}
		if c.Request().Body != nil {
			body, err := io.ReadAll(io.LimitReader(c.Request().Body, 1))
			if err != nil || len(body) != 0 {
				return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_request", Message: "Publication request must not contain a body"}})
			}
		}
		tenantID := domain.TenantID(strings.TrimSpace(c.Param("tenant_id")))
		runID := domain.WorkloadRunID(strings.TrimSpace(c.Param("run_id")))
		if !workloadArtifactAccessID.MatchString(string(tenantID)) || !workloadArtifactAccessID.MatchString(string(runID)) {
			return c.JSON(http.StatusBadRequest, errorResponse{Error: errorBody{Code: "invalid_workload_run", Message: "Workload Run identity is invalid"}})
		}
		attemptID, err := transaction.Within(c.Request().Context(), pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (string, error) {
			membership, err := authz.RequireTenantMembership(ctx, tx, user.ID, tenantID)
			if err != nil {
				return "", err
			}
			run, err := workloads.Get(ctx, tx, tenantID, runID)
			if err != nil || run.Kind != domain.WorkloadAgent ||
				(membership.Role != domain.TenantRoleOwner && membership.Role != domain.TenantRoleAdmin && run.RequestedBy != user.ID) {
				return "", workloads.ErrNotFound
			}
			var latest string
			err = tx.QueryRow(ctx, `SELECT id FROM workload_run_attempts WHERE tenant_id=$1 AND workload_run_id=$2
				ORDER BY generation DESC LIMIT 1`, tenantID, runID).Scan(&latest)
			if err != nil || latest == "" {
				return "", workloads.ErrNotFound
			}
			details, _ := json.Marshal(map[string]string{"attempt_id": latest})
			if err := audit.Append(ctx, tx, audit.Event{TenantID: tenantID, ActorID: user.ID,
				Action: "workload.agent_proposal.publish", TargetType: "workload_run", TargetID: string(runID),
				Details: details, OccurredAt: time.Now().UTC()}); err != nil {
				return "", err
			}
			return latest, nil
		})
		if errors.Is(err, authz.ErrTenantAccessDenied) || errors.Is(err, workloads.ErrNotFound) {
			return c.JSON(http.StatusNotFound, errorResponse{Error: errorBody{Code: "not_found", Message: "Agent publication not found"}})
		}
		if err != nil {
			return databaseUnavailable(c)
		}
		published, err := submitter.Publish(c.Request().Context(), string(tenantID), string(runID), attemptID)
		if err != nil || published.TenantID != string(tenantID) || published.WorkloadRunID != string(runID) ||
			(published.State != "branch_created" && published.State != "pull_request_created") || published.BranchName == "" || published.BaseBranch == "" {
			return c.JSON(http.StatusBadGateway, errorResponse{Error: errorBody{Code: "publication_unavailable", Message: "Agent proposal could not be published"}})
		}
		c.Response().Header().Set("Cache-Control", "no-store")
		return c.JSON(http.StatusOK, published)
	}
}
