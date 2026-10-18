//go:build integration

package actionsrunner

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

// This composes authorized queued admission, the startup worker, observed runner
// status, terminal Check state, Broker HTTP cleanup leases and cleanup recovery.
// External providers are fakes; HTTP signature ingress and outgoing Checks API
// delivery are verified by separate integrations.
func TestActionsRunnerTerminalWebhookCleanupRecovery(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pool := testutil.NewIsolatedPostgres(t)
			if err := dsql.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_cleanup','Cleanup',now())`,
				`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_cleanup','cleanup','',now())`,
				`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_cleanup',8801,'acme','active','usr_cleanup',now(),now())`,
				`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8801,'tenant_cleanup',now())`,
				`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_cleanup',8801,8802,'acme','api','main',now(),true,now()-interval '1 minute')`,
			} {
				if _, err := pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
			inbox := githubwebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
			queuedPayload := []byte(fmt.Sprintf(`{"action":"queued","installation":{"id":8801},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"queued","labels":["self-hosted","hako"]}}`, strings.Repeat("a", 40)))
			if inserted, err := inbox.Record(ctx, hakowebhook.VerifiedDelivery{DeliveryID: "043b2c3d-4e5f-4789-8abc-def012345679", Event: "workflow_job", Action: "queued", Payload: queuedPayload}, now); err != nil || !inserted {
				t.Fatalf("record authorized queued workflow job: inserted=%v err=%v", inserted, err)
			}
			admission, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(time.Second), nil)
			if err != nil || admission.ActionsRunnerJobsAdmitted != 1 {
				t.Fatalf("admit authorized workflow job: stats=%+v err=%v", admission, err)
			}
			claimTx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claimed, found, err := githubwebhook.ClaimNextActionsRunnerJob(ctx, claimTx, "runner-cleanup-claim", now.Add(2*time.Second), 5*time.Minute)
			if err != nil || !found || claimed.State != "claimed" || claimed.TenantID != "tenant_cleanup" || claimed.JobID != 9901 {
				_ = claimTx.Rollback(ctx)
				t.Fatalf("claim authorized workflow job: job=%+v found=%v err=%v", claimed, found, err)
			}
			if err := claimTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			// Model the Resource Plane launch broker's atomic claimed→starting
			// transition, then run the real startup worker over injected providers.
			launchAt := time.Now().UTC()
			if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='starting',resource_plane_id='rp-tokyo-01',updated_at=$1
				WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1 AND state='claimed' AND claim_id=$2`, launchAt, claimed.ClaimID); err != nil {
				t.Fatal(err)
			}
			request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: claimed.IntegrationID, WorkflowJobID: claimed.JobID, RunAttempt: claimed.RunAttempt, ClaimID: claimed.ClaimID}
			command := githubbroker.ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: claimed.IntegrationID, TenantID: domain.TenantID(claimed.TenantID),
				Installation: claimed.Installation, Repository: claimed.Repository, WorkflowJobID: claimed.JobID, WorkflowRunID: claimed.WorkflowRunID,
				RunAttempt: claimed.RunAttempt, WorkflowName: claimed.WorkflowName, JobName: claimed.JobName, HeadBranch: claimed.HeadBranch, HeadSHA: claimed.HeadSHA,
				Labels: append([]string(nil), claimed.Labels...), ClaimID: claimed.ClaimID, LeaseExpires: claimed.LeaseExpires, Owner: "acme", Name: "api", State: "starting"}
			instance := LambdaRunnerInstance{ResourcePlaneID: "rp-tokyo-01", MicroVMID: "microvm-cleanup", Endpoint: "https://microvm-cleanup.lambda-microvm.ap-northeast-1.on.aws"}
			runnerToken, err := githubapp.NewRunnerRegistrationToken("synthetic-github-runner-token", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			events := []string{}
			startup, err := NewRunnerStartupWorker(RunnerStartupWorkerConfig{OperationTimeout: time.Minute}, startBrokerStub{command: command, events: &events},
				runnerLauncherStub{instance: instance, events: &events}, postgresRunnerBinder{pool: pool},
				runnerTokenIssuerStub{grant: githubbroker.ActionsRunnerRegistrationGrant{Token: runnerToken, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, events: &events},
				&guestBootstrapStub{events: &events}, startupFailureStub{events: &events}, "rp-tokyo-01")
			if err != nil {
				t.Fatal(err)
			}
			if err := startup.Start(ctx, request); err != nil || !strings.Contains(strings.Join(events, ","), "bootstrap") {
				t.Fatalf("start authorized workflow runner: events=%v err=%v", events, err)
			}
			inProgressPayload := []byte(fmt.Sprintf(`{"action":"in_progress","installation":{"id":8801},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"in_progress","labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-9901-1"}}`, strings.Repeat("a", 40)))
			if inserted, err := inbox.Record(ctx, hakowebhook.VerifiedDelivery{DeliveryID: "143b2c3d-4e5f-4789-8abc-def012345678", Event: "workflow_job", Action: "in_progress", Payload: inProgressPayload}, now.Add(3*time.Second)); err != nil || !inserted {
				t.Fatalf("record GitHub runner assignment: inserted=%v err=%v", inserted, err)
			}
			statusStats, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now.Add(4*time.Second), nil)
			if err != nil || statusStats.Processed != 1 {
				t.Fatalf("apply GitHub runner assignment: stats=%+v err=%v", statusStats, err)
			}
			payload := []byte(fmt.Sprintf(`{"action":"completed","installation":{"id":8801},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"completed","conclusion":%q,"labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-9901-1"}}`, strings.Repeat("a", 40), conclusion))
			for attempt, deliveryID := range []string{"243b2c3d-4e5f-4789-8abc-def012345679", "343b2c3d-4e5f-4789-8abc-def012345679"} {
				terminalAt := now.Add(time.Duration(5+attempt*2) * time.Second)
				if inserted, err := inbox.Record(ctx, hakowebhook.VerifiedDelivery{DeliveryID: deliveryID, Event: "workflow_job", Action: "completed", Payload: payload}, terminalAt); err != nil || !inserted {
					t.Fatalf("record terminal webhook: inserted=%v err=%v", inserted, err)
				}
				stats, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, terminalAt.Add(time.Second), nil)
				if err != nil || stats.Processed != 1 {
					t.Fatalf("apply terminal webhook: stats=%+v err=%v", stats, err)
				}
			}
			var checkConclusion string
			var checkRevision int64
			if err := pool.QueryRow(ctx, `SELECT conclusion,revision FROM github_actions_runner_checks WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&checkConclusion, &checkRevision); err != nil || checkConclusion != conclusion || checkRevision != 3 {
				t.Fatalf("terminal Check identity/conclusion: conclusion=%q revision=%d err=%v", checkConclusion, checkRevision, err)
			}
			service, err := githubbroker.NewActionsRunnerCleanupQueueService(pool, transaction.DefaultPolicy(), []string{"rp-tokyo-01"})
			if err != nil {
				t.Fatal(err)
			}
			handler := githubbroker.WithActionsRunnerCleanupQueue(http.NotFoundHandler(), service)
			queue, err := githubbroker.NewHTTPActionsRunnerCleanupQueueClient("https://broker.invalid", "rp-tokyo-01", &http.Client{Transport: runnerCleanupHTTPTransport(func(r *http.Request) (*http.Response, error) {
				cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				return rec.Result(), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			// Both provider operations fail once. Recreate the worker using the
			// same stable worker name between attempts, as after a process restart.
			vmExists, runnerExists := true, true
			terminateCalls, deregisterCalls := 0, 0
			terminate := func(_ context.Context, plane, vm string) error {
				if plane != "rp-tokyo-01" || vm != "microvm-cleanup" {
					t.Fatal("cleanup escaped its pinned provider identity")
				}
				terminateCalls++
				if terminateCalls == 1 {
					return errors.New("synthetic provider failure")
				}
				vmExists = false
				return nil
			}
			deregister := func(_ context.Context, job githubwebhook.ActionsRunnerCleanupJob) error {
				if vmExists || job.TenantID != "tenant_cleanup" || job.RunnerID == nil || *job.RunnerID != 9810 {
					t.Fatal("deregistration preceded termination or used a foreign identity")
				}
				deregisterCalls++
				if deregisterCalls == 1 {
					return errors.New("synthetic GitHub failure")
				}
				runnerExists = false
				return nil
			}
			for attempt := 1; attempt <= 3; attempt++ {
				worker := cleanupWorker(t, queue, terminate, deregister, time.Now)
				processed, err := worker.ProcessOne(ctx)
				if !processed || (attempt < 3 && !errors.Is(err, ErrCleanupDeferred)) || (attempt == 3 && err != nil) {
					t.Fatalf("cleanup attempt %d: processed=%v err=%v", attempt, processed, err)
				}
				var state, code string
				var count int
				var available time.Time
				if err := pool.QueryRow(ctx, `SELECT state,last_error,attempt_count,available_at FROM github_actions_runner_cleanup_outbox WHERE integration_id='shared' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&state, &code, &count, &available); err != nil {
					t.Fatal(err)
				}
				if count != attempt {
					t.Fatalf("cleanup attempt count=%d, want %d", count, attempt)
				}
				if attempt < 3 {
					wantCode := "microvm_terminate_failed"
					if attempt == 2 {
						wantCode = "runner_deregister_failed"
					}
					if state != "pending" || code != wantCode || !available.After(now) {
						t.Fatalf("cleanup retry not durable: state=%q code=%q available=%v", state, code, available)
					}
					if processed, err := worker.ProcessOne(ctx); err != nil || processed {
						t.Fatalf("cleanup retried before its backoff: processed=%v err=%v", processed, err)
					}
					// Make the isolated test row due; the Broker still controls
					// lease/retry timestamps, without wall-clock sleeps in CI.
					if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET available_at=now()-interval '1 second'`); err != nil {
						t.Fatal(err)
					}
				} else if state != "completed" || code != "" || vmExists || runnerExists {
					t.Fatalf("cleanup incomplete: state=%q code=%q vm=%v runner=%v", state, code, vmExists, runnerExists)
				}
			}
			if terminateCalls != 3 || deregisterCalls != 2 {
				t.Fatalf("unexpected provider cleanup calls: termination=%d deregistration=%d", terminateCalls, deregisterCalls)
			}
			worker := cleanupWorker(t, queue, terminate, deregister, time.Now)
			if processed, err := worker.ProcessOne(ctx); err != nil || processed {
				t.Fatalf("completed cleanup was re-executed: processed=%v err=%v", processed, err)
			}
		})
	}
}

type postgresRunnerBinder struct {
	pool transaction.Beginner
}

func (b postgresRunnerBinder) Bind(ctx context.Context, job githubwebhook.ActionsRunnerJob, microVMID string) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	bound, err := githubwebhook.BindActionsRunnerMicroVM(ctx, tx, job.IntegrationID, job.JobID, job.RunAttempt,
		job.ClaimID, "rp-tokyo-01", microVMID, time.Now().UTC())
	if err != nil || !bound {
		_ = tx.Rollback(ctx)
		if err != nil {
			return err
		}
		return ErrMicroVMIdentityMismatch
	}
	return tx.Commit(ctx)
}

type runnerCleanupHTTPTransport func(*http.Request) (*http.Response, error)

func (f runnerCleanupHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
