//go:build integration

package previewdispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewcontroller"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/previewresult"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPreviewCommandProducerOutboxAndDispatcherEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_preview_dispatch','Preview Dispatch',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_preview_dispatch','preview-dispatch','',$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_preview_dispatch',9911,'acme','active','usr_preview_dispatch',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9911,'tenant_preview_dispatch',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,preview_enabled,preview_enabled_at) VALUES ('tenant_preview_dispatch',9911,9912,'acme','preview-dispatch','main',$1,true,$1)`,
		`INSERT INTO repository_previews(tenant_id,installation_id,github_repository_id,pull_request_number,head_sha,head_ref,desired_state,state,last_delivery_id,last_event_at,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_dispatch',9911,9912,41,$2,'feature/dispatch','present','awaiting_runtime','71000000-0000-4000-8000-000000000001',$1,$3,$1,$1)`,
		`INSERT INTO repository_preview_runtime_allocations(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		 VALUES ('tenant_preview_dispatch',9911,9912,41,1,$2,'feature/dispatch','present','awaiting_runtime',$3,$1,$1)`,
		`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp-preview-dispatch','aws','ap-northeast-1','["microvm"]')`,
	} {
		args := []any{now}
		if strings.Contains(query, "repository_previews(") || strings.Contains(query, "repository_preview_runtime_allocations(") {
			args = []any{now, strings.Repeat("a", 40), now.Add(time.Hour)}
		} else if strings.Contains(query, "resource_planes(") {
			args = nil
		}
		if _, err := pool.Exec(ctx, query, args...); err != nil {
			t.Fatalf("seed Preview producer case: %v", err)
		}
	}

	eventID := "evt_preview_dispatch_1"
	producer := Worker{
		Pool: pool, TargetResourcePlaneID: domain.ResourcePlaneID("rp-preview-dispatch"),
		Now:        func() time.Time { return now.Add(time.Second) },
		NewClaimID: func() (string, error) { return "preview-provision-dispatch", nil },
		NewEventID: func() (string, error) { return eventID, nil },
		Lease:      time.Minute, BatchSize: 1,
	}
	first, err := producer.RunOnce(ctx)
	if err != nil || first != (Stats{Claimed: 1, Enqueued: 1}) {
		t.Fatalf("produce Preview command: stats=%+v err=%v", first, err)
	}
	second, err := producer.RunOnce(ctx)
	if err != nil || second != (Stats{}) {
		t.Fatalf("duplicate producer pass claimed active generation: stats=%+v err=%v", second, err)
	}

	var aggregateType, aggregateID, eventType, payload string
	if err := pool.QueryRow(ctx, `SELECT aggregate_type,aggregate_id,event_type,payload_json FROM outbox_events WHERE id=$1`, eventID).
		Scan(&aggregateType, &aggregateID, &eventType, &payload); err != nil {
		t.Fatalf("read committed Preview command: %v", err)
	}
	command, err := previewprotocol.Decode([]byte(payload), eventType, aggregateID)
	if err != nil || aggregateType != previewprotocol.AggregateType || command.ResourcePlaneID != "rp-preview-dispatch" || command.HeadSHA != strings.Repeat("a", 40) {
		t.Fatalf("stored Preview command invalid: aggregate=%q command=%+v err=%v", aggregateType, command, err)
	}
	var allocationState, pinnedPlane string
	if err := pool.QueryRow(ctx, `SELECT state,target_resource_plane_id FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41 AND generation=1`).Scan(&allocationState, &pinnedPlane); err != nil || allocationState != "provisioning" || pinnedPlane != "rp-preview-dispatch" {
		t.Fatalf("claim was not atomically pinned: state=%q plane=%q err=%v", allocationState, pinnedPlane, err)
	}

	publisher := &capturePublisher{}
	d, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool}, publisher, map[string]string{"rp-preview-dispatch": "workspace-queue"}, dispatcher.Config{
		PreviewRuntimeCommandQueueURLs: map[string]string{"rp-preview-dispatch": "preview-command-queue"},
		Now:                            func() time.Time { return now.Add(2 * time.Second) },
	})
	if err != nil {
		t.Fatal(err)
	}
	dispatched, err := d.RunOnce(ctx)
	if err != nil || dispatched != (dispatcher.Stats{Claimed: 1, Published: 1}) || publisher.queueURL != "preview-command-queue" || publisher.event.ID != eventID {
		t.Fatalf("dispatch committed Preview command: stats=%+v published=%+v err=%v", dispatched, publisher, err)
	}
	var publishedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT published_at FROM outbox_events WHERE id=$1`, eventID).Scan(&publishedAt); err != nil || publishedAt == nil {
		t.Fatalf("Outbox command was not acknowledged after publish: published_at=%v err=%v", publishedAt, err)
	}

	runtime := &previewRuntimeFake{binding: previewcontroller.Binding{MicroVMID: "microvm-preview-41", GatewayRouteID: "preview-route-41"}}
	resultSink := &previewResultFake{}
	resourceController, err := previewcontroller.New("rp-preview-dispatch", runtime, resultSink,
		func() time.Time { return now.Add(3 * time.Second) }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := resourceController.Handle(ctx, publisher.event.Payload, publisher.event.EventType, publisher.event.AggregateID); err != nil || len(resultSink.results) != 1 {
		t.Fatalf("Resource Plane fake did not execute/report Preview command: results=%+v err=%v", resultSink.results, err)
	}
	resultPayload, err := previewprotocol.EncodeResult(resultSink.results[0])
	if err != nil {
		t.Fatalf("encode Preview result: %v", err)
	}
	resultConsumer := previewresult.Consumer{Pool: pool, Now: func() time.Time { return now.Add(4 * time.Second) }}
	applied, err := resultConsumer.Handle(ctx, resultPayload, resultSink.results[0].ClaimID, resultSink.results[0].Action, resultSink.results[0].ResourcePlaneID)
	if err != nil || !applied {
		t.Fatalf("apply successful Preview result: applied=%v err=%v", applied, err)
	}
	applied, err = resultConsumer.Handle(ctx, resultPayload, resultSink.results[0].ClaimID, resultSink.results[0].Action, resultSink.results[0].ResourcePlaneID)
	if err != nil || applied {
		t.Fatalf("duplicate Preview result was not an idempotent stale no-op: applied=%v err=%v", applied, err)
	}
	var finalState, microvmID, routeID string
	if err := pool.QueryRow(ctx, `SELECT state,microvm_id,gateway_route_id FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41 AND generation=1`).Scan(&finalState, &microvmID, &routeID); err != nil ||
		finalState != "running" || microvmID != runtime.binding.MicroVMID || routeID != runtime.binding.GatewayRouteID {
		t.Fatalf("Preview binding projection = %q %q %q err=%v", finalState, microvmID, routeID, err)
	}
	closedAt := now.Add(5 * time.Second)
	if _, err := pool.Exec(ctx, `UPDATE repository_previews SET desired_state='absent',state='cleanup_pending',updated_at=$1
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41`, closedAt); err != nil {
		t.Fatalf("retire Preview: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE repository_preview_runtime_allocations SET desired_state='absent',state='cleanup_pending',updated_at=$1
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41 AND generation=1`, closedAt); err != nil {
		t.Fatalf("retire Preview generation: %v", err)
	}
	cleanupWorker := Worker{Pool: pool, Now: func() time.Time { return closedAt.Add(time.Second) }, Lease: time.Minute, BatchSize: 1,
		NewClaimID: func() (string, error) { return "preview-cleanup-dispatch", nil },
		NewEventID: func() (string, error) { return "evt_preview_cleanup_1", nil }}
	cleanupStats, err := cleanupWorker.RunCleanupOnce(ctx)
	if err != nil || cleanupStats != (Stats{Claimed: 1, Enqueued: 1}) {
		t.Fatalf("enqueue retired Preview cleanup: stats=%+v err=%v", cleanupStats, err)
	}
	cleanupPublisher := &capturePublisher{}
	cleanupDispatcher, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool}, cleanupPublisher,
		map[string]string{"rp-preview-dispatch": "workspace-queue"}, dispatcher.Config{
			PreviewRuntimeCommandQueueURLs: map[string]string{"rp-preview-dispatch": "preview-command-queue"},
			Now:                            func() time.Time { return closedAt.Add(2 * time.Second) },
		})
	if err != nil {
		t.Fatal(err)
	}
	cleanupDispatchStats, err := cleanupDispatcher.RunOnce(ctx)
	if err != nil || cleanupDispatchStats != (dispatcher.Stats{Claimed: 1, Published: 1}) || cleanupPublisher.event.ID != "evt_preview_cleanup_1" {
		t.Fatalf("dispatch Preview cleanup command: stats=%+v event=%+v err=%v", cleanupDispatchStats, cleanupPublisher.event, err)
	}
	cleanupController, err := previewcontroller.New("rp-preview-dispatch", runtime, resultSink,
		func() time.Time { return closedAt.Add(3 * time.Second) }, 30*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := cleanupController.Handle(ctx, cleanupPublisher.event.Payload, cleanupPublisher.event.EventType, cleanupPublisher.event.AggregateID); err != nil {
		t.Fatalf("execute Preview cleanup command: %v", err)
	}
	cleanupResult := resultSink.results[len(resultSink.results)-1]
	if cleanupResult.Action != previewprotocol.ActionCleanup || cleanupResult.Status != "succeeded" || runtime.cleanupCalls != 1 {
		t.Fatalf("cleanup result = %+v cleanup calls=%d", cleanupResult, runtime.cleanupCalls)
	}
	cleanupPayload, err := previewprotocol.EncodeResult(cleanupResult)
	if err != nil {
		t.Fatalf("encode cleanup result: %v", err)
	}
	cleanupConsumer := previewresult.Consumer{Pool: pool, Now: func() time.Time { return closedAt.Add(4 * time.Second) }}
	cleaned, err := cleanupConsumer.Handle(ctx, cleanupPayload, cleanupResult.ClaimID, cleanupResult.Action, cleanupResult.ResourcePlaneID)
	if err != nil || !cleaned {
		t.Fatalf("apply Preview cleanup result: cleaned=%v err=%v", cleaned, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41 AND generation=1`).Scan(&finalState); err != nil || finalState != "cleaned" {
		t.Fatalf("Preview runtime was not cleaned: state=%q err=%v", finalState, err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO repository_preview_runtime_allocations
		(tenant_id,installation_id,github_repository_id,pull_request_number,generation,head_sha,head_ref,desired_state,state,expires_at,created_at,updated_at)
		VALUES ('tenant_preview_dispatch',9911,9912,41,2,$1,'feature/unstarted','absent','cleanup_pending',$2,$3,$3)`,
		strings.Repeat("b", 40), closedAt.Add(time.Hour), closedAt); err != nil {
		t.Fatalf("seed never-provisioned retired generation: %v", err)
	}
	cleanupWorker.NewClaimID = func() (string, error) { return "preview-cleanup-no-provider", nil }
	cleanupWorker.NewEventID = func() (string, error) { return "evt_preview_cleanup_unused", nil }
	noProvider, err := cleanupWorker.RunCleanupOnce(ctx)
	if err != nil || noProvider != (Stats{Claimed: 1, CleanedWithoutProvider: 1}) {
		t.Fatalf("never-provisioned generation was not cleaned locally: stats=%+v err=%v", noProvider, err)
	}
	if err := pool.QueryRow(ctx, `SELECT state FROM repository_preview_runtime_allocations
		WHERE tenant_id='tenant_preview_dispatch' AND pull_request_number=41 AND generation=2`).Scan(&finalState); err != nil || finalState != "cleaned" {
		t.Fatalf("never-provisioned generation state=%q err=%v", finalState, err)
	}
}

type capturePublisher struct {
	queueURL string
	event    outbox.Event
}

func (p *capturePublisher) Publish(_ context.Context, queueURL string, event outbox.Event) error {
	if p.queueURL != "" {
		return fmt.Errorf("unexpected duplicate publish to %s", queueURL)
	}
	p.queueURL = queueURL
	p.event = event
	return nil
}

type previewRuntimeFake struct {
	binding      previewcontroller.Binding
	cleanupCalls int
}

func (r *previewRuntimeFake) Ensure(context.Context, previewprotocol.Command) (previewcontroller.Binding, error) {
	return r.binding, nil
}
func (r *previewRuntimeFake) Cleanup(context.Context, previewprotocol.Command) error {
	r.cleanupCalls++
	return nil
}
func (*previewRuntimeFake) Compensate(context.Context, previewprotocol.Command, previewcontroller.Binding) error {
	return nil
}

type previewResultFake struct {
	results []previewcontroller.Result
}

func (s *previewResultFake) Report(_ context.Context, result previewcontroller.Result) error {
	s.results = append(s.results, result)
	return nil
}
