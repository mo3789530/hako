//go:build integration

package previewprovision

import (
	"context"
	"errors"
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
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_preview_test','aws','ap-northeast-1','["microvm"]')`); err != nil {
		t.Fatalf("seed fixed Preview Resource Plane: %v", err)
	}

	clock := now.Add(time.Second)
	provisioner := &integrationProvisioner{
		failures:        []error{ErrGatewayRoutePublish},
		failureBindings: []previewstore.RuntimeBinding{{ResourcePlaneID: "rp_preview_test", MicroVMID: "microvm_orphan_preview_test"}},
		binding:         previewsBinding("rp_preview_test", "ws_preview_test", "microvm_preview_test", "route_preview_test"),
	}
	claimSequence := 0
	worker := Worker{
		Pool: pool, Provisioner: provisioner, TargetResourcePlaneID: "rp_preview_test", Lease: 10 * time.Second, Timeout: time.Second, BatchSize: 1,
		Now: func() time.Time { return clock },
		NewClaimID: func() (string, error) {
			claimSequence++
			return fmt.Sprintf("preview-provision-%04d", claimSequence), nil
		},
	}
	failed, err := worker.RunOnce(ctx)
	if err != nil || failed.Claimed != 1 || failed.RetryScheduled != 1 || failed.Compensated != 1 || provisioner.compensated != 1 {
		t.Fatalf("failed provider attempt: stats=%+v compensated=%d err=%v", failed, provisioner.compensated, err)
	}
	if got := provisioner.compensatedBindings[0]; got.ResourcePlaneID != "rp_preview_test" || got.MicroVMID != "microvm_orphan_preview_test" || got.GatewayRouteID != "" {
		t.Fatalf("partial provider binding passed to compensation = %+v", got)
	}
	var retryAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provision_next_attempt_at FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_provision' AND pull_request_number=31 AND generation=1`).Scan(&retryAt); err != nil {
		t.Fatalf("read provider retry deadline: %v", err)
	}
	var targetResourcePlaneID string
	if err := pool.QueryRow(ctx, `SELECT target_resource_plane_id FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_provision' AND pull_request_number=31 AND generation=1`).Scan(&targetResourcePlaneID); err != nil || targetResourcePlaneID != "rp_preview_test" {
		t.Fatalf("Preview target Resource Plane = %q err=%v", targetResourcePlaneID, err)
	}
	clock = retryAt.Add(-time.Second)
	early, err := worker.RunOnce(ctx)
	if err != nil || early.Claimed != 0 {
		t.Fatalf("worker claimed before retry deadline: stats=%+v err=%v", early, err)
	}
	clock = retryAt
	wrongPlaneWorker := worker
	wrongPlaneWorker.TargetResourcePlaneID = "rp_another_plane"
	wrongPlaneRetry, err := wrongPlaneWorker.RunOnce(ctx)
	if err != nil || wrongPlaneRetry.Claimed != 0 {
		t.Fatalf("Preview retry moved to a different Resource Plane: stats=%+v err=%v", wrongPlaneRetry, err)
	}
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
	if err != nil || stale.Claimed != 1 || stale.StaleClaim != 1 || stale.Compensated != 1 || provisioner.compensated != 2 {
		t.Fatalf("stale provider result was not compensated: stats=%+v compensated=%d err=%v", stale, provisioner.compensated, err)
	}
	provisioner.onProvision = nil
	currentHead, err := worker.RunOnce(ctx)
	if err != nil || currentHead.Claimed != 1 || currentHead.Bound != 1 {
		t.Fatalf("current PR-head generation was not provisioned: stats=%+v err=%v", currentHead, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,33,$1,'feature/compensation-retry','present','awaiting_runtime','70000000-0000-4000-8000-000000000004',$2,$3,$2,$2)`,
		strings.Repeat("d", 40), clock, clock.Add(time.Hour)); err != nil {
		t.Fatalf("seed compensation-retry Preview: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
		(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,33,1,$1,'feature/compensation-retry','present','awaiting_runtime',$2,$3,$3)`,
		strings.Repeat("d", 40), clock.Add(time.Hour), clock); err != nil {
		t.Fatalf("seed compensation-retry runtime generation: %v", err)
	}
	provisioner.failures = append(provisioner.failures, ErrGatewayRoutePublish)
	provisioner.failureBindings = append(provisioner.failureBindings, previewstore.RuntimeBinding{ResourcePlaneID: "rp_preview_test", MicroVMID: "microvm_retry_preview_test"})
	provisioner.compensationFailures = append(provisioner.compensationFailures, errors.New("temporary cleanup failure"))
	clock = clock.Add(time.Second)
	compensationFailed, err := worker.RunOnce(ctx)
	if !errors.Is(err, ErrCompensationFailed) || compensationFailed.Claimed != 1 || compensationFailed.RetryScheduled != 0 {
		t.Fatalf("compensation failure result = stats %+v error %v", compensationFailed, err)
	}
	var leaseExpiresAt time.Time
	if err := pool.QueryRow(ctx, `SELECT provision_lease_expires_at FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_provision' AND pull_request_number=33 AND generation=1`).Scan(&leaseExpiresAt); err != nil {
		t.Fatalf("read retained provision lease: %v", err)
	}
	if !leaseExpiresAt.After(clock) {
		t.Fatalf("retained provision lease %s is not after worker time %s", leaseExpiresAt, clock)
	}
	clock = leaseExpiresAt.Add(-time.Microsecond)
	blocked, err := worker.RunOnce(ctx)
	if err != nil || blocked.Claimed != 0 {
		t.Fatalf("worker reclaimed before compensation lease expiry: stats=%+v err=%v", blocked, err)
	}
	clock = leaseExpiresAt
	recovered, err := worker.RunOnce(ctx)
	if err != nil || recovered.Claimed != 1 || recovered.Bound != 1 {
		t.Fatalf("provision did not recover after lease expiry: stats=%+v err=%v", recovered, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_previews
		(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,34,$1,'feature/unknown-result','present','awaiting_runtime','70000000-0000-4000-8000-000000000005',$2,$3,$2,$2)`,
		strings.Repeat("e", 40), clock, clock.Add(time.Hour)); err != nil {
		t.Fatalf("seed missing-provider-result Preview: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
		(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_provision',9811,9812,34,1,$1,'feature/unknown-result','present','awaiting_runtime',$2,$3,$3)`,
		strings.Repeat("e", 40), clock.Add(time.Hour), clock); err != nil {
		t.Fatalf("seed missing-provider-result runtime generation: %v", err)
	}
	provisioner.failures = append(provisioner.failures, ErrMicroVMLaunch)
	clock = clock.Add(time.Second)
	unknownResult, err := worker.RunOnce(ctx)
	if err != nil || unknownResult.Claimed != 1 || unknownResult.RetryScheduled != 1 || unknownResult.Compensated != 1 {
		t.Fatalf("failure without provider IDs was not compensated before retry: stats=%+v err=%v", unknownResult, err)
	}
	if last := provisioner.compensatedBindings[len(provisioner.compensatedBindings)-1]; last != (previewstore.RuntimeBinding{}) {
		t.Fatalf("empty provider binding changed before tag recovery: %+v", last)
	}
}

