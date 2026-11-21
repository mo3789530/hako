package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var (
	ErrSpecification  = errors.New("invalid or changed Workload launch specification")
	ErrCleanupPending = errors.New("Workload physical cleanup is pending")
	digestPattern     = regexp.MustCompile(`^[a-f0-9]{64}$`)
	commitPattern     = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
)

// Specification is supplied by an authenticated trusted resolver, not the
// command message. The resolver owns authorization and source staging.
type Specification struct {
	Identity                 workloadjournal.Command
	Kind                     domain.WorkloadKind
	ImageDigest              string
	SourceDigest             string
	CommitSHA                string
	GitHubIntegrationID      string
	GitHubInstallationID     int64
	GitHubRepositoryID       int64
	AgentTaskContextSHA256   string
	TriggerEvent             string
	TriggerBranch            string
	CorrelationID            string
	Deadline                 time.Time
	Predecessor              *workloadjournal.Command
	ArtifactPrefix           string
	RequestedAt              time.Time
	ImageIdentifier          string
	ImageVersion             string
	JobMemoryLimitMB         int
	JobCPULimitMillis        int
	IngressNetworkConnectors []string
	EgressNetworkConnectors  []string
}

type Resolver interface {
	Resolve(context.Context, workloadjournal.Command) (Specification, error)
}

// MicroVM is a required provider contract, not an implementation of an AWS API.
// EnsureRunning must atomically deduplicate by the complete identity and frozen
// specification. Cancel must persist a fence against concurrent/future launches.
// Cleanup returning true must prove no allocation or in-flight launch remains;
// success cannot merely mean that a delete request was sent.
type MicroVM interface {
	EnsureRunning(context.Context, Specification) (string, error)
	Cancel(context.Context, workloadjournal.Command) error
	Cleanup(context.Context, workloadjournal.Command) (bool, error)
	GetStatus(context.Context, workloadjournal.Command) (workloadjournal.ExecutionState, error)
	CollectResult(context.Context, workloadjournal.Command) (workloadjobs.WorkloadResult, error)
}

type Controller struct {
	acceptor *Acceptor
	resolver Resolver
	runtime  MicroVM
}

func NewController(acceptor *Acceptor, resolver Resolver, runtime MicroVM) (*Controller, error) {
	if acceptor == nil || resolver == nil || runtime == nil {
		return nil, ErrInvalidCommand
	}
	return &Controller{acceptor: acceptor, resolver: resolver, runtime: runtime}, nil
}

// Ensure performs one reconciliation step using the caller's context. Re-delivery recovers a
// launching checkpoint using exactly the same identity/specification. There is
// deliberately no queue acknowledgement or production provider here yet.
func (c *Controller) Ensure(ctx context.Context, data []byte) (workloadjournal.Snapshot, error) {
	s, err := c.acceptor.Accept(ctx, data)
	if err != nil {
		return s, err
	}
	if terminal(s.Journal.ExecutionState) || s.Journal.CancelRequested {
		return c.cleanup(ctx, s)
	}
	id := identity(s.Journal)
	// Ensure re-resolves even a launching specification so an authorization or
	// source-binding drift prevents an uncertain/retried launch from proceeding.
	spec, err := c.resolver.Resolve(ctx, id)
	if err != nil {
		return s, err
	}
	hash, err := specificationHash(spec, id)
	if err != nil {
		return s, err
	}
	if spec.Predecessor != nil {
		previous, err := c.load(ctx, *spec.Predecessor)
		if err != nil {
			return s, err
		}
		if previous.Journal.CleanupState != workloadjournal.CleanupConfirmed {
			return s, ErrCleanupPending
		}
	}
	if s.Journal.SpecificationHash != "" && s.Journal.SpecificationHash != hash {
		return s, ErrSpecification
	}
	if validator, ok := c.runtime.(interface{ ValidateSpecification(Specification) error }); ok {
		if err := validator.ValidateSpecification(spec); err != nil {
			return s, err
		}
	}
	if !c.acceptor.now().Before(spec.Deadline) {
		return c.stop(ctx, data, workloadjournal.TimedOut)
	}
	if s.Journal.ExecutionState == workloadjournal.Running {
		return s, nil
	}
	if s.Journal.ExecutionState == workloadjournal.Accepted {
		next := s.Journal
		next.ExecutionState = workloadjournal.Launching
		next.SpecificationHash = hash
		frozen := spec
		frozen.Deadline = frozen.Deadline.UTC()
		frozen.RequestedAt = frozen.RequestedAt.UTC()
		next.SpecificationPayload, err = json.Marshal(frozen)
		if err != nil {
			return s, ErrSpecification
		}
		s, err = c.update(ctx, s, next)
		if err != nil {
			return s, err
		}
	}
	// Old launching records without a frozen spec cannot safely be re-launched.
	if s.Journal.SpecificationHash != hash {
		return s, ErrSpecification
	}
	if err := ctx.Err(); err != nil {
		return s, err
	}
	handle, err := c.runtime.EnsureRunning(ctx, spec)
	if err != nil {
		return s, err
	}
	latest, err := c.load(ctx, id)
	if err != nil {
		return s, err
	}
	if latest.Journal.CancelRequested || terminal(latest.Journal.ExecutionState) {
		return c.cleanup(ctx, latest)
	}
	if !c.acceptor.now().Before(spec.Deadline) {
		return c.stop(ctx, data, workloadjournal.TimedOut)
	}
	next := latest.Journal
	next.RuntimeHandle = handle
	next.ExecutionState = workloadjournal.Running
	if handle == "" {
		return latest, ErrSpecification
	}
	return c.update(ctx, latest, next)
}

