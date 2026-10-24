//go:build integration

package previewprovision

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	previewstore "github.com/mo3789530/hako/internal/store/previews"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestWorkerFencesAndCompensatesStalePreviewProvision(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_provision','Preview Provision',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_provision','preview-provision','',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_provision',9811,'acme','active','usr_preview_provision',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9811,'tenant_preview_provision',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_provision',9811,9812,'acme','preview-provision','main',$1,true,$1)`,
		`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_provision',9811,9812,31,$2,'feature/provision','present','awaiting_runtime','70000000-0000-4000-8000-000000000001',$1,$3,$1,$1)`,
		`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_provision',9811,9812,31,1,$2,'feature/provision','present','awaiting_runtime',$3,$1,$1)`,
	} {
		args := []any{now}
		if strings.Contains(query, "repository_previews(") || strings.Contains(query, "repository_preview_runtime_allocations(") {
			args = []any{now, strings.Repeat("a", 40), now.Add(time.Hour)}
		}
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed Preview provision case: %v", err)
		}
	}

	clock := now.Add(time.Second)
	provisioner := &integrationProvisioner{
		failures: []error{ErrMicroVMLaunch},
		binding:  previewsBinding("rp_preview_test", "ws_preview_test", "microvm_preview_test", "route_preview_test"),
	}
	claimSequence := 0
	worker := Worker{
		Pool: pool, Provisioner: provisioner, Lease: 10 * time.Second, Timeout: time.Second, BatchSize: 1,
		Now: func() time.Time { return clock },
		NewClaimID: func() (string, error) {
			claimSequence++
			return fmt.Sprintf("preview-provision-%04d", claimSequence), nil
		},
	}
	failed, err := worker.RunOnce(ctx)
	if err != nil || failed.Claimed != 1 || failed.RetryScheduled != 1 {
		t.Fatalf("failed provider attempt: stats=%+v err=%v", failed, err)
	}
	var retryAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provision_next_attempt_at FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_provision' AND pull_request_number=31 AND generation=1`).Scan(&retryAt); err != nil {
		t.Fatalf("read provider retry deadline: %v", err)
	}
	clock = retryAt.Add(-time.Second)
	early, err := worker.RunOnce(ctx)
	if err != nil || early.Claimed != 0 {
		t.Fatalf("worker claimed before retry deadline: stats=%+v err=%v", early, err)
	}
	clock = retryAt
	succeeded, err := worker.RunOnce(ctx)
	if err != nil || succeeded.Claimed != 1 || succeeded.Bound != 1 {
		t.Fatalf("successful retry: stats=%+v err=%v", succeeded, err)
	}
	var boundState, vmID string
	if err := pool.QueryRow(ctx, `SELECT state,microvm_id FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_provision' AND pull_request_number=31 AND generation=1`).Scan(&boundState, &vmID); err != nil || boundState != "running" || vmID != "microvm_preview_test" {
		t.Fatalf("Preview provider binding = state %q vm %q err %v", boundState, vmID, err)
	}

	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,32,$1,'feature/stale','present','awaiting_runtime','70000000-0000-4000-8000-000000000002',$2,$3,$2,$2)`,
		strings.Repeat("b", 40), clock, clock.Add(time.Hour)); err != nil {
		t.Fatalf("seed stale-result Preview: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
		(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,32,1,$1,'feature/stale','present','awaiting_runtime',$2,$3,$3)`,
		strings.Repeat("b", 40), clock.Add(time.Hour), clock); err != nil {
		t.Fatalf("seed stale-result runtime generation: %v", err)
	}
	provisioner.onProvision = func(job previewstore.ProvisionJob) {
		if job.PullRequestNumber != 32 {
			t.Errorf("unexpected stale-result job: %+v", job)
			return
		}
		_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return previewstore.ApplyPullRequestDelivery(ctx, tx, "tenant_preview_provision", "70000000-0000-4000-8000-000000000003",
				githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: "synchronize", InstallationID: 9811,
					Repository:  &githubwebhook.RepositoryRef{GitHubID: 9812},
					PullRequest: &githubwebhook.PullRequestRef{Number: 32, State: "open", HeadRef: "feature/stale", HeadSHA: strings.Repeat("c", 40), UpdatedAt: clock.Add(time.Second)}}, clock.Add(time.Second), clock.Add(time.Second))
		})
		if err != nil {
			t.Errorf("advance Preview generation during provider create: %v", err)
		}
	}
	clock = clock.Add(2 * time.Second)
	stale, err := worker.RunOnce(ctx)
	if err != nil || stale.Claimed != 1 || stale.StaleClaim != 1 || stale.Compensated != 1 || provisioner.compensated != 1 {
		t.Fatalf("stale provider result was not compensated: stats=%+v compensated=%d err=%v", stale, provisioner.compensated, err)
	}
}

type integrationProvisioner struct {
	failures    []error
	binding     previewstore.RuntimeBinding
	onProvision func(previewstore.ProvisionJob)
	compensated int
}

func (p *integrationProvisioner) Provision(_ context.Context, job previewstore.ProvisionJob) (previewstore.RuntimeBinding, error) {
	if p.onProvision != nil {
		p.onProvision(job)
	}
	if len(p.failures) > 0 {
		err := p.failures[0]
		p.failures = p.failures[1:]
		return previewstore.RuntimeBinding{}, err
	}
	binding := p.binding
	binding.WorkspaceID = job.RuntimeWorkspaceID
	return binding, nil
}

func (p *integrationProvisioner) Compensate(context.Context, previewstore.ProvisionJob, previewstore.RuntimeBinding) error {
	p.compensated++
	return nil
}

func previewsBinding(plane, workspace, microVM, route string) previewstore.RuntimeBinding {
	return previewstore.RuntimeBinding{ResourcePlaneID: domain.ResourcePlaneID(plane), WorkspaceID: domain.WorkspaceID(workspace), MicroVMID: microVM, GatewayRouteID: route}
}