func TestScheduledWorkerSelectsTenantEligibleCapacityAndReleasesAfterCleanup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_scheduled','Preview Scheduled',$1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_scheduled','preview-scheduled','',$1)`, []any{now}},
		{`INSERT INTO workspaces(id,tenant_id,owner_id,name,runtime_class,image,created_at,updated_at) VALUES ('ws_preview_sched_load','tenant_preview_scheduled','usr_preview_scheduled','existing','standard','image',$1,$1)`, []any{now}},
		{`INSERT INTO workspace_status(workspace_id,desired_state,observed_state,updated_at) VALUES ('ws_preview_sched_load','running','running',$1)`, []any{now}},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_scheduled',9821,'acme','active','usr_preview_scheduled',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9821,'tenant_preview_scheduled',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_scheduled',9821,9822,'acme','preview-scheduled','main',$1,true,$1)`, []any{now}},
		{`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_scheduled',9821,9822,41,$2,'feature/scheduled','present','awaiting_runtime','70000000-0000-4000-8000-000000000041',$1,$3,$1,$1)`, []any{now, strings.Repeat("e", 40), now.Add(time.Hour)}},
		{`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_scheduled',9821,9822,41,1,$2,'feature/scheduled','present','awaiting_runtime',$3,$1,$1)`, []any{now, strings.Repeat("e", 40), now.Add(time.Hour)}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES
		 ('rp_preview_full','aws','ap-northeast-1','["microvm"]'),('rp_preview_available','aws','ap-northeast-1','["microvm"]'),
		 ('rp_preview_forbidden','aws','ap-southeast-1','["microvm"]')`, nil},
		{`INSERT INTO resource_plane_status(resource_plane_id,status,updated_at) VALUES
		 ('rp_preview_full','active',$1),('rp_preview_available','active',$1),('rp_preview_forbidden','active',$1)`, []any{now}},
		{`INSERT INTO runtime_class_resources(runtime_class,cpu_millicores,memory_mib,updated_at) VALUES ('standard',1000,1024,$1)`, []any{now}},
		{`INSERT INTO resource_plane_capacities(resource_plane_id,max_workspaces,reserved_workspaces,updated_at,max_cpu_millicores,reserved_cpu_millicores,max_memory_mib,reserved_memory_mib) VALUES
		 ('rp_preview_full',1,1,$1,1000,1000,1024,1024),('rp_preview_available',2,1,$1,2000,1000,2048,1024),
		 ('rp_preview_forbidden',2,0,$1,2000,0,2048,0)`, []any{now}},
		{`INSERT INTO placements(workspace_id,resource_plane_id,placed_at) VALUES ('ws_preview_sched_load','rp_preview_available',$1)`, []any{now}},
		{`INSERT INTO resource_plane_reservations(workspace_id,resource_plane_id,reserved_at,runtime_class,cpu_millicores,memory_mib) VALUES ('ws_preview_sched_load','rp_preview_available',$1,'standard',1000,1024)`, []any{now}},
		{`INSERT INTO tenant_placement_policies(tenant_id,allowed_regions_json,resource_plane_ids_json,required_capabilities_json,max_cost_tier,minimum_isolation_tier,updated_at)
		 VALUES ('tenant_preview_scheduled','["ap-northeast-1"]','["rp_preview_available","rp_preview_full","rp_preview_forbidden"]','[]',NULL,NULL,$1)`, []any{now}},
	}
	for _, item := range seed {
		if _, err := pool.Exec(ctx, item.query, item.args...); err != nil {
			t.Fatalf("seed scheduled Preview placement: %v", err)
		}
	}
	provisioner := &integrationProvisioner{binding: previewsBinding("", "", "microvm_scheduled", "route_scheduled")}
	worker := Worker{Pool: pool, Provisioner: provisioner, RuntimeClass: "standard", Lease: 10 * time.Second, Timeout: time.Second, BatchSize: 1,
		Now: func() time.Time { return now.Add(time.Second) }, NewClaimID: func() (string, error) { return "preview-scheduled-claim", nil }}
	stats, err := worker.RunOnce(ctx)
	if err != nil || stats.Claimed != 1 || stats.Bound != 1 {
		t.Fatalf("scheduled Preview provision: stats=%+v error=%v", stats, err)
	}
	var targetPlane, state string
	if err := pool.QueryRow(ctx, `SELECT target_resource_plane_id,state FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_scheduled' AND pull_request_number=41 AND generation=1`).Scan(&targetPlane, &state); err != nil || targetPlane != "rp_preview_available" || state != "running" {
		t.Fatalf("scheduled Preview Plane/state = %q/%q error=%v", targetPlane, state, err)
	}
	var reserved int
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces FROM resource_plane_capacities WHERE resource_plane_id='rp_preview_available'`).Scan(&reserved); err != nil || reserved != 2 {
		t.Fatalf("Preview capacity reservation = %d error=%v", reserved, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repository_previews SET desired_state='absent',state='cleanup_pending' WHERE tenant_id='tenant_preview_scheduled' AND pull_request_number=41`); err != nil {
		t.Fatalf("retire Preview for cleanup: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repository_preview_runtime_allocations SET desired_state='absent',state='cleanup_pending' WHERE tenant_id='tenant_preview_scheduled' AND pull_request_number=41`); err != nil {
		t.Fatalf("retire Preview runtime for cleanup: %v", err)
	}
	cleanupJob, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (previewstore.RuntimeCleanupJob, error) {
		jobs, err := previewstore.ClaimRuntimeCleanupDue(ctx, tx, "preview-scheduled-cleanup", now.Add(2*time.Second), 10*time.Second, 1)
		if err != nil || len(jobs) != 1 {
			return previewstore.RuntimeCleanupJob{}, fmt.Errorf("claim Preview cleanup: jobs=%d error=%w", len(jobs), err)
		}
		return jobs[0], nil
	})
	if err != nil {
		t.Fatalf("claim scheduled Preview cleanup: %v", err)
	}
	_, err = transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (bool, error) {
		return previewstore.MarkRuntimeAllocationCleaned(ctx, tx, cleanupJob, now.Add(3*time.Second))
	})
	if err != nil {
		t.Fatalf("confirm Preview cleanup: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT reserved_workspaces FROM resource_plane_capacities WHERE resource_plane_id='rp_preview_available'`).Scan(&reserved); err != nil || reserved != 1 {
		t.Fatalf("confirmed cleanup did not release Preview capacity: reserved=%d error=%v", reserved, err)
	}
}

