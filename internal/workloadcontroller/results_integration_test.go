//go:build integration

package workloadcontroller

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestRPResultReplaysIntoPostgresControlPlane(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES('tenant_1','Integration',now())`,
		`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES('rp_1','aws','ap-northeast-1','[]')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	c, _, r, now := setupController(t)
	if _, err := pool.Exec(ctx, `INSERT INTO workload_runs(id,tenant_id,kind,commit_sha,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		VALUES('run_1','tenant_1','job',$1,'standard','untrusted','running','provisioning','rp_1','integration','integration',86400,$2,2,$3,$3)`, strings.Repeat("c", 40), now.Add(24*time.Hour), *now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		VALUES('attempt_1','tenant_1','run_1',1,1,'rp_1','token_1','running',$1,$1)`, *now); err != nil {
		t.Fatal(err)
	}
	old := c.resolver
	c.resolver = resolveFunc(func(ctx context.Context, id workloadjournal.Command) (Specification, error) {
		s, err := old.Resolve(ctx, id)
		s.ArtifactPrefix = "s3://artifacts/logs/tenant_1/run_1/attempt_1/"
		return s, err
	})
	data := envelope(t, nil)
	if _, err := c.Ensure(ctx, data); err != nil {
		t.Fatal(err)
	}
	r.result = successfulResult(t, c)
	r.result.LogArtifact = &workloadjobs.WorkloadLogArtifact{StorageRef: "s3://artifacts/logs/tenant_1/run_1/attempt_1/log.txt", SizeBytes: 12, SHA256: strings.Repeat("d", 64), Redacted: true}
	handler := workloadjobs.ResultHandler{Pool: pool, Policy: transaction.DefaultPolicy()}
	sends := 0
	var outcomes []workloadjobs.ResultOutcome
	lost := errors.New("lost acknowledgement after CP commit")
	p := publishFunc(func(ctx context.Context, result workloadjobs.WorkloadResult) error {
		sends++
		outcome, err := handler.Handle(ctx, result)
		if err != nil {
			return err
		}
		outcomes = append(outcomes, outcome)
		if sends == 1 {
			return lost
		}
		return nil
	})
	r.cleanupErr = errors.New("Lambda MicroVM cleanup unavailable")
	pending, err := c.Poll(ctx, data, p)
	if err == nil || pending.Journal.CleanupState != workloadjournal.CleanupFailed ||
		pending.Journal.ResultPublished || len(pending.Journal.ResultPayload) == 0 || sends != 0 {
		t.Fatalf("cleanup failure published the terminal result: journal=%+v sends=%d err=%v", pending.Journal, sends, err)
	}
	var stateBeforeCleanup domain.WorkloadRunState
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_runs WHERE id='run_1'`).Scan(&stateBeforeCleanup); err != nil || stateBeforeCleanup != domain.WorkloadProvisioning {
		t.Fatalf("Control Plane Workload became terminal before RP cleanup: state=%s err=%v", stateBeforeCleanup, err)
	}
	r.cleanupErr = nil
	if _, err := c.Poll(ctx, data, p); !errors.Is(err, lost) {
		t.Fatal(err)
	}
	s, err := c.Poll(ctx, data, p)
	if err != nil {
		t.Fatal(err)
	}
	if len(outcomes) != 2 || outcomes[0] != workloadjobs.ResultApplied || outcomes[1] != workloadjobs.ResultDuplicate {
		t.Fatalf("CP outcomes %v", outcomes)
	}
	var state domain.WorkloadRunState
	var count int
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_runs WHERE id='run_1' AND tenant_id='tenant_1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_artifacts WHERE workload_run_id='run_1' AND tenant_id='tenant_1'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if state != domain.WorkloadSucceeded || count != 1 || !s.Journal.ResultPublished || s.Journal.CleanupState != workloadjournal.CleanupConfirmed {
		t.Fatalf("state=%s artifacts=%d journal=%+v", state, count, s.Journal)
	}
}

func TestCancellationOutboxAndRPFenceIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	c, _, r, now := setupController(t)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES('tenant_1','Cancel',now())`,
		`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES('rp_1','aws','ap-northeast-1','[]')`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_runs(id,tenant_id,kind,commit_sha,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		VALUES('run_1','tenant_1','job',$1,'standard','untrusted','running','provisioning','rp_1','cancel','cancel',86400,$2,2,$3,$3)`, strings.Repeat("c", 40), now.Add(24*time.Hour), *now); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		VALUES('attempt_1','tenant_1','run_1',1,1,'rp_1','token_1','dispatch_pending',$1,$1)`, *now); err != nil {
		t.Fatal(err)
	}
	// A rollback must undo both desired state and the command.
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := workloads.RequestCancel(ctx, tx, "tenant_1", "run_1", *now); err != nil {
		_ = tx.Rollback(ctx)
		t.Fatal(err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	var desired string
	var count int
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM workload_runs WHERE id='run_1'`).Scan(&desired); err != nil || desired != "running" {
		t.Fatal("rollback lost desired state", err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events`).Scan(&count); err != nil || count != 0 {
		t.Fatal("rollback left cancel event", err)
	}
	for i := 0; i < 2; i++ {
		_, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			return workloads.RequestCancel(ctx, tx, "tenant_1", "run_1", *now)
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM outbox_events WHERE event_type='workload_attempt.cancel'`).Scan(&count); err != nil || count != 1 {
		t.Fatal("duplicate cancel created extra outbox", err)
	}
	var payload, eventType string
	if err := pool.QueryRow(ctx, `SELECT payload_json,event_type FROM outbox_events WHERE event_type='workload_attempt.cancel'`).Scan(&payload, &eventType); err != nil {
		t.Fatal(err)
	}
	if _, err := c.HandleEvent(ctx, eventType, []byte(payload)); err != nil {
		t.Fatal(err)
	}
	// Simulate original dispatch arriving after cancellation.
	if _, err := c.HandleEvent(ctx, "workload_attempt.dispatch", []byte(payload)); err != nil || r.launches != 0 {
		t.Fatal("late dispatch launched cancelled attempt", err)
	}
	handler := workloadjobs.ResultHandler{Pool: pool, Policy: transaction.DefaultPolicy()}
	s, err := c.Poll(ctx, []byte(payload), publishFunc(func(ctx context.Context, result workloadjobs.WorkloadResult) error {
		_, err := handler.Handle(ctx, result)
		return err
	}))
	if err != nil {
		t.Fatal(err)
	}
	var state string
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_runs WHERE id='run_1'`).Scan(&state); err != nil || state != "cancelled" || !s.Journal.ResultPublished {
		t.Fatal("cancel result not applied", err)
	}
}
