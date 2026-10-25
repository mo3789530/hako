//go:build integration

package workloads

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRetryFailedAgentProposalVerifierIsBoundedAndIdempotent(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	baseSHA, proposalSHA := strings.Repeat("a", 40), strings.Repeat("b", 64)
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_retry','Retry',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_retry','retry-subject','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_retry','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_retry',71,'acme','active','usr_retry',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (71,'tenant_retry',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at) VALUES ('tenant_retry',71,81,'acme','api','main',$1,true,$1)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES ('run_agent_retry','tenant_retry','agent',71,81,$1,'main','issue_comment','main','standard','untrusted','running','succeeded','rp_retry','agent-key-retry','agent-hash-retry',1800,$2,2,$3,$3)`, []any{baseSHA, now.Add(time.Hour), now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,completed_at,error_code,revision,created_at,updated_at)
		 VALUES ('run_verifier_retry','tenant_retry','job',71,81,$1,'refs/heads/main','pull_request','main','standard','untrusted','running','failed','rp_retry','verify-key-retry','verify-hash-retry',1800,$2,$3,'test_failed',2,$4,$4)`, []any{baseSHA, now.Add(time.Hour), now, now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,cleanup_confirmed_at,created_at,updated_at)
		 VALUES ('attempt_agent_retry','tenant_retry','run_agent_retry',1,1,'rp_retry','agent-token-retry','succeeded',$1,$1,$1)`, []any{now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,cleanup_confirmed_at,created_at,updated_at)
		 VALUES ('attempt_verifier_retry','tenant_retry','run_verifier_retry',1,1,'rp_retry','verifier-token-retry','failed',$1,$1,$1)`, []any{now}},
		{`INSERT INTO workload_artifacts(id,tenant_id,workload_run_id,kind,storage_ref,size_bytes,sha256,redacted,created_at)
		 VALUES ('artifact_retry','tenant_retry','run_agent_retry','agent_changes','s3://private/proposal.zip',128,$1,false,$2)`, []any{proposalSHA, now}},
		{`INSERT INTO workload_agent_proposal_verifications(tenant_id,agent_run_id,agent_attempt_id,proposal_artifact_id,proposal_sha256,base_commit_sha,verifier_run_id,verifier_attempt_id,created_at)
		 VALUES ('tenant_retry','run_agent_retry','attempt_agent_retry','artifact_retry',$1,$2,'run_verifier_retry','attempt_verifier_retry',$3)`, []any{proposalSHA, baseSHA, now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed proposal verifier retry: %v", err)
		}
	}
	first, err := retryProposalVerifierTx(ctx, pool, now.Add(time.Second))
	if err != nil || !first.Queued || first.VerifierAttemptID == "attempt_verifier_retry" {
		t.Fatalf("first failed verifier retry = %+v, error=%v", first, err)
	}
	second, err := retryProposalVerifierTx(ctx, pool, now.Add(2*time.Second))
	if err != nil || second.VerifierAttemptID != first.VerifierAttemptID || !second.Queued {
		t.Fatalf("idempotent active retry = %+v, first=%+v error=%v", second, first, err)
	}
	var outboxCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE aggregate_id=$1 AND event_type='workload_attempt.dispatch'`, first.VerifierAttemptID).Scan(&outboxCount); err != nil || outboxCount != 1 {
		t.Fatalf("retry dispatch outbox count=%d error=%v", outboxCount, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_run_attempts SET state='failed',cleanup_confirmed_at=$1 WHERE id=$2`, now.Add(3*time.Second), first.VerifierAttemptID); err != nil {
		t.Fatalf("seed second failed verifier attempt cleanup: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET state='failed',completed_at=$1,error_code='test_failed',revision=revision+1 WHERE id='run_verifier_retry'`, now.Add(3*time.Second)); err != nil {
		t.Fatalf("seed second failed verifier attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workload_quota_slots WHERE tenant_id='tenant_retry' AND workload_run_id='run_verifier_retry'`); err != nil {
		t.Fatalf("release failed verifier retry slot: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workload_artifact_quota_reservations WHERE tenant_id='tenant_retry' AND workload_run_id='run_verifier_retry'`); err != nil {
		t.Fatalf("release failed verifier retry artifact quota: %v", err)
	}
	third, err := retryProposalVerifierTx(ctx, pool, now.Add(4*time.Second))
	if err != nil || !third.Queued || third.VerifierAttemptID == first.VerifierAttemptID {
		t.Fatalf("second bounded retry = %+v, error=%v", third, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_run_attempts SET state='failed',cleanup_confirmed_at=$1 WHERE id=$2`, now.Add(5*time.Second), third.VerifierAttemptID); err != nil {
		t.Fatalf("seed exhausted verifier attempt cleanup: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET state='failed',completed_at=$1,error_code='test_failed',revision=revision+1 WHERE id='run_verifier_retry'`, now.Add(5*time.Second)); err != nil {
		t.Fatalf("seed exhausted verifier attempt: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workload_quota_slots WHERE tenant_id='tenant_retry' AND workload_run_id='run_verifier_retry'`); err != nil {
		t.Fatalf("release exhausted verifier retry slot: %v", err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM workload_artifact_quota_reservations WHERE tenant_id='tenant_retry' AND workload_run_id='run_verifier_retry'`); err != nil {
		t.Fatalf("release exhausted verifier retry artifact quota: %v", err)
	}
	if _, err := retryProposalVerifierTx(ctx, pool, now.Add(6*time.Second)); err != ErrAgentProposalVerifierRetryLimit {
		t.Fatalf("exhausted verifier retry error = %v", err)
	}
	var bindingAttempt string
	if err := pool.QueryRow(ctx, `SELECT verifier_attempt_id FROM workload_agent_proposal_verifications WHERE tenant_id='tenant_retry' AND agent_run_id='run_agent_retry'`).Scan(&bindingAttempt); err != nil || bindingAttempt != third.VerifierAttemptID {
		t.Fatalf("verifier binding points at %q, want %q; error=%v", bindingAttempt, third.VerifierAttemptID, err)
	}
}

func retryProposalVerifierTx(ctx context.Context, pool interface {
	Begin(context.Context) (pgx.Tx, error)
}, now time.Time) (AgentProposalVerifierRetry, error) {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	defer tx.Rollback(ctx)
	result, err := RetryFailedAgentProposalVerifier(ctx, tx, "tenant_retry", "run_agent_retry", "attempt_agent_retry", now)
	if err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return AgentProposalVerifierRetry{}, err
	}
	return result, nil
}
