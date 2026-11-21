package previewdispatch

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestRunOnceRejectsInvalidConfigurationBeforeDatabaseAccess(t *testing.T) {
	for _, test := range []struct {
		name   string
		worker Worker
		ctx    context.Context
	}{
		{name: "missing pool", worker: Worker{RuntimeClass: "standard"}, ctx: context.Background()},
		{name: "missing placement policy", worker: Worker{Pool: rejectingPool{}, BatchSize: 1}, ctx: context.Background()},
		{name: "invalid plane", worker: Worker{Pool: rejectingPool{}, TargetResourcePlaneID: "bad plane", BatchSize: 1}, ctx: context.Background()},
		{name: "invalid lease", worker: Worker{Pool: rejectingPool{}, TargetResourcePlaneID: "rp-test", Lease: time.Second}, ctx: context.Background()},
		{name: "invalid batch", worker: Worker{Pool: rejectingPool{}, TargetResourcePlaneID: "rp-test", BatchSize: 101}, ctx: context.Background()},
		{name: "nil context", worker: Worker{Pool: rejectingPool{}, TargetResourcePlaneID: "rp-test"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			stats, err := test.worker.RunOnce(test.ctx)
			if !errors.Is(err, ErrWorkerConfiguration) || stats != (Stats{}) {
				t.Fatalf("RunOnce() = %+v, %v; want empty stats and configuration error", stats, err)
			}
		})
	}
}

func TestRunCleanupOnceRejectsInvalidConfigurationBeforeDatabaseAccess(t *testing.T) {
	worker := Worker{Pool: rejectingPool{}, BatchSize: 101}
	stats, err := worker.RunCleanupOnce(context.Background())
	if !errors.Is(err, ErrWorkerConfiguration) || stats != (Stats{}) {
		t.Fatalf("RunCleanupOnce() = %+v, %v; want empty stats and configuration error", stats, err)
	}
}

// Begin panics if validation accidentally reaches database access.
type rejectingPool struct{}

func (rejectingPool) Begin(context.Context) (pgx.Tx, error) { panic("unexpected database access") }
