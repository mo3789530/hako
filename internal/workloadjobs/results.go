package workloadjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var (
	ErrWorkloadResultMismatch  = errors.New("Workload result does not match its Run or attempt")
	ErrWorkloadResultInvalid   = errors.New("Workload result is invalid")
	resultErrorCodePattern     = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	resultCorrelationIDPattern = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	resultCommitSHAPattern     = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	resultSHA256Pattern        = regexp.MustCompile(`^[0-9a-f]{64}$`)
	pipelineNamePattern        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
)

// WorkloadResult is emitted by a trusted Resource Plane result channel. It
// contains metadata only; log bytes must already be redacted and uploaded.
type WorkloadResult struct {
	SchemaVersion   int                      `json:"schema_version"`
	TenantID        domain.TenantID          `json:"tenant_id"`
	WorkloadRunID   domain.WorkloadRunID     `json:"workload_run_id"`
	AttemptID       string                   `json:"attempt_id"`
	Generation      int64                    `json:"generation"`
	ResourcePlaneID domain.ResourcePlaneID   `json:"resource_plane_id"`
	CorrelationID   string                   `json:"correlation_id,omitempty"`
	CommitSHA       string                   `json:"commit_sha"`
	Status          domain.WorkloadRunState  `json:"status"`
	ExitCode        int                      `json:"exit_code"`
	ErrorCode       string                   `json:"error_code,omitempty"`
	CompletedAt     time.Time                `json:"completed_at"`
	LogArtifact     *WorkloadLogArtifact     `json:"log_artifact,omitempty"`
	ChangeBundle    *WorkloadChangeArtifact  `json:"change_bundle,omitempty"`
	PipelineResults []WorkloadPipelineResult `json:"pipeline_results,omitempty"`
}

// WorkloadPipelineResult records bounded manifest metadata and validated
// repository diagnostics. It never contains raw command output.
type WorkloadPipelineResult struct {
	Name        string                           `json:"name"`
	Status      string                           `json:"status"`
	ExitCode    int                              `json:"exit_code"`
	ErrorCode   string                           `json:"error_code,omitempty"`
	Annotations []domain.WorkloadCheckAnnotation `json:"annotations,omitempty"`
}

