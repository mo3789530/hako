package workspaceruntime

import (
	"context"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

type localResolverFunc func(context.Context, resourcecontroller.Command) ([]byte, error)

func (f localResolverFunc) ResolveLocal(ctx context.Context, command resourcecontroller.Command) ([]byte, error) {
	return f(ctx, command)
}

type localDriverFake struct {
	states       map[string]string
	launches     map[string][]byte
	ensureCalls  int
	suspendCalls int
	resumeCalls  int
	deleteCalls  int
	failAfter    map[string]error
}

func newLocalDriverFake() *localDriverFake {
	return &localDriverFake{states: map[string]string{}, launches: map[string][]byte{}, failAfter: map[string]error{}}
}

func (d *localDriverFake) EnsureRunning(_ context.Context, id string, payload []byte) error {
	d.ensureCalls++
	if old, ok := d.launches[id]; ok && string(old) != string(payload) {
		return errors.New("allocation launch specification changed")
	}
	d.launches[id] = append([]byte(nil), payload...)
	d.states[id] = "running"
	return d.fail("ensure")
}

func (d *localDriverFake) Suspend(_ context.Context, id string) error {
	d.suspendCalls++
	if d.states[id] != "" && d.states[id] != "running" && d.states[id] != "suspended" {
		return errors.New("allocation is not running")
	}
	d.states[id] = "suspended"
	return d.fail("suspend")
}

func (d *localDriverFake) Resume(_ context.Context, id string, payload []byte) error {
	d.resumeCalls++
	if d.states[id] != "suspended" && d.states[id] != "running" {
		return errors.New("allocation is not suspended")
	}
	if string(d.launches[id]) != string(payload) {
		return errors.New("resume did not reuse pinned launch specification")
	}
	d.states[id] = "running"
	return d.fail("resume")
}

func (d *localDriverFake) Delete(_ context.Context, id string) error {
	d.deleteCalls++
	delete(d.states, id)
	delete(d.launches, id)
	return d.fail("delete")
}

func (d *localDriverFake) fail(operation string) error {
	err := d.failAfter[operation]
	delete(d.failAfter, operation)
	return err
}

func TestLocalMicroVMLifecycleSurvivesRuntimeReconstruction(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	resolverCalls := 0
	resolver := localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		resolverCalls++
		return []byte(`{"kernel":"/opt/hako/kernel","initrd":"/opt/hako/guest.cpio","disk":"/var/lib/hako/workspaces/ws_test.raw"}`), nil
	})
	makeRuntime := func() *LocalMicroVM {
		runtime, err := NewLocalMicroVM(driver, store, resolver, func() time.Time { now = now.Add(time.Second); return now })
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	runtime := makeRuntime()
	ensure := testCommand(domain.OperationEnsureRunning, 1, "op_local_ensure")
	if state, err := runtime.Execute(ctx, ensure); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("ensure = %q, %v", state, err)
	}
	if state, err := makeRuntime().Execute(ctx, ensure); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("duplicate ensure after runtime reconstruction = %q, %v", state, err)
	}
	if resolverCalls != 1 || driver.ensureCalls != 2 || len(driver.states) != 1 {
		t.Fatalf("duplicate ensure changed allocation or re-resolved launch: resolver=%d ensure=%d allocations=%d", resolverCalls, driver.ensureCalls, len(driver.states))
	}

	suspend := testCommand(domain.OperationSuspend, 2, "op_local_suspend")
	if state, err := makeRuntime().Execute(ctx, suspend); err != nil || state != domain.ObservedWorkspaceSuspended {
		t.Fatalf("suspend = %q, %v", state, err)
	}
	resume := testCommand(domain.OperationResume, 3, "op_local_resume")
	if state, err := makeRuntime().Execute(ctx, resume); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("resume = %q, %v", state, err)
	}
	if resolverCalls != 1 || driver.resumeCalls != 1 {
		t.Fatalf("resume resolved a new launch or was not run once: resolver=%d resume=%d", resolverCalls, driver.resumeCalls)
	}
	if kind, id, generation, err := makeRuntime().ResourceBinding(ctx, resume); err != nil || kind != "cloud_hypervisor_local" || id == "" || generation != 1 {
		t.Fatalf("resource binding = %q %q %d, %v", kind, id, generation, err)
	}

	remove := testCommand(domain.OperationDelete, 4, "op_local_delete")
	if state, err := makeRuntime().Execute(ctx, remove); err != nil || state != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete = %q, %v", state, err)
	}
	if state, err := makeRuntime().Execute(ctx, remove); err != nil || state != domain.ObservedWorkspaceDeleted {
		t.Fatalf("duplicate delete = %q, %v", state, err)
	}
	if driver.suspendCalls != 1 || driver.resumeCalls != 1 || driver.deleteCalls != 1 {
		t.Fatalf("unexpected driver calls: suspend=%d resume=%d delete=%d", driver.suspendCalls, driver.resumeCalls, driver.deleteCalls)
	}
	if _, err := makeRuntime().Execute(ctx, ensure); !errors.Is(err, workspacejournal.ErrStale) {
		t.Fatalf("delayed ensure after tombstone was not rejected: %v", err)
	}
}