func (c *Controller) Cancel(ctx context.Context, data []byte) (workloadjournal.Snapshot, error) {
	return c.stop(ctx, data, workloadjournal.Cancelled)
}

// HandleEvent uses the trusted command-channel event type (SQS HakoEventType),
// not an action supplied by repository code. It is not an acknowledgement API.
func (c *Controller) HandleEvent(ctx context.Context, eventType string, data []byte) (workloadjournal.Snapshot, error) {
	switch eventType {
	case "workload_attempt.dispatch":
		return c.Ensure(ctx, data)
	case "workload_attempt.cancel":
		return c.Cancel(ctx, data)
	default:
		return workloadjournal.Snapshot{}, ErrInvalidCommand
	}
}

func (c *Controller) stop(ctx context.Context, data []byte, state workloadjournal.ExecutionState) (workloadjournal.Snapshot, error) {
	s, err := c.acceptor.Accept(ctx, data)
	if err != nil {
		return s, err
	}
	if !terminal(s.Journal.ExecutionState) {
		next := s.Journal
		next.CancelRequested = true
		next.ExecutionState = state
		next.CleanupState = workloadjournal.CleanupPending
		s, err = c.update(ctx, s, next)
		if err != nil {
			return s, err
		}
	}
	return c.cleanup(ctx, s)
}

// RetryCleanup can be driven by a durable journal recovery scheduler. It does
// not start replacement attempts or infer deletion from logical terminal state.
func (c *Controller) RetryCleanup(ctx context.Context, data []byte) (workloadjournal.Snapshot, error) {
	s, err := c.acceptor.Accept(ctx, data)
	if err != nil {
		return s, err
	}
	if !terminal(s.Journal.ExecutionState) {
		return s, ErrCleanupPending
	}
	return c.cleanup(ctx, s)
}

func (c *Controller) cleanup(ctx context.Context, s workloadjournal.Snapshot) (workloadjournal.Snapshot, error) {
	if s.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		return s, nil
	}
	if !terminal(s.Journal.ExecutionState) {
		return s, ErrCleanupPending
	}
	id := identity(s.Journal)
	if s.Journal.CancelRequested {
		if err := c.runtime.Cancel(ctx, id); err != nil {
			return c.cleanupFailure(ctx, s, err)
		}
	}
	confirmed, err := c.runtime.Cleanup(ctx, id)
	if err != nil {
		return c.cleanupFailure(ctx, s, err)
	}
	if !confirmed {
		return s, ErrCleanupPending
	}
	// Provider recovery may have persisted a previously unknown runtime handle.
	// Refresh that checkpoint before marking physical cleanup confirmed.
	s, err = c.load(ctx, id)
	if err != nil {
		return s, err
	}
	if s.Journal.CleanupState == workloadjournal.CleanupConfirmed {
		return s, nil
	}
	next := s.Journal
	next.CleanupState = workloadjournal.CleanupConfirmed
	return c.update(ctx, s, next)
}

func (c *Controller) cleanupFailure(ctx context.Context, s workloadjournal.Snapshot, cause error) (workloadjournal.Snapshot, error) {
	latest, err := c.load(ctx, identity(s.Journal))
	if err != nil {
		return s, errors.Join(cause, err)
	}
	s = latest
	if s.Journal.CleanupState == workloadjournal.CleanupFailed {
		return s, cause
	}
	next := s.Journal
	next.CleanupState = workloadjournal.CleanupFailed
	updated, err := c.update(ctx, s, next)
	return updated, errors.Join(cause, err)
}

func (c *Controller) update(ctx context.Context, s workloadjournal.Snapshot, next workloadjournal.Journal) (workloadjournal.Snapshot, error) {
	next.Revision = s.Journal.Revision + 1
	next.UpdatedAt = c.acceptor.now().UTC()
	if err := workloadjournal.ValidateUpdate(s, next); err != nil {
		return s, err
	}
	updated, err := c.acceptor.store.CompareAndSwap(ctx, s, next)
	if err != nil {
		return s, err
	}
	return updated, nil
}

func (c *Controller) load(ctx context.Context, id workloadjournal.Command) (workloadjournal.Snapshot, error) {
	s, err := c.acceptor.store.Get(ctx, id.TenantID, id.WorkloadRunID, id.AttemptID)
	if err != nil {
		return s, err
	}
	if s.ETag == "" || s.Journal.Validate() != nil || identity(s.Journal) != id {
		return s, workloadjournal.ErrConflict
	}
	return s, nil
}

