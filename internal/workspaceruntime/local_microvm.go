package workspaceruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

// LocalMicroVMDriver is the host lifecycle boundary for a local Cloud Hypervisor
// implementation. Every method must be idempotent for the supplied allocation
// ID. EnsureRunning must attach the same persistent Workspace disk on retries
// and after Resume; Delete must stop the VM and remove only that allocation's
// owned resources.
type LocalMicroVMDriver interface {
	EnsureRunning(context.Context, string, []byte) error
	Suspend(context.Context, string) error
	Resume(context.Context, string, []byte) error
	Delete(context.Context, string) error
}

// LocalLaunchSpecResolver returns trusted, host-local launch JSON. Operation
// payloads never supply kernel, initrd, rootfs, or disk paths.
type LocalLaunchSpecResolver interface {
	ResolveLocal(context.Context, resourcecontroller.Command) ([]byte, error)
}

// LocalMicroVM applies Resource Controller operations to a host-local VM
// driver. It shares the same durable revision/tombstone journal as the Lambda
// runtime, so command ordering does not change with provider selection.
type LocalMicroVM struct {
	driver   LocalMicroVMDriver
	store    workspacejournal.Store
	locker   workspacejournal.OperationLocker
	resolver LocalLaunchSpecResolver
	now      func() time.Time
}

// LocalReconcileResult reports one durable allocation's reconciliation
// outcome. Errors are per-allocation so one damaged VM does not hide healthy
// allocations later in the bounded listing.
type LocalReconcileResult struct {
	TenantID        domain.TenantID
	WorkspaceID     domain.WorkspaceID
	ResourcePlaneID domain.ResourcePlaneID
	Observed        domain.ObservedWorkspaceState
	Err             error
}

type LocalReconcileReporter func([]LocalReconcileResult, error)