func TestLocalMicroVMRetriesAmbiguousDriverResultsFromDurableJournal(t *testing.T) {
	ctx := context.Background()
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	resolver := localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"kernel":"/opt/hako/kernel","initrd":"/opt/hako/guest.cpio"}`), nil
	})
	runtime, err := NewLocalMicroVM(driver, store, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	makeRuntime := func() *LocalMicroVM {
		runtime, err := NewLocalMicroVM(driver, store, resolver, nil)
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	ensure := testCommand(domain.OperationEnsureRunning, 1, "op_local_retry_ensure")
	driver.failAfter["ensure"] = errors.New("driver response lost after launch")
	if _, err := runtime.Execute(ctx, ensure); err == nil {
		t.Fatal("ambiguous ensure result was acknowledged")
	}
	if state, err := makeRuntime().Execute(ctx, ensure); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("ensure retry after runtime reconstruction = %q, %v", state, err)
	}

	suspend := testCommand(domain.OperationSuspend, 2, "op_local_retry_suspend")
	driver.failAfter["suspend"] = errors.New("driver response lost after stop")
	if _, err := makeRuntime().Execute(ctx, suspend); err == nil {
		t.Fatal("ambiguous suspend result was acknowledged")
	}
	if state, err := makeRuntime().Execute(ctx, suspend); err != nil || state != domain.ObservedWorkspaceSuspended {
		t.Fatalf("suspend retry = %q, %v", state, err)
	}

	resume := testCommand(domain.OperationResume, 3, "op_local_retry_resume")
	driver.failAfter["resume"] = errors.New("driver response lost after boot")
	if _, err := makeRuntime().Execute(ctx, resume); err == nil {
		t.Fatal("ambiguous resume result was acknowledged")
	}
	if state, err := makeRuntime().Execute(ctx, resume); err != nil || state != domain.ObservedWorkspaceRunning {
		t.Fatalf("resume retry = %q, %v", state, err)
	}

	remove := testCommand(domain.OperationDelete, 4, "op_local_retry_delete")
	driver.failAfter["delete"] = errors.New("driver response lost after cleanup")
	if _, err := makeRuntime().Execute(ctx, remove); err == nil {
		t.Fatal("ambiguous delete result was acknowledged")
	}
	if state, err := makeRuntime().Execute(ctx, remove); err != nil || state != domain.ObservedWorkspaceDeleted {
		t.Fatalf("delete retry = %q, %v", state, err)
	}
	if driver.ensureCalls != 2 || driver.suspendCalls != 2 || driver.resumeCalls != 2 || driver.deleteCalls != 2 {
		t.Fatalf("idempotent retries were not exercised: ensure=%d suspend=%d resume=%d delete=%d", driver.ensureCalls, driver.suspendCalls, driver.resumeCalls, driver.deleteCalls)
	}
}

func TestLocalMicroVMSerializesConcurrentDuplicateOperations(t *testing.T) {
	ctx := context.Background()
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	resolverCalls := 0
	resolver := localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		resolverCalls++
		return []byte(`{"kernel":"/opt/hako/kernel","initrd":"/opt/hako/guest.cpio"}`), nil
	})
	one, err := NewLocalMicroVM(driver, store, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	two, err := NewLocalMicroVM(driver, store, resolver, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := testCommand(domain.OperationEnsureRunning, 1, "op_local_concurrent")
	start := make(chan struct{})
	results := make(chan error, 2)
	var wait sync.WaitGroup
	for _, runtime := range []*LocalMicroVM{one, two} {
		wait.Add(1)
		go func(runtime *LocalMicroVM) {
			defer wait.Done()
			<-start
			observed, err := runtime.Execute(ctx, command)
			if err == nil && observed != domain.ObservedWorkspaceRunning {
				err = errors.New("duplicate operation returned a non-running state")
			}
			results <- err
		}(runtime)
	}
	close(start)
	wait.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if resolverCalls != 1 || driver.ensureCalls != 2 || len(driver.states) != 1 {
		t.Fatalf("concurrent duplicate changed allocation or re-resolved launch: resolver=%d ensure=%d allocations=%d", resolverCalls, driver.ensureCalls, len(driver.states))
	}
}

func TestLocalMicroVMReconcilesDurableAllocationsAfterControllerRestart(t *testing.T) {
	ctx := context.Background()
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	resolver := localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"kernel":"/opt/hako/kernel","initrd":"/opt/hako/guest.cpio"}`), nil
	})
	makeRuntime := func() *LocalMicroVM {
		runtime, err := NewLocalMicroVM(driver, store, resolver, nil)
		if err != nil {
			t.Fatal(err)
		}
		return runtime
	}
	if _, err := makeRuntime().Execute(ctx, testCommand(domain.OperationEnsureRunning, 1, "op_local_reconcile_ensure")); err != nil {
		t.Fatal(err)
	}
	// Simulate a controller process restart losing process-local state while
	// the durable journal and allocation disk remain.
	driver.states = map[string]string{}
	results, err := makeRuntime().Reconcile(ctx, 10)
	if err != nil || len(results) != 1 || results[0].Err != nil || results[0].Observed != domain.ObservedWorkspaceRunning {
		t.Fatalf("running reconciliation = %+v, %v", results, err)
	}
	if len(driver.states) != 1 || driver.ensureCalls != 2 {
		t.Fatalf("running allocation was not recovered: states=%v ensureCalls=%d", driver.states, driver.ensureCalls)
	}

	if _, err := makeRuntime().Execute(ctx, testCommand(domain.OperationSuspend, 2, "op_local_reconcile_suspend")); err != nil {
		t.Fatal(err)
	}
	driver.states = map[string]string{}
	results, err = makeRuntime().Reconcile(ctx, 10)
	if err != nil || len(results) != 1 || results[0].Err != nil || results[0].Observed != domain.ObservedWorkspaceSuspended {
		t.Fatalf("suspended reconciliation = %+v, %v", results, err)
	}
	if driver.suspendCalls != 2 {
		t.Fatalf("suspended allocation was not confirmed stopped: suspendCalls=%d", driver.suspendCalls)
	}
}