// resolveSpecification uses the immutable launch payload for an already
// frozen attempt. Ensure intentionally keeps using the trusted resolver for
// launch/relaunch so source or authorization drift fails closed.
func (c *Controller) resolveSpecification(ctx context.Context, id workloadjournal.Command, journal workloadjournal.Journal) (Specification, error) {
	if len(journal.SpecificationPayload) == 0 {
		return c.resolver.Resolve(ctx, id)
	}
	decoder := json.NewDecoder(bytes.NewReader(journal.SpecificationPayload))
	decoder.DisallowUnknownFields()
	var spec Specification
	if decoder.Decode(&spec) != nil || decoder.Decode(new(any)) != io.EOF || spec.Identity != id {
		return Specification{}, ErrSpecification
	}
	hash, err := specificationHash(spec, id)
	if err != nil || hash != journal.SpecificationHash {
		return Specification{}, ErrSpecification
	}
	return spec, nil
}

func identity(j workloadjournal.Journal) workloadjournal.Command {
	return workloadjournal.Command{TenantID: j.TenantID, WorkloadRunID: j.WorkloadRunID, AttemptID: j.AttemptID,
		Generation: j.Generation, ResourcePlaneID: j.ResourcePlaneID, IdempotencyToken: j.IdempotencyToken}
}

func specificationHash(s Specification, id workloadjournal.Command) (string, error) {
	if s.Identity != id || !digestPattern.MatchString(s.ImageDigest) || !digestPattern.MatchString(s.SourceDigest) ||
		!commitPattern.MatchString(s.CommitSHA) || s.Deadline.IsZero() ||
		(s.Kind == domain.WorkloadAgent && (!metadataIDPattern.MatchString(s.GitHubIntegrationID) || s.GitHubInstallationID <= 0 ||
			s.GitHubRepositoryID <= 0 || !digestPattern.MatchString(s.AgentTaskContextSHA256))) ||
		(s.Kind != domain.WorkloadAgent && (s.GitHubIntegrationID != "" || s.GitHubInstallationID != 0 || s.GitHubRepositoryID != 0 || s.AgentTaskContextSHA256 != "")) ||
		(s.Kind != "" && s.Kind != domain.WorkloadJob && s.Kind != domain.WorkloadAgent) ||
		!validTriggerSelection(effectiveSpecificationKind(s.Kind), s.TriggerEvent, s.TriggerBranch) {
		return "", ErrSpecification
	}
	if (s.JobMemoryLimitMB == 0) != (s.JobCPULimitMillis == 0) ||
		(s.JobMemoryLimitMB != 0 && (effectiveSpecificationKind(s.Kind) != domain.WorkloadJob || s.JobMemoryLimitMB < 256 ||
			s.JobMemoryLimitMB > 16384 || s.JobCPULimitMillis < 250 || s.JobCPULimitMillis > 8000)) ||
		(s.TriggerEvent != "" && effectiveSpecificationKind(s.Kind) == domain.WorkloadJob && (s.JobMemoryLimitMB == 0 || s.JobCPULimitMillis == 0)) {
		return "", ErrSpecification
	}
	if id.Generation == 1 && s.Predecessor != nil {
		return "", ErrSpecification
	}
	if id.Generation > 1 {
		p := s.Predecessor
		if p == nil || p.TenantID != id.TenantID || p.WorkloadRunID != id.WorkloadRunID || p.ResourcePlaneID != id.ResourcePlaneID ||
			p.Generation != id.Generation-1 || p.AttemptID == id.AttemptID {
			return "", ErrSpecification
		}
	}
	s.Deadline = s.Deadline.UTC()
	s.RequestedAt = s.RequestedAt.UTC()
	b, err := json.Marshal(s)
	if err != nil {
		return "", ErrSpecification
	}
	digest := sha256.Sum256(b)
	return hex.EncodeToString(digest[:]), nil
}

func effectiveSpecificationKind(kind domain.WorkloadKind) domain.WorkloadKind {
	if kind == "" {
		return domain.WorkloadJob
	}
	return kind
}

func validTriggerSelection(kind domain.WorkloadKind, event, branch string) bool {
	if event == "" && branch == "" {
		return kind == domain.WorkloadJob
	}
	if kind == domain.WorkloadJob && event != "push" && event != "pull_request" ||
		kind == domain.WorkloadAgent && event != "issue_comment" ||
		kind != domain.WorkloadJob && kind != domain.WorkloadAgent || len(branch) == 0 || len(branch) > 255 ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") ||
		strings.Contains(branch, "@{") {
		return false
	}
	for _, char := range branch {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}

func terminal(s workloadjournal.ExecutionState) bool {
	return s == workloadjournal.Succeeded || s == workloadjournal.Failed || s == workloadjournal.Cancelled || s == workloadjournal.TimedOut
}