// RunLocalReconcileLoop performs a full reconciliation immediately, then
// repeats it at the requested interval. Per-allocation failures are reported
// without stopping healthy allocations or the next pass; discovery failures
// are reported and retried on the following pass.
func RunLocalReconcileLoop(ctx context.Context, runtime *LocalMicroVM, interval time.Duration, limit int, report LocalReconcileReporter) error {
	if ctx == nil || runtime == nil || interval < 100*time.Millisecond || interval > 10*time.Minute || limit < 1 || limit > 100000 {
		return errors.New("local Workspace reconcile loop configuration is invalid")
	}
	for {
		results, err := runtime.Reconcile(ctx, limit)
		if report != nil {
			report(results, err)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func NewLocalMicroVM(driver LocalMicroVMDriver, store workspacejournal.Store, resolver LocalLaunchSpecResolver, now func() time.Time) (*LocalMicroVM, error) {
	if driver == nil || store == nil || resolver == nil {
		return nil, errors.New("local MicroVM driver, journal store, and trusted launch resolver are required")
	}
	locker, ok := store.(workspacejournal.OperationLocker)
	if !ok {
		return nil, errors.New("local MicroVM journal store must serialize Workspace operations across processes")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &LocalMicroVM{driver: driver, store: store, locker: locker, resolver: resolver, now: now}, nil
}

func (r *LocalMicroVM) Execute(ctx context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	if r == nil || ctx == nil || command.SchemaVersion != 2 || command.WorkspaceRevision < 1 ||
		command.TenantID == "" || command.WorkspaceID == "" || command.ResourcePlaneID == "" || command.OperationID == "" {
		return domain.ObservedWorkspaceFailed, ErrSpecification
	}
	unlock, err := r.locker.LockOperation(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil {
		return domain.ObservedWorkspaceFailed, err
	}
	defer unlock()
	identity := workspacejournal.Command{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: command.WorkspaceRevision,
		OperationID: command.OperationID, Type: command.Type}
	snapshot, err := r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if errors.Is(err, workspacejournal.ErrNotFound) {
		var initial workspacejournal.Journal
		switch command.Type {
		case domain.OperationEnsureRunning:
			payload, resolveErr := r.resolver.ResolveLocal(ctx, command)
			if resolveErr != nil {
				return domain.ObservedWorkspaceFailed, resolveErr
			}
			initial, err = workspacejournal.NewWithLaunchSpec(identity, localLaunchToken(identity, payload), payload, r.now())
		case domain.OperationDelete:
			initial, err = workspacejournal.New(identity, "", "", r.now())
		default:
			return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
		}
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		snapshot, err = r.store.Create(ctx, initial)
		if errors.Is(err, workspacejournal.ErrConflict) {
			snapshot, err = r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
		}
	}
	if err != nil {
		return domain.ObservedWorkspaceFailed, err
	}
	if snapshot.ETag == "" || snapshot.Journal.Validate() != nil || !matchesLocalIdentity(snapshot.Journal, command) {
		return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
	}

	var payload []byte
	var token string
	if command.Type == domain.OperationEnsureRunning && snapshot.Journal.State == workspacejournal.Terminated {
		payload, err = r.resolver.ResolveLocal(ctx, command)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		token = localLaunchToken(identity, payload)
	} else if command.Type == domain.OperationEnsureRunning && snapshot.Journal.WorkspaceRevision == command.WorkspaceRevision &&
		snapshot.Journal.OperationID == command.OperationID && snapshot.Journal.OperationType == command.Type {
		payload = append([]byte(nil), snapshot.Journal.LaunchSpecPayload...)
		token = snapshot.Journal.LaunchToken
	}
	next, applyErr := workspacejournal.ApplyCommandWithLaunchSpec(snapshot, identity, token, payload, r.now())
	if errors.Is(applyErr, workspacejournal.ErrDuplicate) {
		applyErr = nil
	} else if applyErr == nil {
		snapshot, err = r.store.CompareAndSwap(ctx, snapshot, next)
		if err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
	}
	if applyErr != nil {
		return domain.ObservedWorkspaceFailed, applyErr
	}
	return r.reconcile(ctx, snapshot)
}

func (r *LocalMicroVM) ResourceBinding(ctx context.Context, command resourcecontroller.Command) (string, string, int64, error) {
	snapshot, err := r.store.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil {
		return "", "", 0, err
	}
	j := snapshot.Journal
	if snapshot.ETag == "" || j.Validate() != nil || !matchesLocalIdentity(j, command) ||
		j.WorkspaceRevision != command.WorkspaceRevision || j.OperationID != command.OperationID ||
		j.OperationType != command.Type || j.ProviderID == "" || j.Tombstone {
		return "", "", 0, workspacejournal.ErrConflict
	}
	return "cloud_hypervisor_local", j.ProviderID, j.AllocationGeneration, nil
}

// Reconcile loads a bounded durable allocation listing and re-reads every
// record under its process-shared operation lock before applying host state.
// The listing is only a discovery snapshot; the locked read is authoritative.
func (r *LocalMicroVM) Reconcile(ctx context.Context, limit int) ([]LocalReconcileResult, error) {
	if r == nil || ctx == nil || limit < 1 {
		return nil, errors.New("local Workspace reconciliation context and positive limit are required")
	}
	lister, ok := r.store.(interface {
		ListJournals(context.Context, int) ([]workspacejournal.Snapshot, error)
	})
	if !ok {
		return nil, errors.New("local Workspace journal store does not support bounded reconciliation listing")
	}
	discovered, err := lister.ListJournals(ctx, limit)
	if err != nil {
		return nil, err
	}
	results := make([]LocalReconcileResult, 0, len(discovered))
	for _, listed := range discovered {
		j := listed.Journal
		result := LocalReconcileResult{TenantID: j.TenantID, WorkspaceID: j.WorkspaceID, ResourcePlaneID: j.ResourcePlaneID}
		unlock, lockErr := r.locker.LockOperation(ctx, j.TenantID, j.WorkspaceID, j.ResourcePlaneID)
		if lockErr != nil {
			result.Err = lockErr
			result.Observed = domain.ObservedWorkspaceFailed
			results = append(results, result)
			if ctx.Err() != nil {
				return results, ctx.Err()
			}
			continue
		}
		current, getErr := r.store.Get(ctx, j.TenantID, j.WorkspaceID, j.ResourcePlaneID)
		if getErr != nil {
			result.Err = getErr
		} else if current.ETag == "" || current.Journal.Validate() != nil || !sameLocalJournalIdentity(current.Journal, j) {
			result.Err = workspacejournal.ErrConflict
		} else {
			result.Observed, result.Err = r.reconcile(ctx, current)
		}
		unlock()
		if result.Err != nil {
			result.Observed = domain.ObservedWorkspaceFailed
		}
		results = append(results, result)
	}
	return results, nil
}

func (r *LocalMicroVM) reconcile(ctx context.Context, snapshot workspacejournal.Snapshot) (domain.ObservedWorkspaceState, error) {
	j := snapshot.Journal
	if j.State == workspacejournal.Deleted {
		return domain.ObservedWorkspaceDeleted, nil
	}
	if j.State == workspacejournal.Failed && !j.Tombstone {
		return domain.ObservedWorkspaceFailed, errors.New("local Workspace MicroVM is in a failed state")
	}
	if j.Tombstone {
		if j.State == workspacejournal.Failed && j.CleanupState == workspacejournal.CleanupFailed {
			var err error
			snapshot, err = r.advanceJournal(ctx, snapshot, workspacejournal.Deleting, j.ProviderID, workspacejournal.CleanupPending)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			j = snapshot.Journal
		}
		if j.State != workspacejournal.Deleting {
			return domain.ObservedWorkspaceFailed, workspacejournal.ErrConflict
		}
		// The runtime checkpoints a deterministic provider ID before asking the
		// driver to create anything. A tombstone without one therefore means a
		// crash occurred before the VM could exist.
		if j.ProviderID == "" {
			return r.advance(ctx, snapshot, workspacejournal.Deleted, "", workspacejournal.CleanupConfirmed, domain.ObservedWorkspaceDeleted)
		}
		if err := r.driver.Delete(ctx, j.ProviderID); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return r.advance(ctx, snapshot, workspacejournal.Deleted, j.ProviderID, workspacejournal.CleanupConfirmed, domain.ObservedWorkspaceDeleted)
	}
	if j.State == workspacejournal.Provisioning {
		if j.ProviderID == "" {
			providerID := localProviderID(j)
			var err error
			snapshot, err = r.advanceJournal(ctx, snapshot, workspacejournal.Provisioning, providerID, j.CleanupState)
			if err != nil {
				return domain.ObservedWorkspaceFailed, err
			}
			j = snapshot.Journal
		}
		if err := r.driver.EnsureRunning(ctx, j.ProviderID, append([]byte(nil), j.LaunchSpecPayload...)); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return r.advance(ctx, snapshot, workspacejournal.Running, j.ProviderID, j.CleanupState, domain.ObservedWorkspaceRunning)
	}
	if j.State == workspacejournal.Suspending {
		if err := r.driver.Suspend(ctx, j.ProviderID); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return r.advance(ctx, snapshot, workspacejournal.Suspended, j.ProviderID, j.CleanupState, domain.ObservedWorkspaceSuspended)
	}
	if j.State == workspacejournal.Resuming {
		if err := r.driver.Resume(ctx, j.ProviderID, append([]byte(nil), j.LaunchSpecPayload...)); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return r.advance(ctx, snapshot, workspacejournal.Running, j.ProviderID, j.CleanupState, domain.ObservedWorkspaceRunning)
	}
	switch j.State {
	case workspacejournal.Running:
		if err := r.driver.EnsureRunning(ctx, j.ProviderID, append([]byte(nil), j.LaunchSpecPayload...)); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return domain.ObservedWorkspaceRunning, nil
	case workspacejournal.Suspended:
		if err := r.driver.Suspend(ctx, j.ProviderID); err != nil {
			return domain.ObservedWorkspaceFailed, err
		}
		return domain.ObservedWorkspaceSuspended, nil
	case workspacejournal.Terminated:
		// A provider-terminated allocation is left stopped until an explicit
		// ensure (new generation) or delete Operation arrives.
		return "", nil
	default:
		return domain.ObservedWorkspaceFailed, fmt.Errorf("unsupported local Workspace journal state %q", j.State)
	}
}

func (r *LocalMicroVM) advance(ctx context.Context, snapshot workspacejournal.Snapshot, state workspacejournal.State, providerID string,
	cleanup workspacejournal.CleanupState, observed domain.ObservedWorkspaceState) (domain.ObservedWorkspaceState, error) {
	_, err := r.advanceJournal(ctx, snapshot, state, providerID, cleanup)
	if err != nil {
		return domain.ObservedWorkspaceFailed, err
	}
	return observed, nil
}

func (r *LocalMicroVM) advanceJournal(ctx context.Context, snapshot workspacejournal.Snapshot, state workspacejournal.State, providerID string,
	cleanup workspacejournal.CleanupState) (workspacejournal.Snapshot, error) {
	next, err := workspacejournal.Advance(snapshot, state, providerID, cleanup, r.now())
	if err != nil {
		return workspacejournal.Snapshot{}, err
	}
	return r.store.CompareAndSwap(ctx, snapshot, next)
}

func localLaunchToken(command workspacejournal.Command, payload []byte) string {
	identity := strings.Join([]string{string(command.ResourcePlaneID), string(command.TenantID), string(command.WorkspaceID),
		fmt.Sprint(command.WorkspaceRevision), string(command.OperationID), workspacejournal.DigestLaunchSpec(payload)}, "\x00")
	digest := sha256.Sum256([]byte(identity))
	return "launch_" + hex.EncodeToString(digest[:16])
}

func localProviderID(j workspacejournal.Journal) string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%d", j.ResourcePlaneID, j.TenantID, j.WorkspaceID, j.AllocationGeneration)
	digest := sha256.Sum256([]byte(identity))
	return "ch_" + hex.EncodeToString(digest[:16])
}

func matchesLocalIdentity(j workspacejournal.Journal, command resourcecontroller.Command) bool {
	return j.TenantID == command.TenantID && j.WorkspaceID == command.WorkspaceID && j.ResourcePlaneID == command.ResourcePlaneID
}

func sameLocalJournalIdentity(a, b workspacejournal.Journal) bool {
	return a.TenantID == b.TenantID && a.WorkspaceID == b.WorkspaceID && a.ResourcePlaneID == b.ResourcePlaneID
}

var _ resourcecontroller.Runtime = (*LocalMicroVM)(nil)
var _ resourcecontroller.ResourceBindingReporter = (*LocalMicroVM)(nil)