type WorkloadLogArtifact struct {
	StorageRef string `json:"storage_ref"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
	Redacted   bool   `json:"redacted"`
}

// WorkloadChangeArtifact identifies a bounded Native Agent proposal bundle.
// The bundle is untrusted source material and is not a patch authorization.
type WorkloadChangeArtifact struct {
	StorageRef string `json:"storage_ref"`
	SizeBytes  int64  `json:"size_bytes"`
	SHA256     string `json:"sha256"`
}

type ResultOutcome string

const (
	ResultApplied   ResultOutcome = "applied"
	ResultDuplicate ResultOutcome = "duplicate"
	ResultStale     ResultOutcome = "stale"
)

type ResultHandler struct {
	Pool   transaction.Beginner
	Policy transaction.Policy
	Now    func() time.Time
}

// Handle applies a result only to the matching current attempt. Replayed
// terminal results are idempotent; results from older generations are stale
// and acknowledged without changing the Run.
func (h *ResultHandler) Handle(ctx context.Context, result WorkloadResult) (ResultOutcome, error) {
	if h == nil || h.Pool == nil || ctx == nil {
		return "", errors.New("context and Workload result database are required")
	}
	if err := validateWorkloadResult(result); err != nil {
		return "", err
	}
	now := time.Now
	if h.Now != nil {
		now = h.Now
	}
	completedAt := result.CompletedAt.UTC()
	if completedAt.IsZero() {
		completedAt = now().UTC()
	}
	artifactID := ""
	changeArtifactID := ""
	if result.LogArtifact != nil {
		var err error
		artifactID, err = idgen.New("artifact_")
		if err != nil {
			return "", fmt.Errorf("generate Workload artifact ID: %w", err)
		}
	}
	if result.ChangeBundle != nil {
		var err error
		changeArtifactID, err = idgen.New("artifact_")
		if err != nil {
			return "", fmt.Errorf("generate Workload change artifact ID: %w", err)
		}
	}
	outcome, err := transaction.Within(ctx, h.Pool, h.Policy, func(ctx context.Context, tx pgx.Tx) (ResultOutcome, error) {
		var attemptTenant domain.TenantID
		var attemptRun domain.WorkloadRunID
		var generation int64
		var planeID domain.ResourcePlaneID
		var attemptState string
		if err := tx.QueryRow(ctx, `SELECT tenant_id, workload_run_id, generation, resource_plane_id, state
			FROM workload_run_attempts WHERE id = $1`, result.AttemptID).Scan(&attemptTenant, &attemptRun, &generation, &planeID, &attemptState); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return "", ErrWorkloadResultMismatch
			}
			return "", fmt.Errorf("load Workload result attempt: %w", err)
		}
		if attemptTenant != result.TenantID || attemptRun != result.WorkloadRunID || planeID != result.ResourcePlaneID {
			return "", ErrWorkloadResultMismatch
		}
		if generation != result.Generation {
			return ResultStale, nil
		}
		var currentAttemptID string
		var currentGeneration int64
		if err := tx.QueryRow(ctx, `SELECT id, generation FROM workload_run_attempts
			WHERE tenant_id = $1 AND workload_run_id = $2 ORDER BY generation DESC LIMIT 1`,
			result.TenantID, result.WorkloadRunID).Scan(&currentAttemptID, &currentGeneration); err != nil {
			return "", fmt.Errorf("load current Workload attempt: %w", err)
		}
		if currentAttemptID != result.AttemptID || currentGeneration != result.Generation {
			return ResultStale, nil
		}
		run, err := workloads.Get(ctx, tx, result.TenantID, result.WorkloadRunID)
		if err != nil {
			return "", err
		}
		if run.ResourcePlaneID != result.ResourcePlaneID || run.CommitSHA != result.CommitSHA ||
			result.ChangeBundle != nil && run.Kind != domain.WorkloadAgent {
			return "", ErrWorkloadResultMismatch
		}
		var proposalVerification *workloads.AgentProposalVerification
		if run.Kind == domain.WorkloadJob {
			binding, bindingErr := workloads.GetAgentProposalVerificationForVerifier(ctx, tx, run.TenantID, run.ID)
			if bindingErr == nil {
				if binding.VerifierAttemptID != result.AttemptID || binding.BaseCommitSHA != run.CommitSHA || result.ChangeBundle != nil {
					return "", ErrWorkloadResultMismatch
				}
				proposalVerification = &binding
			} else if !errors.Is(bindingErr, workloads.ErrNotFound) {
				return "", bindingErr
			}
		}
		if result.CorrelationID != run.CorrelationID {
			return "", ErrWorkloadResultMismatch
		}
		if isTerminalWorkloadState(run.State) {
			return ResultDuplicate, nil
		}
		if run.State != domain.WorkloadProvisioning && run.State != domain.WorkloadRunning {
			return "", ErrWorkloadResultMismatch
		}
		if attemptState == string(domain.WorkloadSucceeded) || attemptState == string(domain.WorkloadFailed) ||
			attemptState == string(domain.WorkloadCancelled) || attemptState == string(domain.WorkloadTimedOut) {
			return ResultDuplicate, nil
		}
		if attemptState != "accepted" && attemptState != "running" && attemptState != "dispatch_pending" {
			return "", ErrWorkloadResultMismatch
		}

		next := result.Status
		errorCode := result.ErrorCode
		if run.DesiredState == domain.WorkloadDesiredCancelled {
			next, errorCode = domain.WorkloadCancelled, "cancelled"
		} else if !completedAt.Before(run.TimeoutAt) {
			next, errorCode = domain.WorkloadTimedOut, "job_timeout"
		}
		if run.State == domain.WorkloadProvisioning {
			run, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID,
				RunID: run.ID, ExpectedRevision: run.Revision, NextState: domain.WorkloadRunning, OccurredAt: completedAt})
			if err != nil {
				return "", err
			}
		}
		if result.LogArtifact != nil {
			if err := workloads.AddArtifact(ctx, tx, domain.WorkloadArtifact{ID: artifactID, TenantID: run.TenantID,
				RunID: run.ID, Kind: "logs", StorageRef: result.LogArtifact.StorageRef,
				SizeBytes: result.LogArtifact.SizeBytes, SHA256: result.LogArtifact.SHA256,
				Redacted: result.LogArtifact.Redacted, CreatedAt: completedAt}); err != nil {
				return "", fmt.Errorf("record Workload log artifact: %w", err)
			}
		}
		if result.ChangeBundle != nil && next == domain.WorkloadSucceeded {
			if err := workloads.AddArtifact(ctx, tx, domain.WorkloadArtifact{ID: changeArtifactID, TenantID: run.TenantID,
				RunID: run.ID, Kind: "agent_changes", StorageRef: result.ChangeBundle.StorageRef,
				SizeBytes: result.ChangeBundle.SizeBytes, SHA256: result.ChangeBundle.SHA256,
				Redacted: false, CreatedAt: completedAt}); err != nil {
				return "", fmt.Errorf("record Workload Agent change artifact: %w", err)
			}
		}
		if len(result.PipelineResults) > 0 {
			for _, pipeline := range result.PipelineResults {
				if _, err := tx.Exec(ctx, `INSERT INTO workload_pipeline_results
					(tenant_id, workload_run_id, generation, pipeline_name, status, exit_code, error_code, completed_at)
					VALUES ($1,$2,$3,$4,$5,$6,NULLIF($7,''),$8)`, result.TenantID, result.WorkloadRunID,
					result.Generation, pipeline.Name, pipeline.Status, pipeline.ExitCode, pipeline.ErrorCode, completedAt); err != nil {
					return "", fmt.Errorf("persist Workload pipeline result: %w", err)
				}
			}
		}
		run, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: run.TenantID,
			RunID: run.ID, ExpectedRevision: run.Revision, NextState: next, ErrorCode: errorCode, OccurredAt: completedAt})
		if err != nil {
			return "", err
		}
		for _, pipeline := range result.PipelineResults {
			resultMetadata := domain.WorkloadPipelineResult{Name: pipeline.Name, Status: pipeline.Status, ExitCode: pipeline.ExitCode,
				ErrorCode: pipeline.ErrorCode, Annotations: pipeline.Annotations}
			if proposalVerification != nil {
				if err := workloads.RecordAgentProposalVerificationCheck(ctx, tx, *proposalVerification, result.Generation, resultMetadata, completedAt); err != nil {
					return "", err
				}
				continue // Never report a verifier result on the unmodified base commit.
			}
			if err := workloads.SyncGitHubPipelineCheck(ctx, tx, run, resultMetadata, completedAt); err != nil {
				return "", err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE workload_run_attempts SET state = $1, updated_at = $2
			, cleanup_confirmed_at = $2
			WHERE id = $3 AND tenant_id = $4 AND workload_run_id = $5 AND generation = $6
			AND state IN ('dispatch_pending','accepted','running')`, string(next), completedAt,
			result.AttemptID, result.TenantID, result.WorkloadRunID, result.Generation); err != nil {
			return "", fmt.Errorf("finish Workload attempt: %w", err)
		}
		if next == domain.WorkloadSucceeded && result.ChangeBundle != nil && run.Kind == domain.WorkloadAgent {
			if err := enqueueAgentProposalVerifier(ctx, tx, run, result.AttemptID, changeArtifactID, result.ChangeBundle.SHA256, completedAt); err != nil {
				return "", err
			}
		}
		return ResultApplied, nil
	})
	if err == nil && outcome == ResultApplied {
		slog.InfoContext(ctx, "applied Hako Workload result", "correlation_id", result.CorrelationID,
			"tenant_id", result.TenantID, "workload_run_id", result.WorkloadRunID,
			"attempt_id", result.AttemptID, "generation", result.Generation, "status", result.Status)
	}
	return outcome, err
}

