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
	if _, err := pool.Exec(ctx, `INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('user_result','subject_result','',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_installations
		(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		VALUES ('tenant_result',901,'result-owner','active','user_result',now(),now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at)
		VALUES (901,'tenant_result',now())`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories
		(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at)
		VALUES ('tenant_result',901,902,'result-owner','result-repo','main',now())`); err != nil {
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
		if _, err := pool.Exec(ctx, `INSERT INTO workload_run_attempts (id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
			VALUES ($1,'tenant_result',$2,1,1,'rp_result',$1,'running',$3,$3)`, attemptID, runID, now); err != nil {
			t.Fatal(err)
		}
	}
	create("run_result_ok", "attempt_result_ok", "running", now.Add(10*time.Minute))
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET correlation_id='req_dddddddd-dddd-4ddd-8ddd-dddddddddddd'
		WHERE tenant_id='tenant_result' AND id='run_result_ok'`); err != nil {
		t.Fatal(err)
	}
	create("run_result_cancel", "attempt_result_cancel", "cancelled", now.Add(10*time.Minute))
	create("run_result_timeout", "attempt_result_timeout", "running", now.Add(time.Minute))
	create("run_result_agent", "attempt_result_agent", "running", now.Add(10*time.Minute))
	create("run_result_agent_cancel", "attempt_result_agent_cancel", "cancelled", now.Add(10*time.Minute))
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET kind='agent',github_installation_id=901,github_repository_id=902
		WHERE tenant_id='tenant_result' AND id IN ('run_result_agent','run_result_agent_cancel')`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET github_installation_id=901,github_repository_id=902
		WHERE tenant_id='tenant_result' AND id='run_result_ok'`); err != nil {
		t.Fatal(err)
	}

	handler := ResultHandler{Pool: pool, Policy: transaction.DefaultPolicy(), Now: func() time.Time { return now.Add(2 * time.Minute) }}
	result := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_ok",
		AttemptID: "attempt_result_ok", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		CorrelationID:   "req_dddddddd-dddd-4ddd-8ddd-dddddddddddd",
		PipelineResults: []WorkloadPipelineResult{{Name: "build", Status: "succeeded"}, {Name: "test", Status: "succeeded"}},
		Status:          domain.WorkloadSucceeded, CompletedAt: now.Add(20 * time.Second), LogArtifact: &WorkloadLogArtifact{
			StorageRef: "s3://hako-results/run_result_ok/logs", SizeBytes: 12, SHA256: strings.Repeat("b", 64), Redacted: true}}
	spoofedCorrelation := result
	spoofedCorrelation.CorrelationID = "req_99999999-9999-4999-8999-999999999999"
	if _, err := handler.Handle(ctx, spoofedCorrelation); !errors.Is(err, ErrWorkloadResultMismatch) {
		t.Fatalf("result with a foreign correlation ID was accepted: %v", err)
	}
	jobWithAgentBundle := result
	jobWithAgentBundle.ChangeBundle = &WorkloadChangeArtifact{StorageRef: "s3://hako-results/run_result_ok/agent-changes/change.zip",
		SizeBytes: 4, SHA256: strings.Repeat("c", 64)}
	if _, err := handler.Handle(ctx, jobWithAgentBundle); !errors.Is(err, ErrWorkloadResultMismatch) {
		t.Fatalf("Job run accepted a Native Agent proposal artifact: %v", err)
	}
	outcome, err := handler.Handle(ctx, result)
	if err != nil || outcome != ResultApplied {
		t.Fatalf("Handle() = %q, %v", outcome, err)
	}
	run, err := getResultRun(ctx, pool, "run_result_ok")
	if err != nil || run.State != domain.WorkloadSucceeded || run.StartedAt == nil || run.CompletedAt == nil ||
		len(run.PipelineResults) != 2 || run.PipelineResults[0].Name != "build" || run.PipelineResults[0].Status != "succeeded" ||
		run.PipelineResults[1].Name != "test" || run.PipelineResults[1].Status != "succeeded" {
		t.Fatalf("result did not persist terminal Run: %+v, %v", run, err)
	}
	var pipelineCheckCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_github_checks
		WHERE tenant_id='tenant_result' AND workload_run_id='run_result_ok' AND pipeline_name <> ''`).Scan(&pipelineCheckCount); err != nil || pipelineCheckCount != 2 {
		t.Fatalf("per-pipeline GitHub Check rows = %d, want 2: %v", pipelineCheckCount, err)
	}
	checkRows, err := pool.Query(ctx, `SELECT pipeline_name,check_name,conclusion FROM workload_github_checks
		WHERE tenant_id='tenant_result' AND workload_run_id='run_result_ok' AND pipeline_name <> '' ORDER BY pipeline_name`)
	if err != nil {
		t.Fatal(err)
	}
	var checks []struct{ pipeline, name, conclusion string }
	for checkRows.Next() {
		var check struct{ pipeline, name, conclusion string }
		if err := checkRows.Scan(&check.pipeline, &check.name, &check.conclusion); err != nil {
			checkRows.Close()
			t.Fatal(err)
		}
		checks = append(checks, check)
	}
	if err := checkRows.Err(); err != nil {
		checkRows.Close()
		t.Fatal(err)
	}
	checkRows.Close()
	if len(checks) != 2 || checks[0].pipeline != "build" || checks[0].name != "hako/job/build" || checks[0].conclusion != "success" ||
		checks[1].pipeline != "test" || checks[1].name != "hako/job/test" || checks[1].conclusion != "success" {
		t.Fatalf("pipeline-specific durable GitHub Checks = %+v", checks)
	}
	pipelineCheck, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (workloads.GitHubCheck, error) {
		return workloads.GetGitHubPipelineCheck(ctx, tx, "tenant_result", "run_result_ok", "test")
	})
	if err != nil || pipelineCheck.PipelineName != "test" || pipelineCheck.Name != "hako/job/test" || pipelineCheck.Conclusion != "success" {
		t.Fatalf("load pipeline-scoped durable Check = %+v, %v", pipelineCheck, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct{}, error) {
		return struct{}{}, workloads.SaveGitHubPipelineCheckRunID(ctx, tx, "tenant_result", "run_result_ok", "test", commit, 991, now.Add(time.Second))
	}); err != nil {
		t.Fatalf("persist pipeline-scoped GitHub Check ID: %v", err)
	}
	var pipelineCheckOutboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id='run_result_ok' AND event_type='workload_run.github_check_requested'
		AND payload_json::jsonb ? 'pipeline_name'`).Scan(&pipelineCheckOutboxCount); err != nil || pipelineCheckOutboxCount != 2 {
		t.Fatalf("per-pipeline GitHub Check Outbox rows = %d, want 2: %v", pipelineCheckOutboxCount, err)
	}
	var correlatedCheckOutboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events
		WHERE aggregate_id='run_result_ok' AND event_type='workload_run.github_check_requested'
		AND payload_json::jsonb->>'correlation_id'='req_dddddddd-dddd-4ddd-8ddd-dddddddddddd'`).Scan(&correlatedCheckOutboxCount); err != nil || correlatedCheckOutboxCount != 4 {
		t.Fatalf("correlated GitHub Check Outbox rows = %d, want 4: %v", correlatedCheckOutboxCount, err)
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
	if artifacts[0].CorrelationID != result.CorrelationID {
		t.Fatalf("artifact correlation ID=%q, want %q", artifacts[0].CorrelationID, result.CorrelationID)
	}
	agentResult := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_agent",
		AttemptID: "attempt_result_agent", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(30 * time.Second),
		ChangeBundle: &WorkloadChangeArtifact{StorageRef: "s3://hako-results/run_result_agent/agent-changes/change.zip",
			SizeBytes: 128, SHA256: strings.Repeat("c", 64)}}
	if outcome, err := handler.Handle(ctx, agentResult); err != nil || outcome != ResultApplied {
		t.Fatalf("Agent change result = %q, %v", outcome, err)
	}
	agentArtifacts, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, "tenant_result", "run_result_agent")
	})
	if err != nil || len(agentArtifacts) != 1 || agentArtifacts[0].Kind != "agent_changes" || agentArtifacts[0].Redacted ||
		agentArtifacts[0].SHA256 != strings.Repeat("c", 64) {
		t.Fatalf("persisted Agent proposal artifact = %+v, %v", agentArtifacts, err)
	}
	cancelledAgentResult := WorkloadResult{SchemaVersion: 1, TenantID: "tenant_result", WorkloadRunID: "run_result_agent_cancel",
		AttemptID: "attempt_result_agent_cancel", Generation: 1, ResourcePlaneID: "rp_result", CommitSHA: commit,
		Status: domain.WorkloadSucceeded, CompletedAt: now.Add(30 * time.Second),
		ChangeBundle: &WorkloadChangeArtifact{StorageRef: "s3://hako-results/run_result_agent_cancel/agent-changes/change.zip",
			SizeBytes: 128, SHA256: strings.Repeat("c", 64)}}
	if outcome, err := handler.Handle(ctx, cancelledAgentResult); err != nil || outcome != ResultApplied {
		t.Fatalf("cancelled Agent result = %q, %v", outcome, err)
	}
	cancelledAgentArtifacts, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, "tenant_result", "run_result_agent_cancel")
	})
	if err != nil || len(cancelledAgentArtifacts) != 0 {
		t.Fatalf("cancelled Agent proposal artifact was exposed: %+v %v", cancelledAgentArtifacts, err)
	}
}

func getResultRun(ctx context.Context, pool *pgxpool.Pool, runID domain.WorkloadRunID) (domain.WorkloadRun, error) {
	return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_result", runID)
	})
}
