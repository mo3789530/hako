//go:build integration

package workloads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/quotas"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestConcurrentWorkloadQuotaReservationAndTerminalRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_workload_quota','Workload quota',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_workload_quota','workload-quota-owner','',now())`); err != nil {
		t.Fatal(err)
	}
	const testRunLimit = 3
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, "tenant_workload_quota", "usr_workload_quota", quotas.DefaultWorkspaces, testRunLimit, quotas.DefaultArtifactBytes, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, time.Now().UTC())
	}); err != nil {
		t.Fatalf("configure Tenant Workload quota: %v", err)
	}
	type result struct {
		run     domain.WorkloadRun
		created bool
		err     error
	}
	results := make(chan result, testRunLimit+3)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < cap(results); index++ {
		index := index
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			now := time.Now().UTC()
			run := domain.WorkloadRun{ID: domain.WorkloadRunID(fmt.Sprintf("run_quota_%d", index)), TenantID: "tenant_workload_quota",
				Kind: domain.WorkloadJob, RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
				IdempotencyKey: fmt.Sprintf("quota-key-%d", index), TimeoutSeconds: 600, TimeoutAt: now.Add(10 * time.Minute), CreatedAt: now}
			type creation struct {
				run     domain.WorkloadRun
				created bool
			}
			stored, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (creation, error) {
				stored, created, err := CreateOrGet(ctx, tx, run)
				return creation{stored, created}, err
			})
			results <- result{run: stored.run, created: stored.created, err: err}
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	accepted := make([]domain.WorkloadRun, 0, testRunLimit)
	quotaErrors := 0
	for result := range results {
		if errors.Is(result.err, ErrQuotaExceeded) || errors.Is(result.err, quotas.ErrWorkloadQueueQuotaExceeded) {
			quotaErrors++
			continue
		}
		if result.err != nil || !result.created {
			t.Fatalf("concurrent Workload admission returned run=%+v created=%t err=%v", result.run, result.created, result.err)
		}
		accepted = append(accepted, result.run)
	}
	if len(accepted) != testRunLimit || quotaErrors != 3 {
		t.Fatalf("concurrent quota admitted %d and rejected %d, want %d and 3", len(accepted), quotaErrors, testRunLimit)
	}
	var active, reserved int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id='tenant_workload_quota' AND state NOT IN ('succeeded','failed','cancelled','timed_out')`).Scan(&active); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id='tenant_workload_quota'`).Scan(&reserved); err != nil || active != testRunLimit || reserved != testRunLimit {
		t.Fatalf("active/reserved Workload quota state=%d/%d err=%v", active, reserved, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifact_quota_reservations WHERE tenant_id='tenant_workload_quota'`).Scan(&reserved); err != nil || reserved != testRunLimit {
		t.Fatalf("active artifact byte reservations=%d, want %d: %v", reserved, testRunLimit, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, "tenant_workload_quota", "usr_workload_quota", quotas.DefaultWorkspaces, testRunLimit-1, quotas.DefaultArtifactBytes, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, time.Now().UTC())
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("quota update below current active reservations = %v, want ErrBelowCurrentUsage", err)
	}

	// Stable idempotency replay remains available at the limit and does not
	// consume a second reservation.
	replay := accepted[0]
	replay.ID = "run_quota_replay_must_not_exist"
	type creation struct {
		run     domain.WorkloadRun
		created bool
	}
	stored, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (creation, error) {
		run, created, err := CreateOrGet(ctx, tx, replay)
		return creation{run, created}, err
	})
	if err != nil || stored.created || stored.run.ID != accepted[0].ID {
		t.Fatalf("idempotent retry at quota limit=%+v created=%t err=%v", stored.run, stored.created, err)
	}

	now := time.Now().UTC()
	requestedCancel, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return RequestCancel(ctx, tx, accepted[0].TenantID, accepted[0].ID, now)
	})
	if err != nil || requestedCancel.DesiredState != domain.WorkloadDesiredCancelled {
		t.Fatalf("cancel request at quota limit failed: run=%+v err=%v", requestedCancel, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id=$1 AND workload_run_id=$2`, accepted[0].TenantID, accepted[0].ID).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("cancel request must hold the reservation until runtime cleanup: slot count=%d err=%v", reserved, err)
	}
	terminalRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		current, err := Get(ctx, tx, accepted[0].TenantID, accepted[0].ID)
		if err != nil {
			return domain.WorkloadRun{}, err
		}
		return Transition(ctx, tx, TransitionInput{TenantID: current.TenantID, RunID: current.ID, ExpectedRevision: current.Revision,
			NextState: domain.WorkloadCancelled, OccurredAt: now.Add(time.Second)})
	})
	if err != nil || terminalRun.State != domain.WorkloadCancelled {
		t.Fatalf("terminal Workload transition failed: run=%+v err=%v", terminalRun, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifact_quota_reservations WHERE tenant_id=$1 AND workload_run_id=$2`, accepted[0].TenantID, accepted[0].ID).Scan(&reserved); err != nil || reserved != 0 {
		t.Fatalf("terminal Workload Run should release unused artifact reservation: count=%d err=%v", reserved, err)
	}
	newRun := domain.WorkloadRun{ID: "run_quota_after_release", TenantID: "tenant_workload_quota", Kind: domain.WorkloadJob,
		RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, IdempotencyKey: "quota-after-release",
		TimeoutSeconds: 600, TimeoutAt: now.Add(10 * time.Minute), CreatedAt: now}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (creation, error) {
		run, created, err := CreateOrGet(ctx, tx, newRun)
		return creation{run, created}, err
	}); err != nil {
		t.Fatalf("new Workload Run could not claim terminally released slot: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id='tenant_workload_quota'`).Scan(&reserved); err != nil || reserved != testRunLimit {
		t.Fatalf("released slot was not reused exactly once: reservations=%d err=%v", reserved, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifact_quota_reservations WHERE tenant_id='tenant_workload_quota'`).Scan(&reserved); err != nil || reserved != testRunLimit {
		t.Fatalf("artifact reservation should be reused with Workload capacity: reservations=%d err=%v", reserved, err)
	}
}

func TestQueuedWorkloadBacklogQuotaReleasesWhenRunsLeaveQueue(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_queue_limit','Queue limit',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_queue_limit','queue-limit-owner','',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithQueueAndPreview(ctx, tx, "tenant_queue_limit", "usr_queue_limit", quotas.DefaultWorkspaces,
			5, 2, quotas.DefaultPreviewRuns, quotas.DefaultPreviewLifetimeSeconds, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, time.Now().UTC())
	}); err != nil {
		t.Fatalf("configure queued Workload limit: %v", err)
	}
	create := func(id string, now time.Time) (domain.WorkloadRun, error) {
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_queue_limit", Kind: domain.WorkloadJob,
			RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, IdempotencyKey: id,
			TimeoutSeconds: 600, TimeoutAt: now.Add(10 * time.Minute), CreatedAt: now}
		return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			stored, _, err := CreateOrGet(ctx, tx, run)
			return stored, err
		})
	}
	now := time.Now().UTC()
	first, err := create("run_queue_limit_1", now)
	if err != nil || first.State != domain.WorkloadPending {
		t.Fatalf("first queued Workload = %+v, %v", first, err)
	}
	second, err := create("run_queue_limit_2", now.Add(time.Second))
	if err != nil || second.State != domain.WorkloadPending {
		t.Fatalf("second queued Workload = %+v, %v", second, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithQueueAndPreview(ctx, tx, "tenant_queue_limit", "usr_queue_limit", quotas.DefaultWorkspaces,
			5, 1, quotas.DefaultPreviewRuns, quotas.DefaultPreviewLifetimeSeconds, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(2*time.Second))
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("lower queue cap below pending usage = %v, want ErrBelowCurrentUsage", err)
	}
	var runs int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id='tenant_queue_limit'`).Scan(&runs); err != nil || runs != 2 {
		t.Fatalf("Workload Run count=%d error=%v, want two", runs, err)
	}
	first, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: first.TenantID, RunID: first.ID,
			ExpectedRevision: first.Revision, NextState: domain.WorkloadProvisioning, OccurredAt: now.Add(3 * time.Second)})
	})
	if err != nil || first.State != domain.WorkloadProvisioning {
		t.Fatalf("start first Workload = %+v, %v", first, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithQueueAndPreview(ctx, tx, "tenant_queue_limit", "usr_queue_limit", quotas.DefaultWorkspaces,
			5, 1, quotas.DefaultPreviewRuns, quotas.DefaultPreviewLifetimeSeconds, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(4*time.Second))
	}); err != nil {
		t.Fatalf("queue cap should be lowered after dispatch: %v", err)
	}
	if _, err := create("run_queue_limit_3", now.Add(5*time.Second)); !errors.Is(err, quotas.ErrWorkloadQueueQuotaExceeded) {
		t.Fatalf("third queued Workload = %v, want queue quota exhaustion", err)
	}
	second, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: second.TenantID, RunID: second.ID,
			ExpectedRevision: second.Revision, NextState: domain.WorkloadProvisioning, OccurredAt: now.Add(6 * time.Second)})
	})
	if err != nil || second.State != domain.WorkloadProvisioning {
		t.Fatalf("start second Workload = %+v, %v", second, err)
	}
	if _, err := create("run_queue_limit_3", now.Add(7*time.Second)); err != nil {
		t.Fatalf("queue capacity was not released after dispatch: %v", err)
	}
}

