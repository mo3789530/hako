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

	"github.com/mo3789530/hako/internal/githubbroker"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

// This composes the authenticated-inbox boundary, terminal status transaction,
// Broker HTTP queue and cleanup worker. Provider operations are stateful fakes;
// it does not exercise GitHub signature verification or AWS networking.
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
				`INSERT INTO github_actions_runner_jobs(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,runner_id,runner_name,resource_plane_id,lambda_microvm_id,created_at,updated_at) VALUES ('shared','queued-cleanup','tenant_cleanup',8801,8802,9901,9902,1,'CI','test','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','["self-hosted","hako"]','running',9810,'hako-9901-1','rp-tokyo-01','microvm-cleanup',now(),now())`,
				`INSERT INTO github_actions_runner_checks(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,workflow_run_id,commit_sha,check_name,external_id,status,started_at,output_title,output_summary,revision,updated_at) VALUES ('shared',9901,1,'tenant_cleanup',8801,8802,9902,'aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','Hako Actions Runner','hako-actions-runner/shared/9901/1','in_progress',now(),'Hako Actions Runner','Running',2,now())`,
			} {
				if _, err := pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Truncate(time.Microsecond)
			payload := []byte(fmt.Sprintf(`{"action":"completed","installation":{"id":8801},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"completed","conclusion":%q,"labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-9901-1"}}`, strings.Repeat("a", 40), conclusion))
			inbox := githubwebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
			for _, deliveryID := range []string{"143b2c3d-4e5f-4789-8abc-def012345678", "243b2c3d-4e5f-4789-8abc-def012345678"} {
				if inserted, err := inbox.Record(ctx, hakowebhook.VerifiedDelivery{DeliveryID: deliveryID, Event: "workflow_job", Action: "completed", Payload: payload}, now); err != nil || !inserted {
					t.Fatalf("record terminal webhook: inserted=%v err=%v", inserted, err)
				}
				stats, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, now, nil)
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

type runnerCleanupHTTPTransport func(*http.Request) (*http.Response, error)

func (f runnerCleanupHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
