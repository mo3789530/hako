package previewresult

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/previewprotocol"
)

type previewResultNeverBeginPool struct{}

func (previewResultNeverBeginPool) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected database transaction")
}

func TestConsumerHandleRejectsInvalidInputsBeforeDatabaseAccess(t *testing.T) {
	now := time.Date(2026, 10, 25, 2, 0, 0, 0, time.UTC)
	result := previewprotocol.Result{
		SchemaVersion: 1, Action: previewprotocol.ActionProvision, TenantID: "tenant_test", InstallationID: 17,
		RepositoryID: 23, PullRequestNumber: 4, Generation: 2,
		RuntimeWorkspaceID: "prvws_0123456789abcdef0123456789abcdef", ResourcePlaneID: "rp-test",
		ClaimID: "preview-claim-1234", ClaimAttempt: 1, LeaseExpiresAt: now.Add(time.Minute),
		Status: "succeeded", Binding: &previewprotocol.RuntimeBinding{MicroVMID: "microvm-1", GatewayRouteID: "route-1"},
		CompletedAt: now,
	}
	payload, err := previewprotocol.EncodeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	consumer := Consumer{Pool: previewResultNeverBeginPool{}, Now: func() time.Time { return now }}
	tests := []struct {
		name     string
		consumer Consumer
		ctx      context.Context
		payload  []byte
		claimID  string
		action   string
		planeID  string
		wantErr  error
	}{
		{name: "nil context", consumer: consumer, payload: payload, claimID: result.ClaimID, action: result.Action, planeID: result.ResourcePlaneID, wantErr: ErrConfiguration},
		{name: "cancelled context", consumer: consumer, ctx: canceled, payload: payload, claimID: result.ClaimID, action: result.Action, planeID: result.ResourcePlaneID, wantErr: ErrConfiguration},
		{name: "missing pool", consumer: Consumer{}, ctx: context.Background(), payload: payload, claimID: result.ClaimID, action: result.Action, planeID: result.ResourcePlaneID, wantErr: ErrConfiguration},
		{name: "malformed result", consumer: consumer, ctx: context.Background(), payload: []byte("{"), claimID: result.ClaimID, action: result.Action, planeID: result.ResourcePlaneID},
		{name: "claim attribute mismatch", consumer: consumer, ctx: context.Background(), payload: payload, claimID: "foreign-claim-1234", action: result.Action, planeID: result.ResourcePlaneID},
		{name: "action attribute mismatch", consumer: consumer, ctx: context.Background(), payload: payload, claimID: result.ClaimID, action: previewprotocol.ActionCleanup, planeID: result.ResourcePlaneID},
		{name: "plane attribute mismatch", consumer: consumer, ctx: context.Background(), payload: payload, claimID: result.ClaimID, action: result.Action, planeID: "rp-foreign"},
		{name: "completion too far in future", consumer: consumer, ctx: context.Background(), payload: futureResultPayload(t, result, now.Add(5*time.Minute+time.Nanosecond)), claimID: result.ClaimID, action: result.Action, planeID: result.ResourcePlaneID},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			applied, err := test.consumer.Handle(test.ctx, test.payload, test.claimID, test.action, test.planeID)
			if err == nil {
				t.Fatal("Handle() unexpectedly accepted invalid input")
			}
			if test.wantErr != nil && !errors.Is(err, test.wantErr) {
				t.Fatalf("Handle() error = %v, want %v", err, test.wantErr)
			}
			if applied {
				t.Fatal("Handle() reported a database result for rejected input")
			}
		})
	}
}

func futureResultPayload(t *testing.T, result previewprotocol.Result, completedAt time.Time) []byte {
	t.Helper()
	result.CompletedAt = completedAt
	payload, err := previewprotocol.EncodeResult(result)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
