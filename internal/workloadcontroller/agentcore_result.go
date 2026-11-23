package workloadcontroller

import (
	"context"
	"errors"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadsource"
)

// AgentCoreWorkloadResult converts a strictly decoded Runtime draft into the
// existing Resource Plane result contract. The ordinary Controller persists
// that result, confirms Runtime cleanup, and only then publishes it to the
// Control Plane. A successful return here is never verifier evidence.
func AgentCoreWorkloadResult(ctx context.Context, spec Specification, input workloadagentcore.InvocationRequest,
	draft workloadagentcore.InvocationDraft, artifacts workloadagent.AttemptChangeBundleWriter,
	completedAt time.Time) (workloadjobs.WorkloadResult, error) {
	if ctx == nil || ctx.Err() != nil || spec.Kind != domain.WorkloadAgent || spec.Identity != input.Command ||
		spec.GitHubRepositoryID != input.RepositoryID || spec.CommitSHA != input.CommitSHA || spec.SourceDigest != input.SourceSHA256 ||
		input.Command.TenantID != draft.Attempt.TenantID || input.Command.WorkloadRunID != draft.Attempt.RunID ||
		input.Command.AttemptID != draft.Attempt.AttemptID || input.Command.ResourcePlaneID != draft.Attempt.ResourcePlaneID ||
		input.Command.Generation != draft.Attempt.Generation || input.RepositoryID != draft.Attempt.RepositoryID ||
		input.CommitSHA != draft.Attempt.CommitSHA || input.SourceSHA256 != draft.Attempt.SourceSHA256 ||
		completedAt.IsZero() || !completedAt.Before(spec.Deadline) || draft.Iterations < 0 || draft.Iterations > 10 {
		return workloadjobs.WorkloadResult{}, ErrSpecification
	}
	if _, err := workloadagentcore.EncodeInvocationRequest(input); err != nil {
		return workloadjobs.WorkloadResult{}, ErrSpecification
	}
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(input.Command.TenantID),
		WorkloadRunID: domain.WorkloadRunID(input.Command.WorkloadRunID), AttemptID: input.Command.AttemptID,
		Generation: input.Command.Generation, ResourcePlaneID: domain.ResourcePlaneID(input.Command.ResourcePlaneID),
		CorrelationID: spec.CorrelationID, CommitSHA: input.CommitSHA, Status: domain.WorkloadSucceeded,
		ExitCode: 0, CompletedAt: completedAt.UTC()}
	switch draft.Status {
	case "no_proposal":
		if draft.ProposalSHA != "" || len(draft.ProposalData) != 0 {
			return workloadjobs.WorkloadResult{}, ErrSpecification
		}
	case "proposal_ready":
		if artifacts == nil || len(draft.ProposalData) == 0 {
			return workloadjobs.WorkloadResult{}, ErrSpecification
		}
		inspection, err := workloadsource.InspectAgentProposalArchive(draft.ProposalData)
		if err != nil || inspection.SHA256 != draft.ProposalSHA {
			return workloadjobs.WorkloadResult{}, ErrSpecification
		}
		artifact, err := artifacts.PutAgentChangeBundle(ctx, input.Command, draft.ProposalData, draft.ProposalSHA)
		if err != nil || artifact.SizeBytes != int64(len(draft.ProposalData)) || artifact.SHA256 != draft.ProposalSHA {
			return workloadjobs.WorkloadResult{}, errors.New("AgentCore proposal artifact persistence failed")
		}
		result.ChangeBundle = &artifact
	default:
		return workloadjobs.WorkloadResult{}, ErrSpecification
	}
	if err := validateResult(result, spec); err != nil {
		return workloadjobs.WorkloadResult{}, ErrSpecification
	}
	return result, nil
}
