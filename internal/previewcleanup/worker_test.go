package previewcleanup

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/previews"
)

type cleanupNeverBeginPool struct{}

func (cleanupNeverBeginPool) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected database transaction")
}

type cleanupExecutorFunc func(context.Context, previews.CleanupJob) error

func (f cleanupExecutorFunc) Cleanup(ctx context.Context, job previews.CleanupJob) error {
	return f(ctx, job)
}

func TestClassifyUsesOnlyBoundedCleanupCategories(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{ErrBindingMissing, "preview_binding_missing"},
		{ErrGatewayRouteRemoval, "gateway_route_remove_failed"},
		{ErrMicroVMTermination, "microvm_terminate_failed"},
		{errors.New("provider error contains a secret"), "provider_unavailable"},
	} {
		if got := classify(test.err); got != test.want {
			t.Errorf("classify(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestRunOnceRejectsInvalidWorkerConfiguration(t *testing.T) {
	valid := Worker{Pool: cleanupNeverBeginPool{}, Executor: cleanupExecutorFunc(func(context.Context, previews.CleanupJob) error { return nil }),
		Lease: 10 * time.Second, Timeout: time.Second, BatchSize: 1, NewClaimID: func() (string, error) { return "claim-cleanup-1234", nil }}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		worker Worker
		ctx    context.Context
	}{
		{name: "nil context", worker: valid},
		{name: "cancelled context", worker: valid, ctx: canceled},
		{name: "missing pool", worker: Worker{Executor: valid.Executor}, ctx: context.Background()},
		{name: "missing executor", worker: Worker{Pool: valid.Pool}, ctx: context.Background()},
		{name: "lease below minimum", worker: withCleanupConfig(valid, 4*time.Second, time.Second, 1), ctx: context.Background()},
		{name: "lease above maximum", worker: withCleanupConfig(valid, 11*time.Minute, time.Second, 1), ctx: context.Background()},
		{name: "timeout below minimum", worker: withCleanupConfig(valid, 10*time.Second, 500*time.Millisecond, 1), ctx: context.Background()},
		{name: "timeout reaches lease", worker: withCleanupConfig(valid, 10*time.Second, 10*time.Second, 1), ctx: context.Background()},
		{name: "batch above maximum", worker: withCleanupConfig(valid, 10*time.Minute, time.Second, 101), ctx: context.Background()},
		{name: "batch timeout exceeds lease", worker: withCleanupConfig(valid, 10*time.Second, 5*time.Second, 2), ctx: context.Background()},
		{name: "claim id generation failed", worker: withCleanupClaim(valid, "", errors.New("entropy unavailable")), ctx: context.Background()},
		{name: "claim id too short", worker: withCleanupClaim(valid, "short", nil), ctx: context.Background()},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stats, err := test.worker.RunOnce(test.ctx)
			if !errors.Is(err, ErrWorkerConfiguration) {
				t.Fatalf("RunOnce() error = %v, want ErrWorkerConfiguration", err)
			}
			if stats != (Stats{}) {
				t.Fatalf("RunOnce() stats = %+v, want empty stats", stats)
			}
		})
	}
}

func withCleanupConfig(worker Worker, lease, timeout time.Duration, batch int) Worker {
	worker.Lease, worker.Timeout, worker.BatchSize = lease, timeout, batch
	return worker
}

func withCleanupClaim(worker Worker, claim string, err error) Worker {
	worker.NewClaimID = func() (string, error) { return claim, err }
	return worker
}
