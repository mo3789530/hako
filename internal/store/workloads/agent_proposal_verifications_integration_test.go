//go:build integration

package workloads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestBindAgentProposalVerificationRequiresCleanedSuccessfulAttemptAndIsImmutable(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	baseSHA := strings.Repeat("a", 40)
	proposalSHA := strings.Repeat("b", 64)
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_proof','Proof',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_proof','proof-subject','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_proof','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_proof',71,'acme','active','usr_proof',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (71,'tenant_proof',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_proof',71,81,'acme','api','main',$1)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES ('run_agent_proof','tenant_proof','agent',71,81,$1,'main','issue_comment','main','standard','untrusted','running','succeeded','rp_proof','key_agent_proof','hash_agent_proof',600,$2,2,$3,$3)`, []any{baseSHA, now.Add(10 * time.Minute), now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES ('run_verifier_proof','tenant_proof','job',71,81,$1,'main','','','standard','untrusted','running','pending','rp_proof','key_verifier_proof','hash_verifier_proof',600,$2,1,$3,$3)`, []any{baseSHA, now.Add(10 * time.Minute), now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES ('attempt_agent_proof','tenant_proof','run_agent_proof',1,1,'rp_proof','token_agent_proof','cleanup_pending',$1,$1)`, []any{now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES ('attempt_verifier_proof','tenant_proof','run_verifier_proof',1,1,'rp_proof','token_verifier_proof','dispatch_pending',$1,$1)`, []any{now}},
		{`INSERT INTO workload_artifacts(id,tenant_id,workload_run_id,kind,storage_ref,size_bytes,sha256,redacted,created_at)
		 VALUES ('artifact_proof','tenant_proof','run_agent_proof','agent_changes','s3://private/proposal.zip',128,$1,false,$2)`, []any{proposalSHA, now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed proposal verification: %v", err)
		}
	}
	binding := AgentProposalVerification{TenantID: "tenant_proof", AgentRunID: "run_agent_proof", AgentAttemptID: "attempt_agent_proof",
		ProposalArtifactID: "artifact_proof", ProposalSHA256: proposalSHA, BaseCommitSHA: baseSHA,
		VerifierRunID: "run_verifier_proof", VerifierAttemptID: "attempt_verifier_proof", CreatedAt: now}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error { return BindAgentProposalVerification(ctx, tx, binding) }); !errors.Is(err, ErrAgentProposalVerificationConflict) {
		t.Fatalf("cleanup-pending Agent attempt binding error = %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workload_run_attempts SET state='cleanup_confirmed' WHERE id='attempt_agent_proof'`); err != nil {
		t.Fatal(err)
	}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error { return BindAgentProposalVerification(ctx, tx, binding) }); err != nil {
		t.Fatalf("bind cleaned proposal to isolated verifier: %v", err)
	}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error { return BindAgentProposalVerification(ctx, tx, binding) }); err != nil {
		t.Fatalf("idempotent binding retry: %v", err)
	}
	if err := withTx(ctx, pool, func(tx pgx.Tx) error {
		stored, err := GetAgentProposalVerification(ctx, tx, binding.TenantID, binding.AgentRunID, binding.AgentAttemptID)
		if err != nil {
			return err
		}
		if stored.ProposalSHA256 != proposalSHA || stored.BaseCommitSHA != baseSHA || stored.VerifierRunID != binding.VerifierRunID || stored.ProposalArtifactID != binding.ProposalArtifactID {
			t.Fatalf("stored verification binding = %+v", stored)
		}
		return nil
	}); err != nil {
		t.Fatalf("read verification binding: %v", err)
	}
	changed := binding
	changed.ProposalSHA256 = strings.Repeat("c", 64)
	if err := withTx(ctx, pool, func(tx pgx.Tx) error { return BindAgentProposalVerification(ctx, tx, changed) }); !errors.Is(err, ErrAgentProposalVerificationConflict) {
		t.Fatalf("changed proposal digest binding error = %v", err)
	}
	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_agent_proposal_verifications WHERE tenant_id='tenant_proof'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("persisted verification bindings = %d, err=%v", count, err)
	}
}

func withTx(ctx context.Context, pool *pgxpool.Pool, fn func(pgx.Tx) error) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit(ctx)
}
