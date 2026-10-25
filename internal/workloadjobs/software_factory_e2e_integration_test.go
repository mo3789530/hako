//go:build integration

package workloadjobs

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubchecks"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	webhookstore "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

const factoryE2ESecret = "ghs_SyntheticSecretNeverPersisted0123456789"
const factoryE2EWebhookSecret = "synthetic-webhook-secret-for-local-e2e"

// TestSoftwareFactoryPodmanPostgresAndGitHubCheck exercises the local
// Software Factory path without public GitHub credentials or AWS resources:
// verified webhook inbox -> Run/Check/outbox -> isolated Podman Go test ->
// redacted artifact -> durable Check dispatcher/consumer -> fake GitHub API.
func TestSoftwareFactoryPodmanPostgresAndGitHubCheck(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	const image = "docker.io/library/golang:1.27.3-alpine"
	if err := exec.CommandContext(ctx, "podman", "image", "exists", image).Run(); err != nil {
		if os.Getenv("HAKO_E2E_REQUIRE_PODMAN") == "1" {
			t.Fatalf("required Podman image %s is unavailable", image)
		}
		t.Skipf("Podman image %s is unavailable", image)
	}

	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("migrate isolated PostgreSQL schema: %v", err)
	}
	now := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_factory_e2e','Factory E2E',$1)`,
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_factory_foreign','Factory E2E foreign Tenant',$1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_factory_e2e','factory-e2e','',$1)`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('binding_factory_e2e','tenant_factory_e2e',9700,'approved','private-ref','oauth-ref','webhook-ref',$1)`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_factory_e2e',9700,'tenant_owned','tenant_factory_e2e','factory-app','client',$1)`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_factory_e2e','tenant_factory_e2e','registration_factory_e2e',9700,'binding_factory_e2e','active',1,1,'factory-e2e-key','factory-e2e-hash','usr_factory_e2e',$1,$1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_factory_e2e',9701,'factory-owner','active','usr_factory_e2e',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9701,'tenant_factory_e2e',$1)`,
		`INSERT INTO tenant_github_app_installation_bindings (tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_factory_e2e','integration_factory_e2e',9701,$1)`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_factory_foreign',9703,'foreign-owner','active','usr_factory_e2e',$1,$1)`,
		`INSERT INTO github_app_installation_bindings (installation_id,tenant_id,bound_at) VALUES (9703,'tenant_factory_foreign',$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatalf("seed Factory E2E identity: %v", err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,factory_enabled_at,synchronized_at) VALUES ('tenant_factory_e2e',9701,9702,'factory-owner','factory-repo','main',true,$1,$2)`, now.Add(-time.Second), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,factory_enabled,synchronized_at) VALUES ('tenant_factory_foreign',9703,9704,'foreign-owner','private-repo','main',true,$1)`, now); err != nil {
		t.Fatal(err)
	}

	repository, commit := makeFactoryE2ERepository(t)
	const deliveryID = "f0f0f0f0-1234-4567-8901-abcdefabcdef"
	payload := githubWebhookFactoryPayload(commit)
	inbox := webhookstore.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	webhookHandler, err := githubwebhook.NewTenantHTTPHandler(factoryE2EWebhookCredentialResolver{},
		githubregistry.CredentialAuthorizer{Pool: pool, Policy: transaction.DefaultPolicy()}, inbox, githubwebhook.DefaultMaxBodyBytes)
	if err != nil {
		t.Fatalf("create local signed webhook ingress: %v", err)
	}
	if response := submitFactoryWebhook(webhookHandler, deliveryID, payload, factoryE2EWebhookSecret); response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate":false`) {
		t.Fatalf("accept correctly signed GitHub webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	if response := submitFactoryWebhook(webhookHandler, deliveryID, payload, factoryE2EWebhookSecret); response.Code != http.StatusAccepted || !strings.Contains(response.Body.String(), `"duplicate":true`) {
		t.Fatalf("deduplicate signed GitHub webhook retry: HTTP %d %s", response.Code, response.Body.String())
	}
	if response := submitFactoryWebhook(webhookHandler, "f1f0f0f0-1234-4567-8901-abcdefabcdef", payload, "wrong-webhook-secret"); response.Code != http.StatusUnauthorized {
		t.Fatalf("reject invalid webhook signature: HTTP %d", response.Code)
	}
	foreignInstallation := githubWebhookFactoryPayloadForInstallation(commit, 9999)
	if response := submitFactoryWebhook(webhookHandler, "f2f0f0f0-1234-4567-8901-abcdefabcdef", foreignInstallation, factoryE2EWebhookSecret); response.Code != http.StatusUnauthorized {
		t.Fatalf("reject signed webhook for an Installation not bound to this Tenant App: HTTP %d", response.Code)
	}
	changedPayload := githubWebhookFactoryPayload(strings.Repeat("b", 40))
	if response := submitFactoryWebhook(webhookHandler, deliveryID, changedPayload, factoryE2EWebhookSecret); response.Code != http.StatusConflict {
		t.Fatalf("reject Delivery ID reuse with different payload: HTTP %d %s", response.Code, response.Body.String())
	}
	creator := func(ctx context.Context, tx pgx.Tx, tenantID domain.TenantID, integrationID, deliveryID, correlationID string, createdAt time.Time) (bool, error) {
		return CreateWebhookRepositoryRun(ctx, tx, tenantID, integrationID, deliveryID, correlationID, createdAt)
	}
	batch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second), creator)
	if err != nil || batch.Processed != 1 || batch.WorkloadRunsCreated != 1 {
		t.Fatalf("process signed repository event into Workload Run: stats=%+v err=%v", batch, err)
	}
	var runID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e'`).Scan(&runID); err != nil {
		t.Fatalf("find Workload Run: %v", err)
	}
	run, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(runID))
	})
	if err != nil || run.CommitSHA != commit || run.GitHubRepository != 9702 || run.TrustLevel != domain.WorkloadUntrusted {
		t.Fatalf("webhook Run did not retain immutable repository identity (run=%s): %+v err=%v", runID, run, err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_foreign", domain.WorkloadRunID(runID))
	}); !errors.Is(err, workloads.ErrNotFound) {
		t.Fatalf("Factory Run was visible from a different Tenant: err=%v", err)
	}
	requesterView, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (struct {
		runs  []domain.WorkloadRun
		total int64
	}, error) {
		runs, total, err := workloads.List(ctx, tx, run.TenantID, "usr_factory_unrelated", 100, 0)
		return struct {
			runs  []domain.WorkloadRun
			total int64
		}{runs: runs, total: total}, err
	})
	if err != nil || requesterView.total != 0 || len(requesterView.runs) != 0 {
		t.Fatalf("Factory Run was visible to an unrelated requester: total=%d runs=%+v err=%v", requesterView.total, requesterView.runs, err)
	}
	const foreignRepositoryDeliveryID = "f5f0f0f0-1234-4567-8901-abcdefabcdef"
	foreignRepositoryPayload := githubWebhookFactoryPayloadForRepository(commit, 9701, 9704, "foreign-owner", "private-repo")
	if response := submitFactoryWebhook(webhookHandler, foreignRepositoryDeliveryID, foreignRepositoryPayload, factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept validly signed delivery referencing a foreign Tenant repository: HTTP %d %s", response.Code, response.Body.String())
	}
	foreignRepositoryBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(1500*time.Millisecond), creator)
	if err != nil || foreignRepositoryBatch.Processed != 1 || foreignRepositoryBatch.WorkloadRunsCreated != 0 {
		t.Fatalf("foreign Tenant repository delivery was not safely ignored: stats=%+v err=%v", foreignRepositoryBatch, err)
	}
	var foreignRepositoryRuns int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND commit_sha=$1`, commit).Scan(&foreignRepositoryRuns); err != nil || foreignRepositoryRuns != 1 {
		t.Fatalf("foreign Tenant repository event created or altered a Run: count=%d err=%v", foreignRepositoryRuns, err)
	}

	logs := &recordingArtifacts{}
	jobRuntime, err := fake.NewJobRuntime(fake.JobRuntimeConfig{GoImage: image})
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Pool: pool, Policy: transaction.DefaultPolicy(), Executor: jobRuntime, Artifacts: logs}
	finished, err := service.ExecuteGoTest(ctx, fake.JobSpec{TenantID: run.TenantID, RunID: run.ID,
		RepositoryPath: repository, CommitSHA: commit, Timeout: 90 * time.Second, SecretValues: []string{factoryE2ESecret}})
	if err != nil || finished.State != domain.WorkloadSucceeded {
		t.Fatalf("execute isolated Podman test for Run %s: state=%s err=%v redacted_logs=%q", runID, finished.State, err, logs.logs)
	}
	if len(logs.logs) == 0 || strings.Contains(string(logs.logs), factoryE2ESecret) {
		t.Fatalf("redacted log artifact is empty or exposes a credential for Run %s", runID)
	}
	artifacts, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, run.TenantID, run.ID)
	})
	if err != nil || len(artifacts) != 1 || !artifacts[0].Redacted || artifacts[0].SHA256 != logs.sha {
		t.Fatalf("persisted log artifact is not redacted and digest-bound for Run %s: %+v err=%v", runID, artifacts, err)
	}
	assertFactoryE2EContainerCleaned(t, run.TenantID, run.ID)

	// Simulate a replacement worker receiving the same pending delivery after
	// the terminal DB commit. It has no process-local result cache; durable Run
	// state must prevent a second container from starting.
	restartedRuntime, err := fake.NewJobRuntime(fake.JobRuntimeConfig{GoImage: image})
	if err != nil {
		t.Fatal(err)
	}
	restartedService := service
	restartedService.Executor = restartedRuntime
	if _, err := restartedService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: run.TenantID, RunID: run.ID,
		RepositoryPath: repository, CommitSHA: commit, Timeout: 90 * time.Second}); !errors.Is(err, ErrNotRunnable) {
		t.Fatalf("replacement worker re-executed terminal Run: err=%v", err)
	}
	if _, ok := restartedRuntime.Result(run.TenantID, run.ID); ok {
		t.Fatal("replacement worker started a second Podman Job for a terminal Run")
	}
	assertFactoryE2EContainerCleaned(t, run.TenantID, run.ID)

	// A second signed delivery exercises a real failing test through the same
	// Tenant trigger and runtime path, rather than only unit-testing result map.
	failureRepository, failureCommit := makeFactoryE2ERepositoryFromSource(t,
		"package factory\n\nimport \"testing\"\n\nfunc TestFactory(t *testing.T) { t.Fatalf(\"credential=%s\", "+strconv.Quote(factoryE2ESecret)+") }\n")
	const failureDeliveryID = "f3f0f0f0-1234-4567-8901-abcdefabcdef"
	if response := submitFactoryWebhook(webhookHandler, failureDeliveryID, githubWebhookFactoryPayload(failureCommit), factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept signed failure-case webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	failureBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(2*time.Second), creator)
	if err != nil || failureBatch.Processed != 1 || failureBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process failure-case webhook into Workload Run: stats=%+v err=%v", failureBatch, err)
	}
	var failureRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND commit_sha=$1`, failureCommit).Scan(&failureRunID); err != nil {
		t.Fatalf("find failure-case Workload Run: %v", err)
	}
	failureRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(failureRunID))
	})
	if err != nil || failureRun.CommitSHA != failureCommit || failureRun.TrustLevel != domain.WorkloadUntrusted {
		t.Fatalf("failure-case Run lost its immutable identity: %+v err=%v", failureRun, err)
	}
	failureLogs := &recordingArtifacts{}
	failureRuntime, err := fake.NewJobRuntime(fake.JobRuntimeConfig{GoImage: image})
	if err != nil {
		t.Fatal(err)
	}
	failureService := Service{Pool: pool, Policy: transaction.DefaultPolicy(), Executor: failureRuntime, Artifacts: failureLogs}
	failed, err := failureService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: failureRun.TenantID, RunID: failureRun.ID,
		RepositoryPath: failureRepository, CommitSHA: failureCommit, Timeout: 90 * time.Second, SecretValues: []string{factoryE2ESecret}})
	if err != nil || failed.State != domain.WorkloadFailed || failed.ErrorCode != "test_failed" {
		t.Fatalf("execute failing Podman test: state=%s code=%s err=%v logs=%q", failed.State, failed.ErrorCode, err, failureLogs.logs)
	}
	if len(failureLogs.logs) == 0 || strings.Contains(string(failureLogs.logs), factoryE2ESecret) || !strings.Contains(string(failureLogs.logs), "[REDACTED]") {
		t.Fatalf("failure artifact is empty or not redacted for Run %s: %q", failureRunID, failureLogs.logs)
	}
	assertFactoryE2EContainerCleaned(t, failureRun.TenantID, failureRun.ID)

	// Cancellation before dispatch must become terminal without creating an
	// attempt, selecting a placement, or starting the local Runtime.
	const cancelDeliveryID = "f4f0f0f0-1234-4567-8901-abcdefabcdef"
	if response := submitFactoryWebhook(webhookHandler, cancelDeliveryID, githubWebhookFactoryPayload(commit), factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept signed cancel-case webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	cancelBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(3*time.Second), creator)
	if err != nil || cancelBatch.Processed != 1 || cancelBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process cancel-case webhook into Workload Run: stats=%+v err=%v", cancelBatch, err)
	}
	cancelKey := workloads.WebhookDeliveryIdempotencyKey("integration_factory_e2e", cancelDeliveryID)
	var cancelRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND idempotency_key=$1`, cancelKey).Scan(&cancelRunID); err != nil {
		t.Fatalf("find cancel-case Workload Run: %v", err)
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.RequestCancel(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(cancelRunID), now.Add(4*time.Second))
	}); err != nil {
		t.Fatalf("request pre-dispatch cancellation: %v", err)
	}
	placementCalls := 0
	preDispatch := DispatchHandler{Pool: pool, Policy: transaction.DefaultPolicy(), Now: func() time.Time { return now.Add(5 * time.Second) },
		Placement: PlacementSelectorFunc(func(context.Context, domain.WorkloadRun) (domain.ResourcePlaneID, error) {
			placementCalls++
			return "rp_factory_e2e", nil
		})}
	if err := preDispatch.Handle(ctx, ScheduleRequest{SchemaVersion: 1, TenantID: "tenant_factory_e2e", WorkloadRunID: domain.WorkloadRunID(cancelRunID)}); err != nil {
		t.Fatalf("consume cancelled scheduler delivery: %v", err)
	}
	cancelled, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(cancelRunID))
	})
	if err != nil || cancelled.State != domain.WorkloadCancelled || cancelled.DesiredState != domain.WorkloadDesiredCancelled || placementCalls != 0 {
		t.Fatalf("pre-dispatch cancellation was not terminal without placement: run=%+v placement calls=%d err=%v", cancelled, placementCalls, err)
	}
	var cancelAttempts int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_run_attempts WHERE tenant_id='tenant_factory_e2e' AND workload_run_id=$1`, cancelRunID).Scan(&cancelAttempts); err != nil || cancelAttempts != 0 {
		t.Fatalf("cancelled pre-dispatch Run created an attempt: count=%d err=%v", cancelAttempts, err)
	}

	// A separate signed delivery exercises cancellation after the Run has
	// entered running. RequestCancel advances its revision; the executor must
	// confirm stop before the service records the terminal Check outcome.
	const inFlightCancelDeliveryID = "f6f0f0f0-1234-4567-8901-abcdefabcdef"
	if response := submitFactoryWebhook(webhookHandler, inFlightCancelDeliveryID, githubWebhookFactoryPayload(commit), factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept signed in-flight-cancel webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	inFlightCancelBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(3500*time.Millisecond), creator)
	if err != nil || inFlightCancelBatch.Processed != 1 || inFlightCancelBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process in-flight-cancel webhook into Workload Run: stats=%+v err=%v", inFlightCancelBatch, err)
	}
	inFlightCancelKey := workloads.WebhookDeliveryIdempotencyKey("integration_factory_e2e", inFlightCancelDeliveryID)
	var inFlightCancelRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND idempotency_key=$1`, inFlightCancelKey).Scan(&inFlightCancelRunID); err != nil {
		t.Fatalf("find in-flight-cancel Workload Run: %v", err)
	}
	inFlightRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(inFlightCancelRunID))
	})
	if err != nil || inFlightRun.State != domain.WorkloadPending || inFlightRun.CommitSHA != commit {
		t.Fatalf("in-flight-cancel Run lost its immutable identity: %+v err=%v", inFlightRun, err)
	}
	cancelExecutor := &cancelWaitingExecutor{started: make(chan struct{}), cancel: make(chan struct{})}
	cancelService := service
	cancelService.Executor = cancelExecutor
	type inFlightOutcome struct {
		run domain.WorkloadRun
		err error
	}
	inFlightOutcomeCh := make(chan inFlightOutcome, 1)
	go func() {
		run, err := cancelService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: inFlightRun.TenantID, RunID: inFlightRun.ID,
			RepositoryPath: repository, CommitSHA: commit, Timeout: 90 * time.Second})
		inFlightOutcomeCh <- inFlightOutcome{run: run, err: err}
	}()
	select {
	case <-cancelExecutor.started:
	case <-ctx.Done():
		t.Fatal("in-flight Factory executor did not start before test deadline")
	}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.RequestCancel(ctx, tx, inFlightRun.TenantID, inFlightRun.ID, now.Add(4*time.Second))
	}); err != nil {
		t.Fatalf("request in-flight Factory cancellation: %v", err)
	}
	cancelExecutor.cancel <- struct{}{}
	select {
	case result := <-inFlightOutcomeCh:
		if result.err != nil || result.run.State != domain.WorkloadCancelled || result.run.DesiredState != domain.WorkloadDesiredCancelled {
			t.Fatalf("in-flight Factory cancellation did not become terminal after executor stop: run=%+v err=%v", result.run, result.err)
		}
	case <-ctx.Done():
		t.Fatal("in-flight Factory cancellation was not acknowledged before test deadline")
	}

	// Timeout and artifact-storage failure should also flow from signed
	// repository events into durable terminal Checks without using Podman.
	const timeoutDeliveryID = "f7f0f0f0-1234-4567-8901-abcdefabcdef"
	if response := submitFactoryWebhook(webhookHandler, timeoutDeliveryID, githubWebhookFactoryPayload(commit), factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept signed timeout-case webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	timeoutBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(4500*time.Millisecond), creator)
	if err != nil || timeoutBatch.Processed != 1 || timeoutBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process timeout-case webhook into Workload Run: stats=%+v err=%v", timeoutBatch, err)
	}
	timeoutKey := workloads.WebhookDeliveryIdempotencyKey("integration_factory_e2e", timeoutDeliveryID)
	var timeoutRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND idempotency_key=$1`, timeoutKey).Scan(&timeoutRunID); err != nil {
		t.Fatalf("find timeout-case Workload Run: %v", err)
	}
	timeoutRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(timeoutRunID))
	})
	if err != nil {
		t.Fatalf("load timeout-case Workload Run: %v", err)
	}
	timeoutLogs := []byte("synthetic test timeout\n")
	timeoutDigest := sha256.Sum256(timeoutLogs)
	timeoutArtifacts := &recordingArtifacts{}
	timeoutService := service
	timeoutService.Artifacts = timeoutArtifacts
	timeoutService.Executor = &resultExecutor{result: fake.JobResult{TenantID: timeoutRun.TenantID, RunID: timeoutRun.ID,
		CommitSHA: timeoutRun.CommitSHA, ExitCode: 124, TimedOut: true, RedactedLogs: timeoutLogs, LogsSHA256: hex.EncodeToString(timeoutDigest[:])}}
	timedOut, err := timeoutService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: timeoutRun.TenantID, RunID: timeoutRun.ID,
		RepositoryPath: repository, CommitSHA: commit, Timeout: 90 * time.Second})
	if err != nil || timedOut.State != domain.WorkloadTimedOut || timedOut.ErrorCode != "job_timeout" {
		t.Fatalf("execute timeout-case Run: state=%s code=%s err=%v", timedOut.State, timedOut.ErrorCode, err)
	}

	const artifactFailureDeliveryID = "f8f0f0f0-1234-4567-8901-abcdefabcdef"
	if response := submitFactoryWebhook(webhookHandler, artifactFailureDeliveryID, githubWebhookFactoryPayload(commit), factoryE2EWebhookSecret); response.Code != http.StatusAccepted {
		t.Fatalf("accept signed artifact-failure webhook: HTTP %d %s", response.Code, response.Body.String())
	}
	artifactFailureBatch, err := webhookstore.ProcessBatchWithRepositoryTriggers(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(5500*time.Millisecond), creator)
	if err != nil || artifactFailureBatch.Processed != 1 || artifactFailureBatch.WorkloadRunsCreated != 1 {
		t.Fatalf("process artifact-failure webhook into Workload Run: stats=%+v err=%v", artifactFailureBatch, err)
	}
	artifactFailureKey := workloads.WebhookDeliveryIdempotencyKey("integration_factory_e2e", artifactFailureDeliveryID)
	var artifactFailureRunID string
	if err := pool.QueryRow(ctx, `SELECT id FROM workload_runs WHERE tenant_id='tenant_factory_e2e' AND idempotency_key=$1`, artifactFailureKey).Scan(&artifactFailureRunID); err != nil {
		t.Fatalf("find artifact-failure Workload Run: %v", err)
	}
	artifactFailureRun, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		return workloads.Get(ctx, tx, "tenant_factory_e2e", domain.WorkloadRunID(artifactFailureRunID))
	})
	if err != nil {
		t.Fatalf("load artifact-failure Workload Run: %v", err)
	}
	artifactFailureError := errors.New("synthetic artifact storage outage")
	artifactFailureService := service
	artifactFailureService.Artifacts = &recordingArtifacts{err: artifactFailureError}
	artifactFailureService.Executor = &resultExecutor{result: fake.JobResult{TenantID: artifactFailureRun.TenantID, RunID: artifactFailureRun.ID,
		CommitSHA: artifactFailureRun.CommitSHA, ExitCode: 0, RedactedLogs: []byte("test passed; artifact unavailable\n"),
		LogsSHA256: fmt.Sprintf("%x", sha256.Sum256([]byte("test passed; artifact unavailable\n")))}}
	artifactFailed, artifactErr := artifactFailureService.ExecuteGoTest(ctx, fake.JobSpec{TenantID: artifactFailureRun.TenantID, RunID: artifactFailureRun.ID,
		RepositoryPath: repository, CommitSHA: commit, Timeout: 90 * time.Second})
	if !errors.Is(artifactErr, artifactFailureError) || artifactFailed.State != domain.WorkloadFailed || artifactFailed.ErrorCode != "artifact_storage_error" {
		t.Fatalf("execute artifact-failure Run: state=%s code=%s err=%v", artifactFailed.State, artifactFailed.ErrorCode, artifactErr)
	}
	artifactFailureMetadata, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) ([]domain.WorkloadArtifact, error) {
		return workloads.ListArtifacts(ctx, tx, artifactFailureRun.TenantID, artifactFailureRun.ID)
	})
	if err != nil || len(artifactFailureMetadata) != 0 {
		t.Fatalf("artifact storage failure left metadata behind: %+v err=%v", artifactFailureMetadata, err)
	}

	// Only the terminal Check revision is needed here; the Run lifecycle
	// transitions have their own Check outbox entries and earlier revisions.
	dispatchNow := time.Now().UTC().Add(time.Minute)
	if _, err := pool.Exec(ctx, `UPDATE outbox_events e SET published_at=$1 WHERE e.event_type <> 'workload_run.github_check_requested' OR NOT EXISTS (
		SELECT 1 FROM workload_github_checks c WHERE c.workload_run_id=e.aggregate_id
		AND c.revision=(e.payload_json::jsonb->>'revision')::bigint)`, dispatchNow); err != nil {
		t.Fatalf("prepare final Check outbox event: %v", err)
	}
	checkQueue := &factoryCheckQueue{}
	dispatch, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, factoryCheckPublisher{queue: checkQueue},
		map[string]string{"rp-factory-test": "https://sqs.invalid/rp"}, dispatcher.Config{GitHubChecksQueueURL: "https://sqs.invalid/checks", Now: func() time.Time { return dispatchNow.Add(time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := dispatch.RunOnce(ctx)
	if err != nil || dispatched.Published != 6 || len(checkQueue.deliveries) != 6 {
		t.Fatalf("dispatch final Check request for Run %s: stats=%+v err=%v", runID, dispatched, err)
	}
	fakeAPI := &factoryGitHubAPI{checks: make(map[int64]githubapp.CheckRun), failNext: 1}
	client, err := githubapp.NewChecksClient(&http.Client{Transport: factoryRoundTripper{handler: http.HandlerFunc(fakeAPI.serveHTTP)}})
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := githubchecks.New(checkQueue, githubchecks.DBStore{Pool: pool, Policy: transaction.DefaultPolicy()},
		factoryCheckCredentials{}, client, githubchecks.Config{BatchSize: 1, Now: func() time.Time { return now.Add(4 * time.Second) }})
	if err != nil {
		t.Fatal(err)
	}
	firstStats, firstErr := consumer.RunOnce(ctx)
	if firstErr == nil || firstStats.Received != 1 || firstStats.Applied != 0 || firstStats.Failed != 1 || len(checkQueue.deliveries) != 6 {
		t.Fatalf("transient GitHub outage should leave only its Check delivery for retry: stats=%+v pending=%d err=%v", firstStats, len(checkQueue.deliveries), firstErr)
	}
	for remaining := 6; remaining > 0; remaining-- {
		retryStats, err := consumer.RunOnce(ctx)
		if err != nil || retryStats.Received != 1 || retryStats.Applied != 1 || len(checkQueue.deliveries) != remaining-1 {
			t.Fatalf("retry one GitHub Check with batch size 1: stats=%+v pending=%d err=%v", retryStats, len(checkQueue.deliveries), err)
		}
	}
	if len(fakeAPI.checks) != 6 || len(checkQueue.deleted) != 6 || len(checkQueue.deliveries) != 0 {
		t.Fatalf("GitHub Check retries did not acknowledge each receipt independently: checks=%d deleted=%d pending=%d", len(fakeAPI.checks), len(checkQueue.deleted), len(checkQueue.deliveries))
	}
	gotSuccess, gotFailure, gotPreDispatchCancelled, gotInFlightCancelled, gotTimedOut, gotArtifactFailed := false, false, false, false, false, false
	for _, check := range fakeAPI.checks {
		if check.Status != githubapp.CheckCompleted {
			t.Fatalf("fake GitHub Check is not terminal: %+v", check)
		}
		switch check.HeadSHA {
		case commit:
			if check.ExternalID == cancelRunID {
				gotPreDispatchCancelled = check.Conclusion == githubapp.CheckRunConclusion("cancelled")
			} else if check.ExternalID == inFlightCancelRunID {
				gotInFlightCancelled = check.Conclusion == githubapp.CheckRunConclusion("cancelled")
			} else if check.ExternalID == timeoutRunID {
				gotTimedOut = check.Conclusion == githubapp.CheckRunConclusion("timed_out")
			} else if check.ExternalID == artifactFailureRunID {
				gotArtifactFailed = check.Conclusion == githubapp.CheckRunConclusion("failure")
			} else {
				gotSuccess = check.Conclusion == githubapp.CheckRunConclusion("success")
			}
		case failureCommit:
			gotFailure = check.Conclusion == githubapp.CheckRunConclusion("failure")
		default:
			t.Fatalf("fake GitHub Check references an unexpected commit: %+v", check)
		}
	}
	if !gotSuccess || !gotFailure || !gotPreDispatchCancelled || !gotInFlightCancelled || !gotTimedOut || !gotArtifactFailed || len(fakeAPI.requests) != 6 {
		t.Fatalf("fake GitHub API did not receive success, failure, pre-dispatch/in-flight cancel, timeout, and artifact failure outcomes: %+v", fakeAPI.checks)
	}
	for _, request := range fakeAPI.requests {
		if strings.Contains(request.Output.Summary, factoryE2ESecret) || strings.Contains(request.Output.Summary, factoryE2EWebhookSecret) {
			t.Fatalf("fake GitHub API received a Check summary with a secret: %+v", request)
		}
	}
	combinedLogs := append(append([]byte(nil), logs.logs...), failureLogs.logs...)
	var artifactCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_artifacts WHERE tenant_id='tenant_factory_e2e' AND workload_run_id=$1 AND redacted=true`, failureRunID).Scan(&artifactCount); err != nil || artifactCount != 1 {
		t.Fatalf("failure artifact metadata was not persisted redacted: count=%d err=%v", artifactCount, err)
	}
	assertFactorySecretAbsent(t, ctx, pool, checkQueue, fakeAPI, combinedLogs)
}

func assertFactoryE2EContainerCleaned(t *testing.T, tenantID domain.TenantID, runID domain.WorkloadRunID) {
	t.Helper()
	command := exec.Command("podman", "ps", "--all", "--quiet",
		"--filter", "label=hako.tenant-id="+string(tenantID),
		"--filter", "label=hako.workload-run-id="+string(runID))
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("inspect terminal Run Podman containers: %v: %s", err, strings.TrimSpace(string(output)))
	}
	if strings.TrimSpace(string(output)) != "" {
		t.Fatalf("terminal Run still has a Podman container after worker completion: %s", strings.TrimSpace(string(output)))
	}
}

func makeFactoryE2ERepository(t *testing.T) (string, string) {
	return makeFactoryE2ERepositoryFromSource(t, "package factory\n\nimport \"testing\"\n\nfunc TestFactory(t *testing.T) {}\n")
}

func makeFactoryE2ERepositoryFromSource(t *testing.T, source string) (string, string) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "hako-factory-e2e-")
	if err != nil {
		t.Fatalf("create temporary Repository checkout: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module example.invalid/factory-e2e\n\ngo 1.25.0\n"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "factory_test.go"), []byte(source), 0644); err != nil {
		t.Fatal(err)
	}
	for _, filename := range []string{"go.mod", "factory_test.go"} {
		if err := os.Chmod(filepath.Join(dir, filename), 0644); err != nil {
			t.Fatalf("make temporary checkout file readable by the container user: %v", err)
		}
	}
	commands := [][]string{{"init", "-b", "main"}, {"-c", "user.name=Hako E2E", "-c", "user.email=hako-e2e@example.invalid", "add", "go.mod", "factory_test.go"}, {"-c", "user.name=Hako E2E", "-c", "user.email=hako-e2e@example.invalid", "commit", "-m", "factory e2e fixture"}, {"rev-parse", "HEAD"}}
	for index, args := range commands {
		command := exec.Command("git", append([]string{"-C", dir}, args...)...)
		output, err := command.CombinedOutput()
		if err != nil {
			t.Fatalf("prepare immutable GitHub fixture (step %d): %v: %s", index+1, err, strings.TrimSpace(string(output)))
		}
		if index == len(commands)-1 {
			if err := os.Chmod(dir, 0755); err != nil {
				t.Fatalf("make temporary checkout traversable by the unprivileged container user: %v", err)
			}
			return dir, strings.TrimSpace(string(output))
		}
	}
	return "", ""
}

func githubWebhookFactoryPayload(commit string) []byte {
	return githubWebhookFactoryPayloadForRepository(commit, 9701, 9702, "factory-owner", "factory-repo")
}

func githubWebhookFactoryPayloadForInstallation(commit string, installationID int64) []byte {
	return githubWebhookFactoryPayloadForRepository(commit, installationID, 9702, "factory-owner", "factory-repo")
}

func githubWebhookFactoryPayloadForRepository(commit string, installationID, repositoryID int64, owner, name string) []byte {
	return []byte(fmt.Sprintf(`{"ref":"refs/heads/main","after":%q,"installation":{"id":%d},"repository":{"id":%d,"name":%q,"default_branch":"main","owner":{"login":%q}}}`, commit, installationID, repositoryID, name, owner))
}

func submitFactoryWebhook(handler http.Handler, deliveryID string, payload []byte, signingSecret string) *httptest.ResponseRecorder {
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write(payload)
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations/github/apps/integration_factory_e2e/webhook", strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}

type factoryE2EWebhookCredentialResolver struct{}

func (factoryE2EWebhookCredentialResolver) ResolveWebhookCredential(_ context.Context, integrationID string) (githubwebhook.WebhookCredential, error) {
	if integrationID != "integration_factory_e2e" {
		return githubwebhook.WebhookCredential{}, errors.New("unknown synthetic integration")
	}
	return githubwebhook.WebhookCredential{IntegrationID: integrationID, Generation: 1, CurrentSecret: []byte(factoryE2EWebhookSecret)}, nil
}

type factoryCheckQueue struct {
	deliveries []githubchecks.Delivery
	deleted    []string
}

func (q *factoryCheckQueue) Receive(_ context.Context, max int, _ time.Duration) ([]githubchecks.Delivery, error) {
	if max < len(q.deliveries) {
		return append([]githubchecks.Delivery(nil), q.deliveries[:max]...), nil
	}
	return append([]githubchecks.Delivery(nil), q.deliveries...), nil
}
func (*factoryCheckQueue) ChangeVisibility(context.Context, string, time.Duration) error { return nil }
func (q *factoryCheckQueue) Delete(_ context.Context, receipt string) error {
	q.deleted = append(q.deleted, receipt)
	remaining := q.deliveries[:0]
	for _, delivery := range q.deliveries {
		if delivery.ReceiptHandle != receipt {
			remaining = append(remaining, delivery)
		}
	}
	q.deliveries = remaining
	return nil
}

type factoryCheckPublisher struct{ queue *factoryCheckQueue }

func (p factoryCheckPublisher) Publish(_ context.Context, _ string, event outbox.Event) error {
	p.queue.deliveries = append(p.queue.deliveries, githubchecks.Delivery{Body: append([]byte(nil), event.Payload...), ReceiptHandle: event.ID})
	return nil
}

type factoryCheckToken struct{}

func (factoryCheckToken) Value() string        { return "synthetic-check-token" }
func (factoryCheckToken) ExpiresAt() time.Time { return time.Now().Add(time.Hour) }

type factoryCheckCredentials struct{}

func (factoryCheckCredentials) ChecksRunToken(_ context.Context, run domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	if run.TenantID != "tenant_factory_e2e" || run.GitHubRepository != 9702 {
		return nil, githubapp.RepositoryAccess{}, errors.New("unexpected Tenant or repository")
	}
	return factoryCheckToken{}, githubapp.RepositoryAccess{Owner: "factory-owner", Name: "factory-repo"}, nil
}

func (factoryCheckCredentials) ActionsRunnerChecksToken(context.Context, string, int64, int64) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	return nil, githubapp.RepositoryAccess{}, errors.New("unexpected Actions Runner Check credential lookup")
}

type factoryRoundTripper struct{ handler http.Handler }

func (r factoryRoundTripper) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	r.handler.ServeHTTP(recorder, request)
	return &http.Response{StatusCode: recorder.Code, Header: recorder.Header(), Body: io.NopCloser(strings.NewReader(recorder.Body.String())), Request: request}, nil
}

type factoryGitHubAPI struct {
	checks   map[int64]githubapp.CheckRun
	requests []githubapp.CheckRunRequest
	nextID   int64
	failNext int
}

func (api *factoryGitHubAPI) serveHTTP(w http.ResponseWriter, request *http.Request) {
	if request.Header.Get("Authorization") != "Bearer synthetic-check-token" || !strings.Contains(request.URL.Path, "/repos/factory-owner/factory-repo/check-runs") {
		http.Error(w, "unexpected destination", http.StatusForbidden)
		return
	}
	if api.failNext > 0 {
		api.failNext--
		http.Error(w, "synthetic temporary GitHub outage", http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	switch request.Method {
	case http.MethodGet:
		var found []githubapp.CheckRun
		for _, check := range api.checks {
			if check.HeadSHA == request.URL.Query().Get("head_sha") {
				found = append(found, check)
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"check_runs": found})
	case http.MethodPost, http.MethodPatch:
		var input githubapp.CheckRunRequest
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			http.Error(w, "invalid check request", http.StatusBadRequest)
			return
		}
		api.requests = append(api.requests, input)
		if api.nextID < 9703 {
			api.nextID = 9703
		}
		id := api.nextID
		if request.Method == http.MethodPatch {
			pathID, unescapeErr := url.PathUnescape(request.URL.Path[strings.LastIndex(request.URL.Path, "/check-runs/")+len("/check-runs/"):])
			checkID, parseErr := strconv.ParseInt(pathID, 10, 64)
			if unescapeErr != nil || parseErr != nil || checkID != 9703 {
				http.Error(w, "unexpected check ID", http.StatusNotFound)
				return
			}
			id = checkID
		}
		check := githubapp.CheckRun{ID: id, Name: input.Name, ExternalID: input.ExternalID, HeadSHA: input.HeadSHA,
			Status: input.Status, Conclusion: input.Conclusion}
		api.checks[id] = check
		if request.Method == http.MethodPost {
			api.nextID++
			w.WriteHeader(http.StatusCreated)
		}
		_ = json.NewEncoder(w).Encode(check)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func assertFactorySecretAbsent(t *testing.T, ctx context.Context, pool *pgxpool.Pool, queue *factoryCheckQueue, api *factoryGitHubAPI, artifact []byte) {
	t.Helper()
	var databaseDump string
	rows, err := pool.Query(ctx, `SELECT payload_json::text FROM outbox_events UNION ALL SELECT payload_json::text FROM github_webhook_deliveries UNION ALL SELECT storage_ref FROM workload_artifacts UNION ALL SELECT output_title || output_summary FROM workload_github_checks`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			t.Fatal(err)
		}
		databaseDump += value
	}
	if rows.Err() != nil {
		t.Fatal(rows.Err())
	}
	serializedQueue, _ := json.Marshal(queue.deliveries)
	serializedChecks, _ := json.Marshal(api.checks)
	for _, secret := range []string{factoryE2ESecret, factoryE2EWebhookSecret} {
		for label, value := range map[string]string{"database": databaseDump, "queue": string(serializedQueue), "Check": string(serializedChecks), "artifact": string(artifact)} {
			if strings.Contains(value, secret) {
				t.Errorf("synthetic credential leaked into %s", label)
			}
		}
	}
}
