//go:build integration

package workloadjobs

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWorkloadResultFencesAttemptsAndPersistsTerminalMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id,name,created_at) VALUES ('tenant_result','Result',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes (id,provider,region,capabilities_json) VALUES ('rp_result','aws','ap-northeast-1','{}')`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	commit := strings.Repeat("a", 40)
	create := func(runID, attemptID string, desired string, timeoutAt time.Time) {
		t.Helper()
		if _, err := pool.Exec(ctx, `INSERT INTO workload_runs (id,tenant_id,kind,commit_sha,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
			VALUES ($1,'tenant_result','job',$2,'standard','untrusted',$3,'provisioning','rp_result',$1,$1,600,$4,2,$5,$5)`,
			runID, commit, desired, timeoutAt, now); err != nil {
			t.Fatal(err)
		}
		if _, err := pool.Exec(ctx, `INSERT INTO workload_run_attempts (id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,client_token,state,created_at,updated_at)
			VALUES ($1,'tenant_result',$2,1,1,'rp_result',$1,'running',$3,$3)`, attemptID, runID, now); err != nil {
			t.Fatal(err)
		}
	}
	create("run_result_ok", "attempt_result_ok", "running", now.Add(10*time.Minute))
	create("run_result_cancel", "attempt_result_cancel", "cancelled", now.Add(10*time.Minute))
	create("run_result_timeout", "attempt_result_timeout", "running", now.Add(time.Minute))

	handler := ResultHandler{Pool: pool, Policy: transaction.DefaultPolicy(), Now: func() time.Time { return now.Add(2 * time.Minute) }}
	result := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_ok",
		AttemptID: "attempt_result_ok", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(20 * time.Second), LogArtifact: &WorkloadLogArtifact{
			StorageRef: "s3://hako-results/run_result_ok/logs", SizeBytes: 12, SHA256: strings.Repeat("b", 64), Redacted: true}}
	outcome, err := handler.Handle(ctx, result)
	if err != nil || outcome != ResultApplied {
		t.Fatalf("Handle() = %q, %v", outcome, err)
	}
	run, err := getResultRun(ctx, pool, "run_result_ok")
	if err != nil || run.State != domain.WorkloadSucceeded || run.StartedAt == nil || run.CompletedAt == nil {
		t.Fatalf("result did not persist terminal Run: %+v, %v", run, err)
	}
	outcome, err = handler.Handle(ctx, result)
	if err != nil || outcome != ResultDuplicate {
		t.Fatalf("duplicate result = %q, %v", outcome, err)
	}
	result.Generation = 2
	outcome, err = handler.Handle(ctx, result)
	if err != nil || outcome != ResultStale {
		t.Fatalf("stale result = %q, %v", outcome, err)
	}
	result.Generation = 1
	result.TenantID = "tenant_other"
	if _, err := handler.Handle(ctx, result); !errors.Is(err, ErrWorkloadResultMismatch) {
		t.Fatalf("cross-Tenant result error = %v", err)
	}

	cancelResult := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_cancel",
		AttemptID: "attempt_result_cancel", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(20 * time.Second)}
	if outcome, err := handler.Handle(ctx, cancelResult); err != nil || outcome != ResultApplied {
		t.Fatalf("cancel race result = %q, %v", outcome, err)
	}
	cancelled, err := getResultRun(ctx, pool, "run_result_cancel")
	if err != nil || cancelled.State != domain.WorkloadCancelled {
		t.Fatalf("cancel request was overwritten by result: %+v, %v", cancelled, err)
	}

	timeoutResult := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_timeout",
		AttemptID: "attempt_result_timeout", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(2 * time.Minute)}
	if outcome, err := handler.Handle(ctx, timeoutResult); err != nil || outcome != ResultApplied {
		t.Fatalf("deadline result = %q, %v", outcome, err)
	}
	timedOut, err := getResultRun(ctx, pool, "run_result_timeout")
	if err != nil || timedOut.State != domain.WorkloadTimedOut {
		t.Fatalf("late result did not honor immutable deadline: %+v, %v", timedOut, err)
	}

	artifacts, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, "tenant_result", "run_result_ok")
	})
	if err != nil || len(artifacts) != 1 || !artifacts[0].Redacted {
		t.Fatalf("persisted log metadata = %+v, %v", artifacts, err)
	}
}

func getResultRun(ctx context.Context, pool *pgxpool.Pool, runID domain.WorkloadRunID) (domain.WorkloadRun, error) {
	return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_result", runID)
	})
}
