package previewprovision

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/previews"
)

type provisionNeverBeginPool struct{}

func (provisionNeverBeginPool) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected database transaction")
}

type provisionerFunc struct {
	provision  func(context.Context, previews.ProvisionJob) (previews.RuntimeBinding, error)
	compensate func(context.Context, previews.ProvisionJob, previews.RuntimeBinding) error
}

func (f provisionerFunc) Provision(ctx context.Context, job previews.ProvisionJob) (previews.RuntimeBinding, error) {
	return f.provision(ctx, job)
}

func (f provisionerFunc) Compensate(ctx context.Context, job previews.ProvisionJob, binding previews.RuntimeBinding) error {
	return f.compensate(ctx, job, binding)
}

func TestClassifyUsesOnlyFixedProvisionErrorCategories(t *testing.T) {
	for _, test := range []struct {
		err  error
		want string
	}{
		{ErrRuntimeProfileMissing, "runtime_profile_missing"},
		{ErrMicroVMLaunch, "microvm_launch_failed"},
		{ErrGatewayRoutePublish, "gateway_route_publish_failed"},
		{errors.New("provider returned sensitive detail"), "provider_unavailable"},
	} {
		if got := classify(test.err); got != test.want {
			t.Errorf("classify(%v) = %q, want %q", test.err, got, test.want)
		}
	}
}

func TestRunOnceRejectsInvalidConfigurationBeforeDatabaseAccess(t *testing.T) {
	provisioner := provisionerFunc{
		provision: func(context.Context, previews.ProvisionJob) (previews.RuntimeBinding, error) {
			return previews.RuntimeBinding{}, nil
		},
		compensate: func(context.Context, previews.ProvisionJob, previews.RuntimeBinding) error { return nil },
	}
	valid := Worker{Pool: provisionNeverBeginPool{}, Provisioner: provisioner,
		Lease: 10 * time.Second, Timeout: time.Second, BatchSize: 1,
		NewClaimID: func() (string, error) { return "preview-provision-1234", nil }}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	tests := []struct {
		name   string
		worker Worker
		ctx    context.Context
	}{
		{name: "nil context", worker: valid},
		{name: "canceled context", worker: valid, ctx: canceled},
		{name: "missing pool", worker: Worker{Provisioner: provisioner}, ctx: context.Background()},
		{name: "missing provisioner", worker: Worker{Pool: valid.Pool}, ctx: context.Background()},
		{name: "short lease", worker: withProvisionConfig(valid, 4*time.Second, time.Second, 1), ctx: context.Background()},
		{name: "long lease", worker: withProvisionConfig(valid, 11*time.Minute, time.Second, 1), ctx: context.Background()},
		{name: "short timeout", worker: withProvisionConfig(valid, 10*time.Second, 500*time.Millisecond, 1), ctx: context.Background()},
		{name: "timeout reaches lease", worker: withProvisionConfig(valid, 10*time.Second, 10*time.Second, 1), ctx: context.Background()},
		{name: "invalid batch", worker: withProvisionConfig(valid, 10*time.Second, time.Second, 101), ctx: context.Background()},
		{name: "batch exceeds lease", worker: withProvisionConfig(valid, 10*time.Second, 5*time.Second, 2), ctx: context.Background()},
		{name: "claim ID generation fails", worker: withProvisionClaim(valid, "", errors.New("random source unavailable")), ctx: context.Background()},
		{name: "claim ID too short", worker: withProvisionClaim(valid, "short", nil), ctx: context.Background()},
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

func withProvisionConfig(worker Worker, lease, timeout time.Duration, batch int) Worker {
	worker.Lease, worker.Timeout, worker.BatchSize = lease, timeout, batch
	return worker
}

func withProvisionClaim(worker Worker, claimID string, err error) Worker {
	worker.NewClaimID = func() (string, error) { return claimID, err }
	return worker
}