func TestArtifactQuotaReservesBeforeWorkloadAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_artifact_quota','Artifact quota',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_artifact_quota','artifact-quota-owner','',now())`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, "tenant_artifact_quota", "usr_artifact_quota", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, quotas.MaxArtifactBytesPerWorkloadRun, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now)
	}); err != nil {
		t.Fatalf("configure one-Run artifact capacity: %v", err)
	}
	create := func(id string) (domain.WorkloadRun, bool, error) {
		t.Helper()
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_artifact_quota", Kind: domain.WorkloadJob,
			RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, IdempotencyKey: id,
			TimeoutSeconds: 600, TimeoutAt: now.Add(10 * time.Minute), CreatedAt: now}
		type createdRun struct {
			run     domain.WorkloadRun
			created bool
		}
		result, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (createdRun, error) {
			stored, created, err := CreateOrGet(ctx, tx, run)
			return createdRun{run: stored, created: created}, err
		})
		return result.run, result.created, err
	}
	first, created, err := create("run_artifact_quota_first")
	if err != nil || !created {
		t.Fatalf("first Run should reserve artifact capacity: created=%t err=%v", created, err)
	}
	if _, created, err := create("run_artifact_quota_second"); !errors.Is(err, quotas.ErrArtifactQuotaExceeded) || created {
		t.Fatalf("second Run should fail closed without artifact capacity: created=%t err=%v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, "tenant_artifact_quota", "usr_artifact_quota", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, quotas.MaxArtifactBytesPerWorkloadRun-1, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(time.Second))
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("quota cannot be lowered below an active artifact reservation: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: first.TenantID, RunID: first.ID,
			ExpectedRevision: first.Revision, NextState: domain.WorkloadCancelled, OccurredAt: now.Add(2 * time.Second)})
	}); err != nil {
		t.Fatalf("terminal Run should release artifact capacity: %v", err)
	}
	if _, created, err := create("run_artifact_quota_after_release"); err != nil || !created {
		t.Fatalf("released artifact capacity should be reusable: created=%t err=%v", created, err)
	}
}

func TestWorkloadQuotaReservationsReleaseAcrossTerminalLifecycle(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_quota_lifecycle','Quota lifecycle',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_quota_lifecycle','quota-lifecycle-owner','',now())`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, "tenant_quota_lifecycle", "usr_quota_lifecycle", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now)
	}); err != nil {
		t.Fatalf("configure Tenant quotas: %v", err)
	}

	createRun := func(id string, timeoutAt time.Time) domain.WorkloadRun {
		t.Helper()
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_quota_lifecycle", Kind: domain.WorkloadJob,
			RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, IdempotencyKey: id,
			TimeoutSeconds: int64(timeoutAt.Sub(now) / time.Second), TimeoutAt: timeoutAt, CreatedAt: now}
		created, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct {
			run     domain.WorkloadRun
			created bool
		}, error) {
			result, created, err := CreateOrGet(ctx, tx, run)
			return struct {
				run     domain.WorkloadRun
				created bool
			}{result, created}, err
		})
		if err != nil || !created.created {
			t.Fatalf("create Workload Run %s: created=%t err=%v", id, created.created, err)
		}
		return created.run
	}
	assertNoReservations := func(runID domain.WorkloadRunID) {
		t.Helper()
		var slots, artifacts int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id=$1 AND workload_run_id=$2`,
			"tenant_quota_lifecycle", runID).Scan(&slots); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifact_quota_reservations WHERE tenant_id=$1 AND workload_run_id=$2`,
			"tenant_quota_lifecycle", runID).Scan(&artifacts); err != nil {
			t.Fatal(err)
		}
		if slots != 0 || artifacts != 0 {
			t.Fatalf("terminal Run %s retained quota reservations: slots=%d artifacts=%d", runID, slots, artifacts)
		}
	}
	transition := func(run domain.WorkloadRun, next domain.WorkloadRunState, at time.Time) (domain.WorkloadRun, error) {
		t.Helper()
		return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
				ExpectedRevision: run.Revision, NextState: next, OccurredAt: at})
		})
	}

	// Cancellation before execution releases both the concurrent-run slot and
	// the bounded log-artifact reservation in the same terminal transaction.
	cancelled := createRun("run_quota_cancelled_before_start", now.Add(10*time.Minute))
	cancelled, err := transition(cancelled, domain.WorkloadCancelled, now.Add(time.Second))
	if err != nil || cancelled.State != domain.WorkloadCancelled {
		t.Fatalf("cancel before start: run=%+v err=%v", cancelled, err)
	}
	assertNoReservations(cancelled.ID)
	if _, err := transition(cancelled, domain.WorkloadCancelled, now.Add(2*time.Second)); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("duplicate cancelled transition = %v, want ErrTransitionConflict", err)
	}
	assertNoReservations(cancelled.ID)

	// Failed provisioning follows a real nonterminal transition before failure.
	failed := createRun("run_quota_failed_provisioning", now.Add(10*time.Minute))
	failed, err = transition(failed, domain.WorkloadProvisioning, now.Add(3*time.Second))
	if err != nil || failed.State != domain.WorkloadProvisioning {
		t.Fatalf("start provisioning: run=%+v err=%v", failed, err)
	}
	failed, err = transition(failed, domain.WorkloadFailed, now.Add(4*time.Second))
	if err != nil || failed.State != domain.WorkloadFailed {
		t.Fatalf("fail provisioning: run=%+v err=%v", failed, err)
	}
	assertNoReservations(failed.ID)
	if _, err := transition(failed, domain.WorkloadFailed, now.Add(5*time.Second)); !errors.Is(err, ErrTransitionConflict) {
		t.Fatalf("duplicate failed transition = %v, want ErrTransitionConflict", err)
	}
	assertNoReservations(failed.ID)

	// Expiration is the timeout lifecycle entry point and must release the same
	// reservations as a direct terminal transition.
	timedOut := createRun("run_quota_timed_out", now.Add(time.Second))
	stats, err := ExpireDue(ctx, pool, transaction.DefaultPolicy(), now.Add(2*time.Second), 10)
	if err != nil || stats != (ExpireStats{Claimed: 1, TimedOut: 1}) {
		t.Fatalf("expire timed-out Run: stats=%+v err=%v", stats, err)
	}
	assertNoReservations(timedOut.ID)
	stats, err = ExpireDue(ctx, pool, transaction.DefaultPolicy(), now.Add(3*time.Second), 10)
	if err != nil || stats != (ExpireStats{}) {
		t.Fatalf("duplicate expiration pass: stats=%+v err=%v", stats, err)
	}
	assertNoReservations(timedOut.ID)

	// Simulate a stale reservation left by an earlier writer/version. The next
	// admission repairs both reservation tables under the Tenant OCC guard.
	if _, err := pool.Exec(ctx, `INSERT INTO workload_quota_slots (tenant_id,slot,workload_run_id,created_at)
		VALUES ('tenant_quota_lifecycle',1,$1,$2)`, cancelled.ID, now); err != nil {
		t.Fatalf("seed stale terminal Workload reservation: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_artifact_quota_reservations (tenant_id,workload_run_id,reserved_bytes,created_at)
		VALUES ('tenant_quota_lifecycle',$1,1,$2)`, failed.ID, now); err != nil {
		t.Fatalf("seed stale terminal artifact reservation: %v", err)
	}

	// All released capacity is reusable after terminal persistence and stale
	// reservation repair.
	replacement := createRun("run_quota_after_lifecycle_release", now.Add(10*time.Minute))
	if replacement.State != domain.WorkloadPending {
		t.Fatalf("replacement Run state=%q, want pending", replacement.State)
	}
	assertNoReservations(cancelled.ID)
	assertNoReservations(failed.ID)
	var replacementSlots, replacementArtifacts int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id=$1 AND workload_run_id=$2`,
		replacement.TenantID, replacement.ID).Scan(&replacementSlots); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_artifact_quota_reservations WHERE tenant_id=$1 AND workload_run_id=$2`,
		replacement.TenantID, replacement.ID).Scan(&replacementArtifacts); err != nil {
		t.Fatal(err)
	}
	if replacementSlots != 1 || replacementArtifacts != 1 {
		t.Fatalf("replacement Run reservation counts = slots %d/artifacts %d, want 1/1", replacementSlots, replacementArtifacts)
	}
}

func TestTenantPreviewQuotaAdmissionLifetimeAndRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_preview_quota','Preview quota',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_preview_quota','preview-quota-owner','',now())`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithPreview(ctx, tx, "tenant_preview_quota", "usr_preview_quota", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, 2, 3600, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now)
	}); err != nil {
		t.Fatalf("configure Preview quota: %v", err)
	}

	create := func(id string, kind domain.WorkloadKind, timeout time.Duration) (domain.WorkloadRun, bool, error) {
		t.Helper()
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_preview_quota", Kind: kind,
			RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted, IdempotencyKey: id,
			TimeoutSeconds: int64(timeout / time.Second), TimeoutAt: now.Add(timeout), CreatedAt: now}
		type result struct {
			run     domain.WorkloadRun
			created bool
		}
		stored, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (result, error) {
			run, created, err := CreateOrGet(ctx, tx, run)
			return result{run: run, created: created}, err
		})
		return stored.run, stored.created, err
	}
	first, created, err := create("run_preview_quota_first", domain.WorkloadPreview, time.Hour)
	if err != nil || !created {
		t.Fatalf("first Preview admission: created=%v err=%v", created, err)
	}
	second, created, err := create("run_preview_quota_second", domain.WorkloadPreview, time.Hour)
	if err != nil || !created {
		t.Fatalf("second Preview below configured limit: created=%v err=%v", created, err)
	}
	if _, created, err := create("run_preview_quota_third", domain.WorkloadPreview, time.Hour); !errors.Is(err, quotas.ErrPreviewQuotaExceeded) || created {
		t.Fatalf("Preview admission over count limit: created=%v err=%v", created, err)
	}
	if _, created, err := create("run_preview_quota_long", domain.WorkloadPreview, time.Hour+time.Second); !errors.Is(err, quotas.ErrPreviewQuotaExceeded) || created {
		t.Fatalf("Preview admission over lifetime limit: created=%v err=%v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithPreview(ctx, tx, "tenant_preview_quota", "usr_preview_quota", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, 1, 3600, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(time.Second))
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("cannot lower Preview count below active Runs: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithPreview(ctx, tx, "tenant_preview_quota", "usr_preview_quota", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, 2, 1800, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(time.Second))
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("cannot lower Preview lifetime below an active Run: %v", err)
	}
	if _, created, err := create("run_preview_quota_job", domain.WorkloadJob, time.Hour+time.Second); err != nil || !created {
		t.Fatalf("non-Preview Run should not consume Preview-specific quota: created=%v err=%v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.SetWithPreview(ctx, tx, "tenant_preview_quota", "usr_preview_quota", quotas.DefaultWorkspaces,
			quotas.DefaultConcurrentWorkloadRuns, 0, 3600, quotas.DefaultArtifactBytes,
			quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(time.Second))
	}); !errors.Is(err, quotas.ErrInvalidLimits) {
		t.Fatalf("invalid zero Preview quota update = %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: first.TenantID, RunID: first.ID,
			ExpectedRevision: first.Revision, NextState: domain.WorkloadCancelled, OccurredAt: now.Add(2 * time.Second)})
	}); err != nil {
		t.Fatalf("cancel Preview Run: %v", err)
	}
	if _, created, err := create("run_preview_quota_after_release", domain.WorkloadPreview, time.Hour); err != nil || !created {
		t.Fatalf("released Preview quota was not reusable: created=%v err=%v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: second.TenantID, RunID: second.ID,
			ExpectedRevision: second.Revision, NextState: domain.WorkloadCancelled, OccurredAt: now.Add(3 * time.Second)})
	}); err != nil {
		t.Fatalf("cancel second Preview Run: %v", err)
	}
	var count int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_runs WHERE tenant_id='tenant_preview_quota' AND id='run_preview_quota_long'`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected over-lifetime Preview left a Run row: count=%d err=%v", count, err)
	}
}

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
	var checkStatus, checkConclusion, checkSHA, checkSummary string
	var checkRunID *int64
	if err := pool.QueryRow(ctx, `SELECT status, COALESCE(conclusion,''), commit_sha, output_summary, check_run_id
		FROM workload_github_checks WHERE tenant_id = $1 AND workload_run_id = $2`, run.TenantID, run.ID).
		Scan(&checkStatus, &checkConclusion, &checkSHA, &checkSummary, &checkRunID); err != nil ||
		checkStatus != "queued" || checkConclusion != "" || checkSHA != run.CommitSHA || checkRunID != nil || !strings.Contains(checkSummary, "pending") {
		t.Fatalf("initial durable GitHub Check = status %q conclusion %q SHA %q summary %q CheckID %v err=%v", checkStatus, checkConclusion, checkSHA, checkSummary, checkRunID, err)
	}
	checkState, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (GitHubCheck, error) {
		return GetGitHubCheck(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || checkState.Status != "queued" || checkState.WorkloadRunID != run.ID || checkState.Revision != 1 || checkState.CheckRunID != 0 {
		t.Fatalf("load durable GitHub Check = %+v err=%v", checkState, err)
	}
	var checkEvents int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'workload_run.github_check_requested'`, run.ID).Scan(&checkEvents); err != nil || checkEvents != 1 {
		t.Fatalf("initial GitHub Check Outbox event count = %d err=%v", checkEvents, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, SaveGitHubCheckRunID(ctx, tx, run.TenantID, run.ID, run.CommitSHA, 991, now.Add(time.Second))
	}); err != nil {
		t.Fatalf("persist GitHub Check ID: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, SaveGitHubCheckRunID(ctx, tx, run.TenantID, run.ID, strings.Repeat("b", 40), 992, now.Add(time.Second))
	}); !errors.Is(err, ErrGitHubCheckIdentityMismatch) {
		t.Fatalf("persist Check ID against a different SHA = %v", err)
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
	for _, transition := range []struct {
		expectedRevision int64
		nextState        domain.WorkloadRunState
		checkStatus      string
	}{
		{expectedRevision: 1, nextState: domain.WorkloadProvisioning, checkStatus: "in_progress"},
		{expectedRevision: 2, nextState: domain.WorkloadRunning, checkStatus: "in_progress"},
	} {
		stored, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
				ExpectedRevision: transition.expectedRevision, NextState: transition.nextState,
				OccurredAt: now.Add(time.Duration(transition.expectedRevision+1) * time.Second)})
		})
		if err != nil || stored.State != transition.nextState {
			t.Fatalf("transition Workload Run to %s = %+v err=%v", transition.nextState, stored, err)
		}
		if err := pool.QueryRow(ctx, `SELECT status FROM workload_github_checks WHERE tenant_id=$1 AND workload_run_id=$2`,
			run.TenantID, run.ID).Scan(&checkStatus); err != nil || checkStatus != transition.checkStatus {
			t.Fatalf("GitHub Check after %s = %q err=%v", transition.nextState, checkStatus, err)
		}
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_run.github_check_requested'`, run.ID).
			Scan(&checkEvents); err != nil || checkEvents != int(transition.expectedRevision+1) {
			t.Fatalf("GitHub Check Outbox count after %s = %d err=%v", transition.nextState, checkEvents, err)
		}
	}
	events, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadRunEvent, error) {
		return ListEvents(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(events) != 3 || events[0].Type != "workload.pending" || events[1].Type != "workload.provisioning" || events[2].Type != "workload.running" {
		t.Fatalf("creation events = %+v err=%v", events, err)
	}
	stored, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return RequestCancel(ctx, tx, run.TenantID, run.ID, now.Add(4*time.Second))
	})
	if err != nil || stored.DesiredState != domain.WorkloadDesiredCancelled || stored.Revision != 4 {
		t.Fatalf("cancel request = %+v err=%v", stored, err)
	}
	_, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return RequestCancel(ctx, tx, run.TenantID, run.ID, now.Add(5*time.Second))
	})
	if err != nil {
		t.Fatalf("repeated cancel request: %v", err)
	}
	completed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
			ExpectedRevision: 4, NextState: domain.WorkloadCancelled, OccurredAt: now.Add(6 * time.Second)})
	})
	if err != nil || completed.State != domain.WorkloadCancelled || completed.CompletedAt == nil || completed.Revision != 5 {
		t.Fatalf("cancel transition = %+v err=%v", completed, err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, conclusion FROM workload_github_checks WHERE tenant_id = $1 AND workload_run_id = $2`, run.TenantID, run.ID).
		Scan(&checkStatus, &checkConclusion); err != nil || checkStatus != "completed" || checkConclusion != "cancelled" {
		t.Fatalf("terminal GitHub Check mapping = %q/%q err=%v", checkStatus, checkConclusion, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'workload_run.github_check_requested'`, run.ID).Scan(&checkEvents); err != nil || checkEvents != 4 {
		t.Fatalf("GitHub Check transition Outbox event count = %d err=%v", checkEvents, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return Transition(ctx, tx, TransitionInput{TenantID: run.TenantID, RunID: run.ID,
			ExpectedRevision: 5, NextState: domain.WorkloadRunning, OccurredAt: now.Add(7 * time.Second)})
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
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, run.TenantID, "usr_workload_test", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, 17, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(6*time.Second))
	}); err != nil {
		t.Fatalf("set exact artifact quota: %v", err)
	}
	tooLarge := unredacted
	tooLarge.ID, tooLarge.SizeBytes, tooLarge.CreatedAt = "artifact_over_quota", 1, now.Add(7*time.Second)
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, AddArtifact(ctx, tx, tooLarge)
	}); !errors.Is(err, quotas.ErrArtifactQuotaExceeded) {
		t.Fatalf("artifact write over Tenant quota = %v, want ErrArtifactQuotaExceeded", err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, run.TenantID, "usr_workload_test", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, 16, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(8*time.Second))
	}); !errors.Is(err, quotas.ErrBelowCurrentUsage) {
		t.Fatalf("artifact quota below retained usage = %v, want ErrBelowCurrentUsage", err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, run.TenantID, "usr_workload_test", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, quotas.DefaultArtifactBytes, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, now.Add(9*time.Second))
	}); err != nil {
		t.Fatalf("restore Tenant artifact quota after boundary test: %v", err)
	}
	artifacts, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return ListArtifacts(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(artifacts) != 1 || !artifacts[0].Redacted {
		t.Fatalf("Workload artifacts = %+v err=%v", artifacts, err)
	}
	artifactExpiry := now.Add(10 * time.Second)
	if _, err := pool.Exec(ctx, `UPDATE workload_artifacts SET expires_at = $1 WHERE id = $2`, artifactExpiry, unredacted.ID); err != nil {
		t.Fatalf("set artifact expiry for cleanup test: %v", err)
	}
	expired, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return ListExpiredArtifacts(ctx, tx, artifactExpiry, 10)
	})
	if err != nil || len(expired) != 1 || expired[0].ID != unredacted.ID {
		t.Fatalf("expired artifact batch = %+v err=%v", expired, err)
	}
	removed, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return DeleteExpiredArtifact(ctx, tx, expired[0], artifactExpiry)
	})
	if err != nil || !removed {
		t.Fatalf("delete expired artifact metadata: removed=%t err=%v", removed, err)
	}
	removed, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return DeleteExpiredArtifact(ctx, tx, expired[0], artifactExpiry)
	})
	if err != nil || removed {
		t.Fatalf("replayed artifact metadata deletion: removed=%t err=%v", removed, err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, run.TenantID, "usr_workload_test", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, 1, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, artifactExpiry.Add(time.Second))
	}); err != nil {
		t.Fatalf("deleted artifact bytes should be released from quota: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (quotas.Limits, error) {
		return quotas.Set(ctx, tx, run.TenantID, "usr_workload_test", quotas.DefaultWorkspaces, quotas.DefaultConcurrentWorkloadRuns, quotas.DefaultArtifactBytes, quotas.DefaultWorkspaceServiceMemoryBytes, quotas.DefaultWorkspaceServiceMilliCPU, artifactExpiry.Add(2*time.Second))
	}); err != nil {
		t.Fatalf("restore artifact quota after cleanup test: %v", err)
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
	var timeoutReservations int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workload_quota_slots WHERE tenant_id=$1 AND workload_run_id=$2`, timeoutRun.TenantID, timeoutRun.ID).Scan(&timeoutReservations); err != nil || timeoutReservations != 0 {
		t.Fatalf("timed-out Workload Run retained its quota reservation: count=%d err=%v", timeoutReservations, err)
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

func TestWorkloadRunPinsTenantAppRevisionAndNeverFallsBackForUnboundInstallation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	statements := []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_run_pin','Run pin',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_run_pin','run-pin-sub','',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_run_pin',9301,'acme','active','usr_run_pin',now(),now()),('tenant_run_pin',9302,'acme','active','usr_run_pin',now(),now())`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9301,'tenant_run_pin',now()),(9302,'tenant_run_pin',now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_run_pin',9301,9303,'acme','owned','main',now()),('tenant_run_pin',9302,9304,'acme','unbound','main',now())`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_run_pin',9305,'tenant_owned','tenant_run_pin','run-pin','Iv1.client',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_run_pin','tenant_run_pin',9305,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:key','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_run_pin','tenant_run_pin','registration_run_pin',9305,'binding_run_pin','active',4,9,'registration-key','` + strings.Repeat("a", 64) + `','usr_run_pin',now(),now())`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_run_pin','integration_run_pin',9301,now())`,
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	newRun := func(id, key string, installation, repository int64) domain.WorkloadRun {
		return domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_run_pin", Kind: domain.WorkloadJob, RequestedBy: "usr_run_pin", IdempotencyKey: key, RuntimeClass: "standard", GitHubInstallation: installation, GitHubRepository: repository, CommitSHA: strings.Repeat("b", 40), TrustLevel: domain.WorkloadUntrusted, TimeoutSeconds: 120, TimeoutAt: now.Add(120 * time.Second), CreatedAt: now}
	}
	created, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		run, _, err := CreateOrGet(ctx, tx, newRun("run_pinned_app", "pin-app-key", 9301, 9303))
		return run, err
	})
	if err != nil || created.GitHubIntegrationID != "integration_run_pin" || created.GitHubConfigRevision != 4 || created.GitHubCredentialGeneration != 9 {
		t.Fatalf("Run did not pin active App identity: %+v err=%v", created, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		_, _, err := CreateOrGet(ctx, tx, newRun("run_unbound_app", "unbound-app-key", 9302, 9304))
		return struct{}{}, err
	}); !errors.Is(err, ErrRepositoryInactive) {
		t.Fatalf("unbound Installation silently fell back to shared credentials: %v", err)
	}
}