func TestLocalMicroVMRequiresTrustedDependenciesAndRejectsInitialResume(t *testing.T) {
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewLocalMicroVM(nil, store, localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) { return nil, nil }), nil); err == nil {
		t.Fatal("nil driver was accepted")
	}
	if _, err := NewLocalMicroVM(newLocalDriverFake(), newMemoryStore(), localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"kernel":"/opt/hako/kernel"}`), nil
	}), nil); err == nil {
		t.Fatal("journal without process-shared Workspace operation locks was accepted")
	}
	runtime, err := NewLocalMicroVM(newLocalDriverFake(), store, localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"kernel":"/opt/hako/kernel"}`), nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	resume := testCommand(domain.OperationResume, 1, "op_local_resume_first")
	if state, err := runtime.Execute(context.Background(), resume); state != domain.ObservedWorkspaceFailed || !errors.Is(err, workspacejournal.ErrConflict) {
		t.Fatalf("initial resume = %q, %v", state, err)
	}
}

func TestLocalMicroVMReconcileLoopRunsImmediatelyRetriesAndStops(t *testing.T) {
	store, err := workspacejournal.NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	runtime, err := NewLocalMicroVM(newLocalDriverFake(), store, localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"kernel_image_path":"/kernel"}`), nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	reports := make(chan struct{}, 3)
	loopErr := make(chan error, 1)
	go func() {
		loopErr <- RunLocalReconcileLoop(ctx, runtime, 100*time.Millisecond, 10, func(results []LocalReconcileResult, err error) {
			if err == nil && len(results) == 0 {
				reports <- struct{}{}
			}
		})
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-reports:
		case <-time.After(time.Second):
			t.Fatalf("reconcile report %d did not arrive", i+1)
		}
	}
	cancel()
	select {
	case err := <-loopErr:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("loop error=%v, want context cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("reconcile loop did not stop after cancellation")
	}
}

func TestLocalMicroVMReconcileLoopRejectsUnboundedConfiguration(t *testing.T) {
	if err := RunLocalReconcileLoop(context.Background(), &LocalMicroVM{}, time.Second, 0, nil); err == nil {
		t.Fatal("reconcile loop accepted an invalid batch limit")
	}
}
