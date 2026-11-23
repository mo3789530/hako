package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

var ErrAgentCoreWorker = errors.New("AgentCore Workload worker could not safely complete the attempt")

type AgentCoreInvocationInput struct {
	Specification Specification
	Request       workloadagentcore.InvocationRequest
}

// AgentCoreInvocationResolver materializes request bytes from trusted,
// attempt-pinned Workload/source state. Queue payloads are not an authority.
type AgentCoreInvocationResolver interface {
	ResolveAgentCoreInvocation(context.Context, workloadjournal.Command) (AgentCoreInvocationInput, error)
}

type AgentCoreWorkloadWorkerConfig struct {
	Journal   workloadjournal.Store
	Inputs    AgentCoreInvocationResolver
	Invoker   workloadagentcore.RuntimeInvoker
	Stopper   workloadagentcore.RuntimeSessionStopper
	Artifacts workloadagent.AttemptChangeBundleWriter
	Publisher ResultPublisher
	LeaseTTL  time.Duration
	// LeaseHeartbeatInterval defaults to one third of LeaseTTL. Tests may set
	// a shorter interval to exercise failure fencing without waiting minutes.
	LeaseHeartbeatInterval time.Duration
	Now                    func() time.Time
}

// AgentCoreWorkloadWorker composes one synchronous Runtime invocation with the
// existing Workload result and cleanup fence. It is provider-agnostic and
// makes no AWS calls during construction; the deployment must still keep this
// worker default-off until the IAM/isolation gates pass.
type AgentCoreWorkloadWorker struct {
	config AgentCoreWorkloadWorkerConfig
}

func NewAgentCoreWorkloadWorker(config AgentCoreWorkloadWorkerConfig) (*AgentCoreWorkloadWorker, error) {
	if config.Journal == nil || config.Inputs == nil || config.Invoker == nil || config.Stopper == nil || config.Publisher == nil {
		return nil, ErrAgentCoreWorker
	}
	if config.LeaseTTL == 0 {
		config.LeaseTTL = 20 * time.Minute
	}
	// The invocation API permits up to 15 minutes. Keep a bounded safety margin
	// in the lease for response handling, result checkpoint, stop and publish.
	if config.LeaseTTL < 20*time.Minute || config.LeaseTTL > 30*time.Minute {
		return nil, ErrAgentCoreWorker
	}
	if config.LeaseHeartbeatInterval == 0 {
		config.LeaseHeartbeatInterval = config.LeaseTTL / 3
	}
	if config.LeaseHeartbeatInterval < 10*time.Millisecond || config.LeaseHeartbeatInterval >= config.LeaseTTL/2 {
		return nil, ErrAgentCoreWorker
	}
	if config.Now == nil {
		config.Now = func() time.Time { return time.Now().UTC() }
	}
	return &AgentCoreWorkloadWorker{config: config}, nil
}

// Run processes or recovers one attempt. Once a journal is Running it never
// invokes again; it records a bounded failure and proceeds to stop/cleanup.
// The same journal CAS lease fences invocation, stop, result checkpoint and
// publication. A caller must use this entrypoint for cancellation work too.
func (w *AgentCoreWorkloadWorker) Run(ctx context.Context, command workloadjournal.Command) (snapshot workloadjournal.Snapshot, err error) {
	if w == nil || ctx == nil || ctx.Err() != nil || !validAgentCoreCommand(command) {
		return workloadjournal.Snapshot{}, ErrAgentCoreWorker
	}
	current, err := w.config.Journal.Get(ctx, command.TenantID, command.WorkloadRunID, command.AttemptID)
	if err != nil || current.ETag == "" || current.Journal.Validate() != nil || !matchesAgentCoreCommand(current.Journal, command) {
		return workloadjournal.Snapshot{}, ErrAgentCoreWorker
	}
	if current.Journal.ResultPublished {
		return current, nil
	}
	owner, err := newAgentCoreLeaseOwner()
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	lease, current, err := workloadjournal.AcquireAttemptLease(ctx, w.config.Journal, command, owner, w.config.Now(), w.config.LeaseTTL)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	leaseState := &agentCoreLeaseState{lease: lease}
	defer func() {
		releaseCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if releaseErr := workloadjournal.ReleaseAttemptLease(releaseCtx, w.config.Journal, leaseState.current(), w.config.Now()); err == nil && releaseErr != nil {
			err = ErrAgentCoreWorker
		}
	}()
	workCtx, cancelWork := context.WithCancel(ctx)
	heartbeatErr, heartbeatDone := w.startLeaseHeartbeat(workCtx, cancelWork, leaseState)
	current, runErr := w.runLeased(workCtx, command, leaseState, current)
	cancelWork()
	<-heartbeatDone
	select {
	case <-heartbeatErr:
		return current, ErrAgentCoreWorker
	default:
	}
	return current, runErr
}