func enqueueAgentProposalVerifier(ctx context.Context, tx pgx.Tx, agent domain.WorkloadRun, agentAttemptID, artifactID, proposalSHA256 string, now time.Time) error {
	if ctx == nil || tx == nil || agent.Kind != domain.WorkloadAgent || agent.State != domain.WorkloadSucceeded ||
		agent.ResourcePlaneID == "" || agent.GitHubInstallation <= 0 || agent.GitHubRepository <= 0 ||
		!resultCommitSHAPattern.MatchString(agent.CommitSHA) || !resultSHA256Pattern.MatchString(proposalSHA256) ||
		agentAttemptID == "" || artifactID == "" || now.IsZero() {
		return ErrWorkloadResultMismatch
	}
	var defaultBranch string
	err := tx.QueryRow(ctx, `SELECT default_branch FROM tenant_github_repositories
		WHERE tenant_id=$1 AND installation_id=$2 AND github_repository_id=$3 AND factory_enabled=true`,
		agent.TenantID, agent.GitHubInstallation, agent.GitHubRepository).Scan(&defaultBranch)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // Factory opt-in is a deliberate default-off scheduling gate.
	}
	if err != nil {
		return fmt.Errorf("resolve Agent proposal verifier policy: %w", err)
	}
	if defaultBranch == "" || len(defaultBranch) > 255 || strings.ContainsAny(defaultBranch, "\r\n\x00") {
		return ErrWorkloadResultMismatch
	}
	keyDigest := sha256.Sum256([]byte(string(agent.TenantID) + "\x00" + string(agent.ID) + "\x00" + agentAttemptID + "\x00" + proposalSHA256))
	verifierKey := "agent-proposal-verify-" + hex.EncodeToString(keyDigest[:])
	runID, err := idgen.New("run_")
	if err != nil {
		return fmt.Errorf("generate Agent proposal verifier Run ID: %w", err)
	}
	verifier := domain.WorkloadRun{ID: domain.WorkloadRunID(runID), TenantID: agent.TenantID, Kind: domain.WorkloadJob,
		RequestedBy: agent.RequestedBy, GitHubInstallation: agent.GitHubInstallation, GitHubRepository: agent.GitHubRepository,
		GitHubIntegrationID: agent.GitHubIntegrationID, GitHubConfigRevision: agent.GitHubConfigRevision,
		GitHubCredentialGeneration: agent.GitHubCredentialGeneration, CorrelationID: agent.CorrelationID,
		CommitSHA: agent.CommitSHA, Ref: "refs/heads/" + defaultBranch, TriggerEvent: "pull_request", TriggerBranch: defaultBranch,
		RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, ResourcePlaneID: agent.ResourcePlaneID,
		TimeoutSeconds: int64((30 * time.Minute) / time.Second), IdempotencyKey: verifierKey, CreatedAt: now.UTC()}
	stored, created, err := workloads.CreateOrGet(ctx, tx, verifier)
	if err != nil {
		return fmt.Errorf("create Agent proposal verifier Run: %w", err)
	}
	if !created || stored.ID != verifier.ID || stored.Kind != domain.WorkloadJob || stored.CommitSHA != agent.CommitSHA ||
		stored.ResourcePlaneID != agent.ResourcePlaneID || stored.GitHubInstallation != agent.GitHubInstallation ||
		stored.GitHubRepository != agent.GitHubRepository || stored.TriggerEvent != "pull_request" || stored.TriggerBranch != defaultBranch {
		return ErrWorkloadResultMismatch
	}
	attemptID, err := idgen.New("attempt_")
	if err != nil {
		return fmt.Errorf("generate Agent proposal verifier attempt ID: %w", err)
	}
	token, err := idgen.New("token_")
	if err != nil {
		return fmt.Errorf("generate Agent proposal verifier idempotency token: %w", err)
	}
	dispatchEventID, err := idgen.New("evt_")
	if err != nil {
		return fmt.Errorf("generate Agent proposal verifier dispatch event ID: %w", err)
	}
	stored, err = workloads.Transition(ctx, tx, workloads.TransitionInput{TenantID: stored.TenantID, RunID: stored.ID,
		ExpectedRevision: stored.Revision, NextState: domain.WorkloadProvisioning, ResourcePlaneID: agent.ResourcePlaneID, OccurredAt: now})
	if err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO workload_run_attempts
		(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		VALUES ($1,$2,$3,1,1,$4,$5,'dispatch_pending',$6,$6)`, attemptID, stored.TenantID, stored.ID, agent.ResourcePlaneID, token, now.UTC()); err != nil {
		return fmt.Errorf("persist Agent proposal verifier attempt: %w", err)
	}
	command := DispatchCommand{SchemaVersion: 1, TenantID: stored.TenantID, WorkloadRunID: stored.ID,
		AttemptID: attemptID, Generation: 1, ResourcePlane: agent.ResourcePlaneID, IdempotencyToken: token,
		CorrelationID: stored.CorrelationID}
	payload, err := json.Marshal(command)
	if err != nil {
		return fmt.Errorf("encode Agent proposal verifier dispatch: %w", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO outbox_events
		(id,aggregate_type,aggregate_id,event_type,payload_json,created_at,published_at,attempt)
		VALUES ($1,'workload_attempt',$2,'workload_attempt.dispatch',$3,$4,NULL,0)`,
		dispatchEventID, attemptID, string(payload), now.UTC()); err != nil {
		return fmt.Errorf("enqueue Agent proposal verifier dispatch: %w", err)
	}
	if err := workloads.BindAgentProposalVerification(ctx, tx, workloads.AgentProposalVerification{
		TenantID: agent.TenantID, AgentRunID: agent.ID, AgentAttemptID: agentAttemptID, ProposalArtifactID: artifactID,
		ProposalSHA256: proposalSHA256, BaseCommitSHA: agent.CommitSHA, VerifierRunID: stored.ID, VerifierAttemptID: attemptID, CreatedAt: now.UTC(),
	}); err != nil {
		return fmt.Errorf("bind Agent proposal to verifier Run: %w", err)
	}
	if err := workloads.SuppressGitHubChecksForRun(ctx, tx, stored.TenantID, stored.ID); err != nil {
		return err
	}
	return nil
}

