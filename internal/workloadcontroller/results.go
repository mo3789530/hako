package workloadcontroller

import (
	"context"
	"encoding/json"
	"net/url"
	"strings"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type ResultPublisher interface {
	Publish(context.Context, workloadjobs.WorkloadResult) error
}

// Poll checkpoints immutable metadata before send. A crash after send but
// before CAS replays the same result for CP fencing/deduplication. There is no
// deployable queue recovery scheduler or production provider implied here.
func (c *Controller) Poll(ctx context.Context, data []byte, publisher ResultPublisher) (workloadjournal.Snapshot, error) {
	if publisher == nil {
		return workloadjournal.Snapshot{}, ErrInvalidCommand
	}
	s, err := c.acceptor.Accept(ctx, data)
	if err != nil {
		return s, err
	}
	if s.Journal.ExecutionState == workloadjournal.Accepted || s.Journal.ExecutionState == workloadjournal.Launching {
		return c.Ensure(ctx, data)
	}
	if !s.Journal.ResultPublished && len(s.Journal.ResultPayload) == 0 {
		id := identity(s.Journal)
		spec, err := c.resolveSpecification(ctx, id, s.Journal)
		if err != nil {
			return s, err
		}
		hash, err := specificationHash(spec, id)
		if err != nil {
			return s, err
		}
		if s.Journal.SpecificationHash != "" && s.Journal.SpecificationHash != hash {
			return s, ErrSpecification
		}
		if !terminal(s.Journal.ExecutionState) && !c.acceptor.now().Before(spec.Deadline) {
			s, err = c.stop(ctx, data, workloadjournal.TimedOut)
			if err != nil {
				return s, err
			}
		}
		var result workloadjobs.WorkloadResult
		if terminal(s.Journal.ExecutionState) {
			if s.Journal.ExecutionState != workloadjournal.Cancelled && s.Journal.ExecutionState != workloadjournal.TimedOut {
				return s, ErrSpecification
			}
			result = workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(id.TenantID), WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID),
				AttemptID: id.AttemptID, Generation: id.Generation, ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID), CommitSHA: spec.CommitSHA,
				CorrelationID: spec.CorrelationID,
				Status:        domain.WorkloadRunState(s.Journal.ExecutionState), CompletedAt: c.acceptor.now().UTC(), ErrorCode: string(s.Journal.ExecutionState)}
		} else {
			state, err := c.runtime.GetStatus(ctx, id)
			if err != nil {
				return s, err
			}
			if state == workloadjournal.Running {
				return s, nil
			}
			if !terminal(state) {
				return s, ErrSpecification
			}
			result, err = c.runtime.CollectResult(ctx, id)
			if err != nil {
				return s, err
			}
			if workloadjournal.ExecutionState(result.Status) != state {
				return s, ErrSpecification
			}
		}
		if err := validateResult(result, spec); err != nil {
			return s, err
		}
		payload, err := json.Marshal(result)
		if err != nil {
			return s, ErrSpecification
		}
		next := s.Journal
		next.ExecutionState = workloadjournal.ExecutionState(result.Status)
		next.CleanupState = workloadjournal.CleanupPending
		if terminal(s.Journal.ExecutionState) {
			next.CleanupState = s.Journal.CleanupState
		}
		next.ResultPayload = payload
		s, err = c.update(ctx, s, next)
		if err != nil {
			return s, err
		}
	}
	if !s.Journal.ResultPublished {
		var result workloadjobs.WorkloadResult
		if json.Unmarshal(s.Journal.ResultPayload, &result) != nil || workloadjobs.ValidateResult(result) != nil {
			return s, ErrSpecification
		}
		if err := publisher.Publish(ctx, result); err != nil {
			return s, err
		}
		next := s.Journal
		next.ResultPublished = true
		s, err = c.update(ctx, s, next)
		if err != nil {
			return s, err
		}
	}
	return c.cleanup(ctx, s)
}

func validateResult(r workloadjobs.WorkloadResult, s Specification) error {
	id := s.Identity
	if workloadjobs.ValidateResult(r) != nil || string(r.TenantID) != id.TenantID || string(r.WorkloadRunID) != id.WorkloadRunID ||
		r.AttemptID != id.AttemptID || r.Generation != id.Generation || string(r.ResourcePlaneID) != id.ResourcePlaneID ||
		r.CommitSHA != s.CommitSHA || r.CorrelationID != s.CorrelationID {
		return ErrSpecification
	}
	if r.LogArtifact != nil {
		ref, err := url.Parse(r.LogArtifact.StorageRef)
		prefix, perr := url.Parse(s.ArtifactPrefix)
		if err != nil || perr != nil || ref.Scheme != "s3" || ref.Host == "" || prefix.Scheme != "s3" || prefix.Host != ref.Host ||
			ref.RawQuery != "" || ref.Fragment != "" || ref.User != nil || ref.RawPath != "" || prefix.RawQuery != "" || prefix.Fragment != "" || prefix.User != nil || prefix.RawPath != "" ||
			!strings.HasSuffix(prefix.Path, "/"+id.TenantID+"/"+id.WorkloadRunID+"/"+id.AttemptID+"/") ||
			!strings.HasPrefix(ref.Path, prefix.Path) || strings.Contains(ref.Path, "..") || strings.ContainsAny(ref.Path, "\\\x00") {
			return ErrSpecification
		}
	}
	if (r.Status == domain.WorkloadSucceeded || r.Status == domain.WorkloadFailed) && !r.CompletedAt.Before(s.Deadline) {
		return ErrSpecification
	}
	return nil
}
