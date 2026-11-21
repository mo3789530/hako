//go:build integration

package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresActionsRunnerCleanupQueueHTTPFencesPlaneAndLeaseGeneration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_cleanup_http','Cleanup HTTP',now())`,
		`INSERT INTO github_actions_runner_jobs(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at) VALUES ('integration_cleanup_http','queued-http','tenant_cleanup_http',77,88,99,100,1,'CI','test','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','["hako"]','completed',now(),now())`,
		`INSERT INTO github_actions_runner_cleanup_outbox(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,runner_id,resource_plane_id,lambda_microvm_id,reason,state,available_at,created_at,updated_at) VALUES ('integration_cleanup_http',99,1,'tenant_cleanup_http',77,88,101,'rp-tokyo-01','microvm-http','completed','pending',now(),now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	service, err := NewActionsRunnerCleanupQueueService(pool, transaction.DefaultPolicy(), []string{"rp-tokyo-01", "rp-us-01"})
	if err != nil {
		t.Fatal(err)
	}
	handler := WithActionsRunnerCleanupQueue(http.NotFoundHandler(), service)
	// Reuse the production verified-certificate authorization boundary with an
	// in-process transport. Cryptographic mTLS handshakes have separate coverage.
	clientFor := func(identity, plane string, verified bool) *HTTPActionsRunnerCleanupQueueClient {
		t.Helper()
		client, err := NewHTTPActionsRunnerCleanupQueueClient("https://broker.invalid", plane, &http.Client{Transport: cleanupQueueTransport(func(r *http.Request) (*http.Response, error) {
			if identity != "" {
				uri, err := url.Parse(identity)
				if err != nil {
					return nil, err
				}
				cert := &x509.Certificate{URIs: []*url.URL{uri}}
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
				if verified {
					r.TLS.VerifiedChains = [][]*x509.Certificate{{cert}}
				}
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, r)
			return rec.Result(), nil
		})})
		if err != nil {
			t.Fatal(err)
		}
		return client
	}
	for _, denied := range []struct {
		identity string
		verified bool
	}{
		{"", false}, {controlPlaneURI, true}, {resourcePlaneURIPrefix + "rp-tokyo-01", false}, {resourcePlaneURIPrefix + "rp-other-01", true},
	} {
		if _, _, err := clientFor(denied.identity, "rp-tokyo-01", denied.verified).Claim(ctx, "rp-tokyo-01", "cleanup-http-claim", time.Now(), time.Minute); err == nil {
			t.Fatalf("untrusted certificate identity admitted: %q verified=%v", denied.identity, denied.verified)
		}
	}
	foreign := clientFor(resourcePlaneURIPrefix+"rp-us-01", "rp-us-01", true)
	if _, found, err := foreign.Claim(ctx, "rp-us-01", "cleanup-http-foreign", time.Now(), time.Minute); err != nil || found {
		t.Fatalf("foreign Plane acquired cleanup: found=%v err=%v", found, err)
	}
	client := clientFor(resourcePlaneURIPrefix+"rp-tokyo-01", "rp-tokyo-01", true)
	first, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-claim", time.Now(), time.Minute)
	if err != nil || !found || first.TenantID != "tenant_cleanup_http" || first.LambdaMicroVMID == nil || *first.LambdaMicroVMID != "microvm-http" || first.RunnerID == nil || *first.RunnerID != 101 || first.AttemptCount != 1 {
		t.Fatalf("claim exact durable cleanup: job=%+v found=%v err=%v", first, found, err)
	}
	if applied, err := foreign.Complete(ctx, first, time.Now()); err != nil || applied {
		t.Fatalf("foreign Plane acknowledged cleanup: applied=%v err=%v", applied, err)
	}
	for _, mutate := range []func(*cleanupLeaseIdentity){
		func(lease *cleanupLeaseIdentity) { lease.WorkflowJobID++ },
		func(lease *cleanupLeaseIdentity) { lease.AttemptCount++ },
		func(lease *cleanupLeaseIdentity) { lease.LeaseExpires = lease.LeaseExpires.Add(time.Second) },
		func(lease *cleanupLeaseIdentity) { lease.ClaimID = "another-http-claim" },
	} {
		lease := cleanupLeaseFromJob(first)
		mutate(lease)
		result, err := client.call(ctx, "complete", cleanupQueueRequest{Lease: lease})
		if err != nil || result.Applied {
			t.Fatalf("forged lease acknowledged cleanup: applied=%v err=%v", result.Applied, err)
		}
	}
	// A malicious client clock must not defer work for years or resurrect a lease.
	callerNow := time.Now().AddDate(10, 0, 0)
	before := time.Now()
	if applied, err := client.Retry(ctx, first, callerNow, callerNow.Add(15*time.Second), "microvm_terminate_failed"); err != nil || !applied {
		t.Fatalf("schedule bounded retry: applied=%v err=%v", applied, err)
	}
	var available time.Time
	if err := pool.QueryRow(ctx, `SELECT available_at FROM github_actions_runner_cleanup_outbox`).Scan(&available); err != nil || available.Before(before.Add(15*time.Second)) || available.After(time.Now().Add(16*time.Second)) {
		t.Fatalf("retry used caller's clock: available=%v err=%v", available, err)
	}
	if _, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-claim", time.Now(), time.Minute); err != nil || found {
		t.Fatalf("retry backoff bypassed: found=%v err=%v", found, err)
	}
	// Make only this test row due without sleeping or touching shared DB state.
	if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET available_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	second, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-claim", time.Now(), time.Minute)
	if err != nil || !found || second.AttemptCount != 2 {
		t.Fatalf("recover cleanup: found=%v generation=%d err=%v", found, second.AttemptCount, err)
	}
	if applied, err := client.Complete(ctx, first, callerNow); err != nil || applied {
		t.Fatalf("stale complete accepted: applied=%v err=%v", applied, err)
	}
	if applied, err := client.Retry(ctx, first, time.Now(), time.Now().Add(time.Minute), "runner_deregister_failed"); err != nil || applied {
		t.Fatalf("stale retry accepted: applied=%v err=%v", applied, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET lease_expires_at=now()-interval '1 second'`); err != nil {
		t.Fatal(err)
	}
	if applied, err := client.Complete(ctx, second, time.Now().AddDate(-10, 0, 0)); err != nil || applied {
		t.Fatalf("expired lease accepted caller timestamp: applied=%v err=%v", applied, err)
	}
	third, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-recovery", time.Now(), time.Minute)
	if err != nil || !found || third.AttemptCount != 3 {
		t.Fatalf("recover expired lease: found=%v generation=%d err=%v", found, third.AttemptCount, err)
	}
	if applied, err := client.Complete(ctx, third, time.Now()); err != nil || !applied {
		t.Fatalf("acknowledge exact recovered lease: applied=%v err=%v", applied, err)
	}
	if _, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-after", time.Now(), time.Minute); err != nil || found {
		t.Fatalf("completed cleanup was re-claimed: found=%v err=%v", found, err)
	}
	for _, query := range []string{
		`INSERT INTO github_actions_runner_jobs(integration_id,delivery_id,tenant_id,installation_id,repository_id,workflow_job_id,workflow_run_id,run_attempt,workflow_name,job_name,head_branch,head_sha,labels,state,created_at,updated_at) VALUES ('integration_cleanup_http','failed-before-launch','tenant_cleanup_http',77,88,199,200,1,'CI','test','main','aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa','["hako"]','failed',now(),now())`,
		`INSERT INTO github_actions_runner_cleanup_outbox(integration_id,workflow_job_id,run_attempt,tenant_id,installation_id,repository_id,reason,state,available_at,created_at,updated_at) VALUES ('integration_cleanup_http',199,1,'tenant_cleanup_http',77,88,'failed','pending',now(),now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	unassigned, found, err := client.Claim(ctx, "rp-tokyo-01", "cleanup-http-unassigned", time.Now(), time.Minute)
	if err != nil || !found || unassigned.ResourcePlaneID == nil || *unassigned.ResourcePlaneID != "rp-tokyo-01" || unassigned.LambdaMicroVMID != nil {
		t.Fatalf("pre-launch cleanup was not pinned on acquisition: job=%+v found=%v err=%v", unassigned, found, err)
	}
	if applied, err := foreign.Complete(ctx, unassigned, time.Now()); err != nil || applied {
		t.Fatalf("foreign Plane acknowledged formerly unassigned cleanup: applied=%v err=%v", applied, err)
	}
	if applied, err := client.Complete(ctx, unassigned, time.Now()); err != nil || !applied {
		t.Fatalf("complete pre-launch cleanup: applied=%v err=%v", applied, err)
	}
}
