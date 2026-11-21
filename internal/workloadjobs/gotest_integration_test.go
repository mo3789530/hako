//go:build integration

package workloadjobs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

type resultExecutor struct {
	result fake.JobResult
	err    error
	calls  int
}

func (e *resultExecutor) RunGoTest(_ context.Context, _ fake.JobSpec) (fake.JobResult, error) {
	e.calls++
	return e.result, e.err
}

type cancelWaitingExecutor struct {
	started chan struct{}
	cancel  chan struct{}
}

func (e *cancelWaitingExecutor) RunGoTest(context.Context, fake.JobSpec) (fake.JobResult, error) {
	close(e.started)
	<-e.cancel
	return fake.JobResult{}, errors.New("synthetic executor cancellation")
}

type recordingArtifacts struct {
	logs []byte
	sha  string
	err  error
}

func (a *recordingArtifacts) PutRedactedLogs(_ context.Context, _ domain.TenantID, _ domain.WorkloadRunID, logs []byte, sha string) (string, error) {
	if a.err != nil {
		return "", a.err
	}
	a.logs = append([]byte(nil), logs...)
	a.sha = sha
	return "s3://hako-artifacts/integration/logs", nil
}

func TestExecuteGoTestPersistsTerminalStateAndRedactedLogMetadata(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenants (id, name, created_at) VALUES ('tenant_factory_jobs', 'Factory Jobs', now())`); err != nil {
		t.Fatal(err)
	}
	policy := transaction.DefaultPolicy()
	commit := "0123456789abcdef0123456789abcdef01234567"
	started := time.Now().UTC().Truncate(time.Second)
	createRun := func(id, key string) domain.WorkloadRun {
		t.Helper()
		run := domain.WorkloadRun{ID: domain.WorkloadRunID(id), TenantID: "tenant_factory_jobs", Kind: domain.WorkloadJob,
			IdempotencyKey: key, CommitSHA: commit, RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
			TimeoutSeconds: 300, TimeoutAt: started.Add(5 * time.Minute), CreatedAt: started}
		if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			stored, _, err := workloads.CreateOrGet(ctx, tx, run)
			return stored, err
		}); err != nil {
			t.Fatalf("create Workload Run: %v", err)
		}
		return run
	}

	logs := []byte("tests passed; token=[REDACTED]\n")
	digest := sha256.Sum256(logs)
	artifacts := &recordingArtifacts{}
	service := Service{Pool: pool, Policy: policy, Artifacts: artifacts, Now: func() time.Time { return started.Add(time.Second) },
		Executor: &resultExecutor{result: fake.JobResult{TenantID: "tenant_factory_jobs", RunID: "run_factory_success",
			CommitSHA: commit, ExitCode: 0, RedactedLogs: logs, LogsSHA256: hex.EncodeToString(digest[:])}}}
	run := createRun("run_factory_success", "factory-success")
	result, err := service.ExecuteGoTest(ctx, fake.JobSpec{TenantID: run.TenantID, RunID: run.ID,
		RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
	if err != nil || result.State != domain.WorkloadSucceeded || result.Revision != 4 {
		t.Fatalf("ExecuteGoTest() = %+v, error = %v", result, err)
	}
	if string(artifacts.logs) != string(logs) || artifacts.sha != hex.EncodeToString(digest[:]) {
		t.Fatalf("stored logs = %q, sha = %q", artifacts.logs, artifacts.sha)
	}
	storedArtifacts, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(storedArtifacts) != 1 || !storedArtifacts[0].Redacted || storedArtifacts[0].SHA256 != hex.EncodeToString(digest[:]) {
		t.Fatalf("persisted artifacts = %+v, error = %v", storedArtifacts, err)
	}

	failedRun := createRun("run_factory_failed", "factory-failed")
	failedService := service
	failedService.Artifacts = &recordingArtifacts{}
	failedService.Executor = &resultExecutor{result: fake.JobResult{TenantID: failedRun.TenantID, RunID: failedRun.ID,
		CommitSHA: commit, ExitCode: 7, RedactedLogs: logs, LogsSHA256: hex.EncodeToString(digest[:])}}
	failed, err := failedService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: failedRun.TenantID, RunID: failedRun.ID,
		RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
	if err != nil || failed.State != domain.WorkloadFailed || failed.ErrorCode != "test_failed" {
		t.Fatalf("failed ExecuteGoTest() = %+v, error = %v", failed, err)
	}

	timedOutRun := createRun("run_factory_timed_out", "factory-timed-out")
	timeoutDigest := sha256.Sum256([]byte("test exceeded deadline\n"))
	timeoutService := service
	timeoutService.Artifacts = &recordingArtifacts{}
	timeoutService.Executor = &resultExecutor{result: fake.JobResult{TenantID: timedOutRun.TenantID, RunID: timedOutRun.ID,
		CommitSHA: commit, TimedOut: true, ExitCode: 124, RedactedLogs: []byte("test exceeded deadline\n"), LogsSHA256: hex.EncodeToString(timeoutDigest[:])}}
	timedOut, err := timeoutService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: timedOutRun.TenantID, RunID: timedOutRun.ID,
		RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
	if err != nil || timedOut.State != domain.WorkloadTimedOut || timedOut.ErrorCode != "job_timeout" {
		t.Fatalf("timed-out ExecuteGoTest() = %+v, error = %v", timedOut, err)
	}

	artifactFailRun := createRun("run_factory_artifact_failed", "factory-artifact-failed")
	artifactDigest := sha256.Sum256([]byte("test passed but artifact upload failed\n"))
	artifactFailure := errors.New("synthetic artifact storage outage")
	artifactService := service
	artifactService.Artifacts = &recordingArtifacts{err: artifactFailure}
	artifactService.Executor = &resultExecutor{result: fake.JobResult{TenantID: artifactFailRun.TenantID, RunID: artifactFailRun.ID,
		CommitSHA: commit, ExitCode: 0, RedactedLogs: []byte("test passed but artifact upload failed\n"), LogsSHA256: hex.EncodeToString(artifactDigest[:])}}
	artifactFailed, err := artifactService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: artifactFailRun.TenantID, RunID: artifactFailRun.ID,
		RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
	if !errors.Is(err, artifactFailure) || artifactFailed.State != domain.WorkloadFailed || artifactFailed.ErrorCode != "artifact_storage_error" {
		t.Fatalf("artifact outage did not fail closed: run=%+v err=%v", artifactFailed, err)
	}
	failedArtifacts, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, artifactFailRun.TenantID, artifactFailRun.ID)
	})
	if err != nil || len(failedArtifacts) != 0 {
		t.Fatalf("failed artifact upload left metadata behind: artifacts=%+v err=%v", failedArtifacts, err)
	}

	mismatchRun := createRun("run_factory_mismatch", "factory-mismatch")
	_, err = service.ExecuteGoTest(ctx, fake.JobSpec{TenantID: mismatchRun.TenantID, RunID: mismatchRun.ID,
		RepositoryPath: t.TempDir(), CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Timeout: time.Minute})
	if !errors.Is(err, ErrCommitMismatch) {
		t.Fatalf("commit mismatch error = %v, want ErrCommitMismatch", err)
	}
	unchanged, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, mismatchRun.TenantID, mismatchRun.ID)
	})
	if err != nil || unchanged.State != domain.WorkloadPending {
		t.Fatalf("commit mismatch changed run state: %+v, error = %v", unchanged, err)
	}

	cancelledRun := createRun("run_factory_cancelled", "factory-cancelled")
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.RequestCancel(ctx, tx, cancelledRun.TenantID, cancelledRun.ID, started.Add(time.Second))
	}); err != nil {
		t.Fatal(err)
	}
	executor := &resultExecutor{result: fake.JobResult{TenantID: cancelledRun.TenantID, RunID: cancelledRun.ID,
		CommitSHA: commit, ExitCode: 0, RedactedLogs: logs, LogsSHA256: hex.EncodeToString(digest[:])}}
	cancelService := service
	cancelService.Executor = executor
	_, err = cancelService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: cancelledRun.TenantID, RunID: cancelledRun.ID,
		RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
	if !errors.Is(err, ErrNotRunnable) || executor.calls != 0 {
		t.Fatalf("cancelled Workload Run executed: calls=%d error=%v", executor.calls, err)
	}

	inFlightRun := createRun("run_factory_inflight_cancelled", "factory-inflight-cancelled")
	cancelExecutor := &cancelWaitingExecutor{started: make(chan struct{}), cancel: make(chan struct{})}
	inFlightService := service
	inFlightService.Executor = cancelExecutor
	type executionOutcome struct {
		run domain.WorkloadRun
		err error
	}
	outcome := make(chan executionOutcome, 1)
	go func() {
		run, err := inFlightService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: inFlightRun.TenantID, RunID: inFlightRun.ID,
			RepositoryPath: t.TempDir(), CommitSHA: commit, Timeout: time.Minute})
		outcome <- executionOutcome{run: run, err: err}
	}()
	select {
	case <-cancelExecutor.started:
	case <-ctx.Done():
		t.Fatal("in-flight executor did not start before test deadline")
	}
	if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.RequestCancel(ctx, tx, inFlightRun.TenantID, inFlightRun.ID, started.Add(2*time.Second))
	}); err != nil {
		t.Fatalf("request in-flight Workload cancellation: %v", err)
	}
	cancelExecutor.cancel <- struct{}{}
	select {
	case result := <-outcome:
		if result.err != nil || result.run.State != domain.WorkloadCancelled || result.run.DesiredState != domain.WorkloadDesiredCancelled {
			t.Fatalf("in-flight cancellation did not become terminal after executor stopped: run=%+v err=%v", result.run, result.err)
		}
	case <-ctx.Done():
		t.Fatal("in-flight cancellation was not acknowledged before test deadline")
	}
	if inFlightArtifacts, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, inFlightRun.TenantID, inFlightRun.ID)
	}); err != nil || len(inFlightArtifacts) != 0 {
		t.Fatalf("cancelled in-flight execution unexpectedly persisted artifacts: %+v err=%v", inFlightArtifacts, err)
	}
}