type integrationProvisioner struct {
	failures             []error
	failureBindings      []previewstore.RuntimeBinding
	binding              previewstore.RuntimeBinding
	onProvision          func(previewstore.ProvisionJob)
	compensated          int
	compensatedBindings  []previewstore.RuntimeBinding
	compensationFailures []error
}

func (p *integrationProvisioner) Provision(_ context.Context, job previewstore.ProvisionJob) (previewstore.RuntimeBinding, error) {
	if p.onProvision != nil {
		p.onProvision(job)
	}
	if len(p.failures) > 0 {
		err := p.failures[0]
		p.failures = p.failures[1:]
		var binding previewstore.RuntimeBinding
		if len(p.failureBindings) > 0 {
			binding = p.failureBindings[0]
			p.failureBindings = p.failureBindings[1:]
			if binding.WorkspaceID == "" && binding.MicroVMID != "" {
				binding.WorkspaceID = job.RuntimeWorkspaceID
			}
		}
		return binding, err
	}
	binding := p.binding
	if binding.ResourcePlaneID == "" {
		binding.ResourcePlaneID = job.TargetResourcePlaneID
	}
	binding.WorkspaceID = job.RuntimeWorkspaceID
	return binding, nil
}

func (p *integrationProvisioner) Compensate(_ context.Context, _ previewstore.ProvisionJob, binding previewstore.RuntimeBinding) error {
	p.compensated++
	p.compensatedBindings = append(p.compensatedBindings, binding)
	if len(p.compensationFailures) > 0 {
		err := p.compensationFailures[0]
		p.compensationFailures = p.compensationFailures[1:]
		return err
	}
	return nil
}

func previewsBinding(plane, workspace, microVM, route string) previewstore.RuntimeBinding {
	return previewstore.RuntimeBinding{ResourcePlaneID: domain.ResourcePlaneID(plane), WorkspaceID: domain.WorkspaceID(workspace), MicroVMID: microVM, GatewayRouteID: route}
}
