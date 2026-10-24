//go:build integration

package workloads

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestResolveExecutionMetadataRevalidatesRunAttemptPlacementAndGitHubBinding(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_spec','Spec', $1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_spec','spec-subject','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_spec','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_other','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_spec',71,'acme','active','usr_spec',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (71,'tenant_spec',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_spec',71,81,'acme','api','main',$1)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,correlation_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES ('run_spec','tenant_spec','job',71,81,'req_33333333-3333-4333-8333-333333333333',$1,'refs/heads/main','push','main','standard','untrusted','running','provisioning','rp_spec','key_spec','hash_spec',600,$2,2,$3,$3)`, []any{strings.Repeat("a", 40), now.Add(10 * time.Minute), now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES ('attempt_spec','tenant_spec','run_spec',1,1,'rp_spec','token_spec','dispatch_pending',$1,$1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed trusted execution metadata: %v", err)
		}
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err := ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_spec", "attempt_spec", 1, now)
	_ = tx.Rollback(ctx)
	if err != nil {
		t.Fatalf("resolve active execution metadata: %v", err)
	}
	if metadata.Kind != domain.WorkloadJob || metadata.GitHubOwner != "acme" || metadata.GitHubRepository != "api" || metadata.CommitSHA != strings.Repeat("a", 40) || metadata.TriggerEvent != "push" || metadata.TriggerBranch != "main" ||
		metadata.CorrelationID != "req_33333333-3333-4333-8333-333333333333" ||
		metadata.RuntimeClass != "standard" || metadata.ResourcePlaneID != "rp_spec" || !metadata.RequestedAt.Equal(now) || !metadata.Deadline.Equal(now.Add(10*time.Minute)) {
		t.Fatalf("unexpected trusted metadata: %+v", metadata)
	}

	for _, request := range []struct {
		name       string
		plane      domain.ResourcePlaneID
		tenant     domain.TenantID
		run        domain.WorkloadRunID
		attempt    string
		generation int64
	}{
		{name: "wrong Resource Plane", plane: "rp_other", tenant: "tenant_spec", run: "run_spec", attempt: "attempt_spec", generation: 1},
		{name: "wrong Tenant", plane: "rp_spec", tenant: "tenant_other", run: "run_spec", attempt: "attempt_spec", generation: 1},
		{name: "wrong attempt", plane: "rp_spec", tenant: "tenant_spec", run: "run_spec", attempt: "attempt_other", generation: 1},
		{name: "wrong generation", plane: "rp_spec", tenant: "tenant_spec", run: "run_spec", attempt: "attempt_spec", generation: 2},
	} {
		t.Run(request.name, func(t *testing.T) {
			tx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			_, err = ResolveExecutionMetadata(ctx, tx, request.plane, request.tenant, request.run, request.attempt, request.generation, now)
			_ = tx.Rollback(ctx)
			if !errors.Is(err, ErrExecutionSpecNotFound) {
				t.Fatalf("unauthorized/mismatched execution metadata error = %v", err)
			}
		})
	}

	// A Tenant-owned active App makes the persisted Run pin mandatory. The
	// legacy/shared path must not be used merely because the Run lacks a pin.
	for _, statement := range []string{
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_spec',901,'tenant_owned','tenant_spec','spec-app','client-spec',$1)`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at) VALUES ('credential_spec','tenant_spec',901,'approved','secret:private','secret:oauth','secret:webhook',$1)`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_spec','tenant_spec','registration_spec',901,'credential_spec','active',1,1,'integration-key','integration-hash','usr_spec',$1,$1)`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_spec','integration_spec',71,$1)`,
	} {
		if _, err := pool.Exec(ctx, statement, now); err != nil {
			t.Fatalf("seed Tenant-owned GitHub App pin: %v", err)
		}
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_spec", "attempt_spec", 1, now); !errors.Is(err, ErrExecutionSpecNotFound) {
		_ = tx.Rollback(ctx)
		t.Fatalf("active Tenant App allowed fallback from an unpinned Run: %v", err)
	}
	_ = tx.Rollback(ctx)
	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET github_integration_id='integration_spec',github_config_revision=1,github_credential_generation=1 WHERE id='run_spec'`); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metadata, err = ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_spec", "attempt_spec", 1, now)
	_ = tx.Rollback(ctx)
	if err != nil || metadata.GitHubIntegrationID != "integration_spec" || metadata.GitHubConfigRevision != 1 || metadata.GitHubCredentialGen != 1 {
		t.Fatalf("pinned active Tenant App metadata = %+v, err=%v", metadata, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_repositories SET factory_enabled=true,factory_enabled_at=$1::timestamptz-interval '1 minute' WHERE tenant_id='tenant_spec' AND github_repository_id=81`, now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_runs(id,tenant_id,kind,requested_by,github_installation_id,github_repository_id,github_integration_id,github_config_revision,github_credential_generation,correlation_id,commit_sha,ref,trigger_event,trigger_branch,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		VALUES ('run_agent_spec','tenant_spec','agent','usr_spec',71,81,'integration_spec',1,1,'req_44444444-4444-4444-8444-444444444444',$1,'main','issue_comment','main','standard','untrusted','running','provisioning','rp_spec','key_agent_spec','hash_agent_spec',600,$2,1,$3,$3)`, strings.Repeat("c", 40), now.Add(10*time.Minute), now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		VALUES ('attempt_agent_spec','tenant_spec','run_agent_spec',1,1,'rp_spec','token_agent_spec','dispatch_pending',$1,$1)`, now); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	agentMetadata, agentErr := ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_agent_spec", "attempt_agent_spec", 1, now)
	_ = tx.Rollback(ctx)
	if agentErr != nil || agentMetadata.Kind != domain.WorkloadAgent || agentMetadata.TriggerEvent != "issue_comment" || agentMetadata.GitHubIntegrationID != "integration_spec" {
		t.Fatalf("Agent execution metadata=%+v error=%v", agentMetadata, agentErr)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=2 WHERE id='integration_spec'`); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_spec", "attempt_spec", 1, now)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrExecutionSpecNotFound) {
		t.Fatalf("stale Tenant App generation returned execution metadata: %v", err)
	}

	if _, err := pool.Exec(ctx, `UPDATE workload_runs SET desired_state='cancelled' WHERE id='run_spec'`); err != nil {
		t.Fatal(err)
	}
	tx, err = pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	_, err = ResolveExecutionMetadata(ctx, tx, "rp_spec", "tenant_spec", "run_spec", "attempt_spec", 1, now)
	_ = tx.Rollback(ctx)
	if !errors.Is(err, ErrExecutionSpecNotFound) {
		t.Fatalf("cancelled Workload Run returned execution metadata: %v", err)
	}
}
