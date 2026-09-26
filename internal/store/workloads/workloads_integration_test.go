//go:build integration

package workloads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWorkloadRunIdempotencyTransitionsTimeoutAndArtifacts(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_workload_test', 'Workload test', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_workload_test', 'workload-test-sub', '', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations (tenant_id, installation_id, account_login, status, requested_by, requested_at, updated_at)
		VALUES ('tenant_workload_test', 9201, 'acme', 'active', 'usr_workload_test', $1, $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id, tenant_id, bound_at) VALUES (9201, 'tenant_workload_test', $1)`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id, installation_id, github_repository_id, owner_login, repository_name, default_branch, synchronized_at)
		VALUES ('tenant_workload_test', 9201, 9202, 'acme', 'api', 'main', $1)`, now); err != nil {
		t.Fatal(err)
	}
	policy := transaction.DefaultPolicy()
	type creation struct {
		run     domain.WorkloadRun
		created bool
	}
	run := domain.WorkloadRun{ID: "run_workload_test", TenantID: "tenant_workload_test", Kind: domain.WorkloadJob,
		RequestedBy: "usr_workload_test", IdempotencyKey: "github-delivery-1", RuntimeClass: "standard",
		GitHubInstallation: 9201, GitHubRepository: 9202, CommitSHA: strings.Repeat("a", 40),
		TrustLevel: domain.WorkloadUntrusted, TimeoutAt: now.Add(2 * time.Minute), CreatedAt: now}
	createdRun, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, run)
		return creation{run: stored, created: created}, err
	})
	stored, created := createdRun.run, createdRun.created
	if err != nil || !created || stored.State != domain.WorkloadPending || stored.Revision != 1 {
		t.Fatalf("create run = %+v created=%v err=%v", stored, created, err)
	}
	retry := run
	retry.ID = "run_should_not_be_created"
	createdRun, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, retry)
		return creation{run: stored, created: created}, err
	})
	stored, created = createdRun.run, createdRun.created
	if err != nil || created || stored.ID != run.ID {
		t.Fatalf("idempotent retry = %+v created=%v err=%v", stored, created, err)
	}
	conflict := retry
	conflict.TrustLevel = domain.WorkloadTrusted
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, _, err := CreateOrGet(ctx, tx, conflict)
		return struct{}{}, err
	}); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("different request with same key error = %v", err)
	}
	events, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadRunEvent, error) {
		return ListEvents(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(events) != 1 || events[0].Type != "workload.pending" {
		t.Fatalf("creation events = %+v err=%v", events, err)
	}
	stored, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return RequestCancel(ctx, tx, run.TenantID, run.ID, now.Add(time.Second))
	})
	if err != nil || stored.DesiredState != domain.WorkloadDesiredCancelled || stored.Revision != 2 {
		t.Fatalf("cancel request = %+v err=%v", stored, err)
	}
	_, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return RequestCancel(ctx, tx, run.TenantID, run.ID, now.Add(2*time.Second))
	})
	if err != nil {
		t.Fatalf("repeated cancel request: %v", err)
	}
	completed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
			ExpectedRevision: 2, NextState: domain.WorkloadCancelled, OccurredAt: now.Add(3 * time.Second)})
	})
	if err != nil || completed.State != domain.WorkloadCancelled || completed.CompletedAt == nil || completed.Revision != 3 {
		t.Fatalf("cancel transition = %+v err=%v", completed, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
			ExpectedRevision: 3, NextState: domain.WorkloadRunning, OccurredAt: now.Add(4 * time.Second)})
	}); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("terminal run transition error = %v", err)
	}
	unredacted := domain.WorkloadArtifact{ID: "artifact_bad_log", TenantID: run.TenantID, RunID: run.ID,
		Kind: "logs", StorageRef: "s3://hako-artifacts/run/logs", SizeBytes: 17,
		SHA256: strings.Repeat("a", 64), CreatedAt: now.Add(5 * time.Second)}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, AddArtifact(ctx, tx, unredacted)
	}); !errors.Is(err, errLogsMustBeRedacted) {
		t.Fatalf("unredacted log write error = %v", err)
	}
	unredacted.Redacted = true
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, AddArtifact(ctx, tx, unredacted)
	}); err != nil {
		t.Fatalf("record redacted logs: %v", err)
	}
	artifacts, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return ListArtifacts(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(artifacts) != 1 || !artifacts[0].Redacted {
		t.Fatalf("Workload artifacts = %+v err=%v", artifacts, err)
	}
	timeoutRun := run
	timeoutRun.ID, timeoutRun.IdempotencyKey = "run_timeout_test", "github-delivery-2"
	timeoutRun.TimeoutAt = now.Add(10 * time.Second)
	createdRun, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, timeoutRun)
		return creation{run: stored, created: created}, err
	})
	if err != nil || !createdRun.created {
		t.Fatalf("create timeout run: created=%v err=%v", createdRun.created, err)
	}
	defaultRun := domain.WorkloadRun{ID: "run_default_timeout", TenantID: run.TenantID, Kind: domain.WorkloadJob,
		IdempotencyKey: "github-delivery-default-timeout", RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted}
	createdRun, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, defaultRun)
		return creation{run: stored, created: created}, err
	})
	if err != nil || !createdRun.created {
		t.Fatalf("create default-timeout run: created=%v err=%v", createdRun.created, err)
	}
	defaultRun.ID = "run_default_timeout_retry"
	createdRun, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, defaultRun)
		return creation{run: stored, created: created}, err
	})
	if err != nil || createdRun.created || createdRun.run.ID != "run_default_timeout" || createdRun.run.TimeoutSeconds != int64(DefaultTimeout/time.Second) {
		t.Fatalf("default-timeout retry: run=%+v created=%v err=%v", createdRun.run, createdRun.created, err)
	}
	stats, err := ExpireDue(ctx, pool, policy, now.Add(11*time.Second), 10)
	if err != nil || stats != (ExpireStats{Claimed: 1, TimedOut: 1}) {
		t.Fatalf("ExpireDue stats = %+v err=%v", stats, err)
	}
	timedOut, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Get(ctx, tx, timeoutRun.TenantID, timeoutRun.ID)
	})
	if err != nil || timedOut.State != domain.WorkloadTimedOut || timedOut.DesiredState != domain.WorkloadDesiredCancelled || timedOut.CompletedAt == nil {
		t.Fatalf("timed-out run = %+v err=%v", timedOut, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_installations SET suspended_at = $1 WHERE tenant_id = 'tenant_workload_test' AND installation_id = 9201`, now.Add(20*time.Second)); err != nil {
		t.Fatal(err)
	}
	// An idempotent retry still returns its original run, while a new run for
	// the same now-suspended Repository cannot be created.
	replayed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (creation, error) {
		stored, created, err := CreateOrGet(ctx, tx, run)
		return creation{run: stored, created: created}, err
	})
	if err != nil || replayed.created {
		t.Fatalf("idempotent run retry after suspend: created=%v err=%v", replayed.created, err)
	}
	blockedRun := run
	blockedRun.ID, blockedRun.IdempotencyKey = "run_suspended_repo", "github-delivery-suspended"
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, _, err := CreateOrGet(ctx, tx, blockedRun)
		return struct{}{}, err
	}); !errors.Is(err, ErrRepositoryInactive) {
		t.Fatalf("new run for suspended Repository error = %v", err)
	}
}
