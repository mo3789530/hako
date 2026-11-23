package workloadjournal

import (
	"context"
	"errors"
	"time"
)

var (
	ErrAttemptLeaseHeld = errors.New("Workload attempt lease is held")
	ErrAttemptLeaseLost = errors.New("Workload attempt lease is no longer owned")
)

type AttemptLease struct {
	Command   Command
	Owner     string
	ExpiresAt time.Time
}

// AcquireAttemptLease claims the journal row using its existing CAS boundary.
// Invocation, cancellation, and cleanup callers must use this same lease so
// those actions cannot overlap. The owner must be unique per worker lifetime.
func AcquireAttemptLease(ctx context.Context, store Store, command Command, owner string,
	now time.Time, ttl time.Duration) (AttemptLease, Snapshot, error) {
	if err := validateAttemptLeaseArgs(ctx, store, command, owner, now, ttl); err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	current, err := store.Get(ctx, command.TenantID, command.WorkloadRunID, command.AttemptID)
	if err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	if !matchesCommand(current.Journal, command) || current.ETag == "" || current.Journal.Validate() != nil ||
		current.Journal.ResultPublished {
		return AttemptLease{}, Snapshot{}, ErrConflict
	}
	if current.Journal.AttemptLeaseOwner != "" && current.Journal.AttemptLeaseExpires.After(now.UTC()) {
		return AttemptLease{}, Snapshot{}, ErrAttemptLeaseHeld
	}
	next := current.Journal
	next.AttemptLeaseOwner = owner
	next.AttemptLeaseExpires = now.UTC().Add(ttl)
	next.Revision++
	next.UpdatedAt = now.UTC()
	if ValidateUpdate(current, next) != nil {
		return AttemptLease{}, Snapshot{}, ErrConflict
	}
	claimed, err := store.CompareAndSwap(ctx, current, next)
	if err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	return AttemptLease{Command: command, Owner: owner, ExpiresAt: next.AttemptLeaseExpires}, claimed, nil
}

// RenewAttemptLease extends an unexpired lease without depending on the
// journal revision captured at acquisition, since invocation and result
// checkpoints legitimately advance that revision while the lease is held.
func RenewAttemptLease(ctx context.Context, store Store, lease AttemptLease, now time.Time, ttl time.Duration) (AttemptLease, Snapshot, error) {
	if err := validateAttemptLeaseArgs(ctx, store, lease.Command, lease.Owner, now, ttl); err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	current, err := store.Get(ctx, lease.Command.TenantID, lease.Command.WorkloadRunID, lease.Command.AttemptID)
	if err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	if !matchesCommand(current.Journal, lease.Command) || current.ETag == "" || current.Journal.Validate() != nil ||
		current.Journal.AttemptLeaseOwner != lease.Owner || !current.Journal.AttemptLeaseExpires.Equal(lease.ExpiresAt) ||
		!current.Journal.AttemptLeaseExpires.After(now.UTC()) {
		return AttemptLease{}, Snapshot{}, ErrAttemptLeaseLost
	}
	next := current.Journal
	next.AttemptLeaseExpires = now.UTC().Add(ttl)
	next.Revision++
	next.UpdatedAt = now.UTC()
	if ValidateUpdate(current, next) != nil {
		return AttemptLease{}, Snapshot{}, ErrConflict
	}
	renewed, err := store.CompareAndSwap(ctx, current, next)
	if err != nil {
		return AttemptLease{}, Snapshot{}, err
	}
	return AttemptLease{Command: lease.Command, Owner: lease.Owner, ExpiresAt: next.AttemptLeaseExpires}, renewed, nil
}

// ReleaseAttemptLease expires the owner's lease through a journal CAS. Keeping
// the owner for diagnostics lets the next acquisition safely fence it by CAS.
func ReleaseAttemptLease(ctx context.Context, store Store, lease AttemptLease, now time.Time) error {
	if ctx == nil || ctx.Err() != nil || store == nil || !validCommand(lease.Command) ||
		!idPattern.MatchString(lease.Owner) || now.IsZero() {
		return ErrAttemptLeaseLost
	}
	current, err := store.Get(ctx, lease.Command.TenantID, lease.Command.WorkloadRunID, lease.Command.AttemptID)
	if err != nil {
		return err
	}
	if !matchesCommand(current.Journal, lease.Command) || current.ETag == "" || current.Journal.Validate() != nil ||
		current.Journal.AttemptLeaseOwner != lease.Owner || !current.Journal.AttemptLeaseExpires.Equal(lease.ExpiresAt) {
		return ErrAttemptLeaseLost
	}
	next := current.Journal
	next.AttemptLeaseExpires = now.UTC()
	next.Revision++
	next.UpdatedAt = now.UTC()
	if ValidateUpdate(current, next) != nil {
		return ErrAttemptLeaseLost
	}
	if _, err := store.CompareAndSwap(ctx, current, next); err != nil {
		return ErrAttemptLeaseLost
	}
	return nil
}

func AttemptLeaseHeld(snapshot Snapshot, lease AttemptLease, now time.Time) bool {
	return snapshot.ETag != "" && snapshot.Journal.Validate() == nil && validCommand(lease.Command) &&
		matchesCommand(snapshot.Journal, lease.Command) && snapshot.Journal.AttemptLeaseOwner == lease.Owner &&
		snapshot.Journal.AttemptLeaseExpires.Equal(lease.ExpiresAt) && lease.ExpiresAt.After(now.UTC())
}

func validateAttemptLeaseArgs(ctx context.Context, store Store, command Command, owner string, now time.Time, ttl time.Duration) error {
	if ctx == nil || store == nil || !validCommand(command) || !idPattern.MatchString(owner) || now.IsZero() ||
		ttl < 5*time.Second || ttl > 30*time.Minute {
		return ErrAttemptLeaseLost
	}
	return ctx.Err()
}

func validCommand(command Command) bool {
	return idPattern.MatchString(command.TenantID) && idPattern.MatchString(command.WorkloadRunID) &&
		idPattern.MatchString(command.AttemptID) && idPattern.MatchString(command.ResourcePlaneID) &&
		idPattern.MatchString(command.IdempotencyToken) && command.Generation > 0
}

func matchesCommand(j Journal, command Command) bool {
	return j.TenantID == command.TenantID && j.WorkloadRunID == command.WorkloadRunID &&
		j.AttemptID == command.AttemptID && j.Generation == command.Generation &&
		j.ResourcePlaneID == command.ResourcePlaneID && j.IdempotencyToken == command.IdempotencyToken
}