// ValidateResult checks metadata before a trusted RP publisher persists/sends it.
func ValidateResult(result WorkloadResult) error {
	return validateWorkloadResult(result)
}

func validateWorkloadResult(result WorkloadResult) error {
	if result.SchemaVersion != 1 || result.TenantID == "" || result.WorkloadRunID == "" ||
		result.AttemptID == "" || result.Generation < 1 || result.ResourcePlaneID == "" ||
		result.CompletedAt.IsZero() {
		return fmt.Errorf("%w: identity, version, generation, and completion time are required", ErrWorkloadResultInvalid)
	}
	if result.CorrelationID != "" && !resultCorrelationIDPattern.MatchString(result.CorrelationID) {
		return fmt.Errorf("%w: correlation ID is invalid", ErrWorkloadResultInvalid)
	}
	if result.Status != domain.WorkloadSucceeded && result.Status != domain.WorkloadFailed &&
		result.Status != domain.WorkloadCancelled && result.Status != domain.WorkloadTimedOut {
		return fmt.Errorf("%w: status must be terminal", ErrWorkloadResultInvalid)
	}
	if result.Status == domain.WorkloadSucceeded && (result.ExitCode != 0 || result.ErrorCode != "") {
		return fmt.Errorf("%w: successful result must have exit code zero and no error code", ErrWorkloadResultInvalid)
	}
	if result.Status == domain.WorkloadFailed && result.ErrorCode == "" && result.ExitCode == 0 {
		return fmt.Errorf("%w: failed result requires a nonzero exit code or safe error code", ErrWorkloadResultInvalid)
	}
	if result.ErrorCode != "" && !resultErrorCodePattern.MatchString(result.ErrorCode) {
		return fmt.Errorf("%w: error code is not a safe identifier", ErrWorkloadResultInvalid)
	}
	if len(result.PipelineResults) > 16 {
		return fmt.Errorf("%w: pipeline result count exceeds its bound", ErrWorkloadResultInvalid)
	}
	previousPipeline := ""
	for _, pipeline := range result.PipelineResults {
		if !pipelineNamePattern.MatchString(pipeline.Name) || pipeline.Name <= previousPipeline || pipeline.ExitCode < 0 || pipeline.ExitCode > 255 {
			return fmt.Errorf("%w: pipeline results must have unique sorted names and bounded exit codes", ErrWorkloadResultInvalid)
		}
		if len(pipeline.Annotations) > githubapp.MaxCheckAnnotations {
			return fmt.Errorf("%w: pipeline Check annotation count exceeds its bound", ErrWorkloadResultInvalid)
		}
		for _, annotation := range pipeline.Annotations {
			if err := githubapp.ValidateCheckAnnotation(githubapp.CheckAnnotation(annotation)); err != nil {
				return fmt.Errorf("%w: pipeline Check annotation is invalid", ErrWorkloadResultInvalid)
			}
		}
		annotationJSON, err := json.Marshal(pipeline.Annotations)
		if err != nil || len(annotationJSON) > 48<<10 {
			return fmt.Errorf("%w: pipeline Check annotations exceed their byte bound", ErrWorkloadResultInvalid)
		}
		switch pipeline.Status {
		case "succeeded":
			if pipeline.ExitCode != 0 || pipeline.ErrorCode != "" {
				return fmt.Errorf("%w: successful pipeline result must have zero exit code and no error", ErrWorkloadResultInvalid)
			}
		case "failed":
			if pipeline.ExitCode == 0 || pipeline.ErrorCode != "" && !resultErrorCodePattern.MatchString(pipeline.ErrorCode) {
				return fmt.Errorf("%w: failed pipeline result requires a bounded failure", ErrWorkloadResultInvalid)
			}
		case "cancelled", "timed_out":
			if pipeline.ExitCode == 0 || pipeline.ErrorCode != "" && !resultErrorCodePattern.MatchString(pipeline.ErrorCode) {
				return fmt.Errorf("%w: pipeline error code is invalid", ErrWorkloadResultInvalid)
			}
		case "skipped":
			if pipeline.ExitCode != 0 || pipeline.ErrorCode != "" {
				return fmt.Errorf("%w: skipped pipeline result cannot carry an exit failure", ErrWorkloadResultInvalid)
			}
		default:
			return fmt.Errorf("%w: unsupported pipeline result status", ErrWorkloadResultInvalid)
		}
		previousPipeline = pipeline.Name
	}
	if result.LogArtifact != nil {
		if result.LogArtifact.SizeBytes < 0 || result.LogArtifact.SizeBytes > 100<<20 ||
			len(result.LogArtifact.SHA256) != 64 || !result.LogArtifact.Redacted {
			return fmt.Errorf("%w: log artifact metadata must be bounded, SHA-256 identified, and redacted", ErrWorkloadResultInvalid)
		}
		if _, err := hex.DecodeString(result.LogArtifact.SHA256); err != nil {
			return fmt.Errorf("%w: log artifact SHA-256 is invalid", ErrWorkloadResultInvalid)
		}
	}
	if result.ChangeBundle != nil {
		if result.Status != domain.WorkloadSucceeded || result.ChangeBundle.SizeBytes < 1 || result.ChangeBundle.SizeBytes > 8<<20 || len(result.ChangeBundle.SHA256) != 64 {
			return fmt.Errorf("%w: change bundle metadata must be bounded and SHA-256 identified", ErrWorkloadResultInvalid)
		}
		if _, err := hex.DecodeString(result.ChangeBundle.SHA256); err != nil {
			return fmt.Errorf("%w: change bundle SHA-256 is invalid", ErrWorkloadResultInvalid)
		}
	}
	return nil
}