func (w *AgentCoreWorkloadWorker) startLeaseHeartbeat(ctx context.Context, cancel context.CancelFunc,
	leaseState *agentCoreLeaseState) (<-chan struct{}, <-chan struct{}) {
	failed := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(w.config.LeaseHeartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				renewCtx, renewCancel := context.WithTimeout(ctx, 5*time.Second)
				_, err := leaseState.renew(renewCtx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
				renewCancel()
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					close(failed)
					cancel()
					return
				}
			}
		}
	}()
	return failed, done
}

func (w *AgentCoreWorkloadWorker) runLeased(ctx context.Context, command workloadjournal.Command,
	leaseState *agentCoreLeaseState, current workloadjournal.Snapshot) (workloadjournal.Snapshot, error) {
	spec, err := decodeAgentCoreWorkloadSpecification(current.Journal)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	if current.Journal.ExecutionState == workloadjournal.Launching {
		var result workloadjobs.WorkloadResult
		switch {
		case current.Journal.CancelRequested:
			result = agentCoreFailureResult(spec, domain.WorkloadCancelled, "cancelled", w.config.Now())
		case !w.config.Now().Before(spec.Deadline):
			result = agentCoreFailureResult(spec, domain.WorkloadTimedOut, "job_timeout", w.config.Now())
		default:
			resolved, resolveErr := w.config.Inputs.ResolveAgentCoreInvocation(ctx, command)
			resolvedHash, specErr := specificationHash(resolved.Specification, command)
			if resolveErr != nil || specErr != nil || resolvedHash != current.Journal.SpecificationHash || resolved.Request.Command != command {
				return current, ErrAgentCoreWorker
			}
			current, err = leaseState.renew(ctx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
			if err != nil {
				return current, ErrAgentCoreWorker
			}
			claimed, draft, invokeErr := workloadagentcore.InvokeOnce(ctx, w.config.Journal, current, leaseState.current(),
				resolved.Request, w.config.Invoker, w.config.Now())
			if invokeErr != nil {
				if claimed.ETag == "" || claimed.Journal.ExecutionState != workloadjournal.Running {
					return current, ErrAgentCoreWorker
				}
				result = agentCoreFailureResult(spec, domain.WorkloadFailed, "agentcore_invocation_unknown", w.config.Now())
				if !w.config.Now().Before(spec.Deadline) {
					result = agentCoreFailureResult(spec, domain.WorkloadTimedOut, "job_timeout", w.config.Now())
				}
				current = claimed
			} else {
				result, err = AgentCoreWorkloadResult(ctx, spec, resolved.Request, draft, w.config.Artifacts, w.config.Now())
				if err != nil {
					result = agentCoreFailureResult(spec, domain.WorkloadFailed, "agentcore_artifact_failed", w.config.Now())
				}
				if !w.config.Now().Before(spec.Deadline) {
					result = agentCoreFailureResult(spec, domain.WorkloadTimedOut, "job_timeout", w.config.Now())
				}
				current = claimed
			}
		}
		current, err = leaseState.renew(ctx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
		if err != nil {
			return current, ErrAgentCoreWorker
		}
		current, err = w.checkpointTerminalResult(ctx, current, spec, result)
		if err != nil {
			return current, ErrAgentCoreWorker
		}
	} else if current.Journal.ExecutionState == workloadjournal.Running {
		// A previous worker claimed InvokeAgentRuntime but did not durably
		// checkpoint a response. The result cannot be reconstructed safely.
		result := agentCoreFailureResult(spec, domain.WorkloadFailed, "agentcore_invocation_unknown", w.config.Now())
		if !w.config.Now().Before(spec.Deadline) {
			result = agentCoreFailureResult(spec, domain.WorkloadTimedOut, "job_timeout", w.config.Now())
		}
		current, err = leaseState.renew(ctx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
		if err != nil {
			return current, ErrAgentCoreWorker
		}
		current, err = w.checkpointTerminalResult(ctx, current, spec, result)
		if err != nil {
			return current, ErrAgentCoreWorker
		}
	} else if !agentCoreTerminal(current.Journal.ExecutionState) {
		return current, ErrAgentCoreWorker
	}

	result, err := decodeAgentCoreWorkloadResult(current.Journal, spec)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	current, err = leaseState.renew(ctx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	current, err = workloadagentcore.StopAndConfirmCleanup(ctx, w.config.Journal, current, leaseState.current(), w.config.Stopper, w.config.Now())
	if err != nil || current.Journal.CleanupState != workloadjournal.CleanupConfirmed {
		return current, ErrAgentCoreWorker
	}
	current, err = leaseState.renew(ctx, w.config.Journal, w.config.Now(), w.config.LeaseTTL)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	if err := w.config.Publisher.Publish(ctx, result); err != nil {
		return current, ErrAgentCoreWorker
	}
	next := current.Journal
	next.ResultPublished = true
	next.Revision++
	next.UpdatedAt = w.config.Now().UTC()
	if workloadjournal.ValidateUpdate(current, next) != nil {
		return current, ErrAgentCoreWorker
	}
	published, err := w.config.Journal.CompareAndSwap(ctx, current, next)
	if err != nil {
		return current, ErrAgentCoreWorker
	}
	return published, nil
}

type agentCoreLeaseState struct {
	mu    sync.Mutex
	lease workloadjournal.AttemptLease
}

func (s *agentCoreLeaseState) current() workloadjournal.AttemptLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lease
}

func (s *agentCoreLeaseState) renew(ctx context.Context, store workloadjournal.Store, now time.Time,
	ttl time.Duration) (workloadjournal.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for attempt := 0; attempt < 3; attempt++ {
		lease, snapshot, err := workloadjournal.RenewAttemptLease(ctx, store, s.lease, now, ttl)
		if err == nil {
			s.lease = lease
			return snapshot, nil
		}
		if !errors.Is(err, workloadjournal.ErrConflict) || attempt == 2 {
			return workloadjournal.Snapshot{}, err
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return workloadjournal.Snapshot{}, ctx.Err()
		case <-timer.C:
		}
	}
	return workloadjournal.Snapshot{}, ErrAgentCoreWorker
}

func (w *AgentCoreWorkloadWorker) checkpointTerminalResult(ctx context.Context, current workloadjournal.Snapshot,
	spec Specification, result workloadjobs.WorkloadResult) (workloadjournal.Snapshot, error) {
	if workloadjobs.ValidateResult(result) != nil || validateResult(result, spec) != nil || result.TenantID != domain.TenantID(current.Journal.TenantID) ||
		result.WorkloadRunID != domain.WorkloadRunID(current.Journal.WorkloadRunID) || result.AttemptID != current.Journal.AttemptID ||
		result.Generation != current.Journal.Generation || result.ResourcePlaneID != domain.ResourcePlaneID(current.Journal.ResourcePlaneID) {
		return current, ErrAgentCoreWorker
	}
	payload, err := json.Marshal(result)
	if err != nil || len(payload) > 32<<10 {
		return current, ErrAgentCoreWorker
	}
	next := current.Journal
	next.ExecutionState = workloadjournal.ExecutionState(result.Status)
	next.CleanupState = workloadjournal.CleanupPending
	next.ResultPayload = payload
	next.Revision++
	next.UpdatedAt = w.config.Now().UTC()
	if workloadjournal.ValidateUpdate(current, next) != nil {
		return current, ErrAgentCoreWorker
	}
	return w.config.Journal.CompareAndSwap(ctx, current, next)
}

func decodeAgentCoreWorkloadSpecification(j workloadjournal.Journal) (Specification, error) {
	if j.Validate() != nil || len(j.SpecificationPayload) == 0 || j.SpecificationHash == "" {
		return Specification{}, ErrSpecification
	}
	decoder := json.NewDecoder(bytes.NewReader(j.SpecificationPayload))
	decoder.DisallowUnknownFields()
	var spec Specification
	if decoder.Decode(&spec) != nil || decoder.Decode(new(any)) != io.EOF {
		return Specification{}, ErrSpecification
	}
	hash, err := specificationHash(spec, identity(j))
	if err != nil || hash != j.SpecificationHash || spec.Kind != domain.WorkloadAgent {
		return Specification{}, ErrSpecification
	}
	return spec, nil
}

func decodeAgentCoreWorkloadResult(j workloadjournal.Journal, spec Specification) (workloadjobs.WorkloadResult, error) {
	if len(j.ResultPayload) == 0 {
		return workloadjobs.WorkloadResult{}, ErrAgentCoreWorker
	}
	var result workloadjobs.WorkloadResult
	if json.Unmarshal(j.ResultPayload, &result) != nil || validateResult(result, spec) != nil ||
		workloadjournal.ExecutionState(result.Status) != j.ExecutionState {
		return workloadjobs.WorkloadResult{}, ErrAgentCoreWorker
	}
	return result, nil
}

func agentCoreFailureResult(spec Specification, status domain.WorkloadRunState, code string, completedAt time.Time) workloadjobs.WorkloadResult {
	return workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(spec.Identity.TenantID),
		WorkloadRunID: domain.WorkloadRunID(spec.Identity.WorkloadRunID), AttemptID: spec.Identity.AttemptID,
		Generation: spec.Identity.Generation, ResourcePlaneID: domain.ResourcePlaneID(spec.Identity.ResourcePlaneID),
		CorrelationID: spec.CorrelationID, CommitSHA: spec.CommitSHA, Status: status, ExitCode: 1,
		ErrorCode: code, CompletedAt: completedAt.UTC()}
}

func agentCoreTerminal(state workloadjournal.ExecutionState) bool {
	switch state {
	case workloadjournal.Succeeded, workloadjournal.Failed, workloadjournal.Cancelled, workloadjournal.TimedOut:
		return true
	default:
		return false
	}
}

func validAgentCoreCommand(command workloadjournal.Command) bool {
	_, err := workloadagentcore.SessionID(command)
	return err == nil
}

func matchesAgentCoreCommand(j workloadjournal.Journal, command workloadjournal.Command) bool {
	return identity(j) == command
}

func newAgentCoreLeaseOwner() (string, error) {
	var entropy [16]byte
	if _, err := rand.Read(entropy[:]); err != nil {
		return "", err
	}
	return "agentcore-" + hex.EncodeToString(entropy[:]), nil
}
