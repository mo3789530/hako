//go:build linux

package workloadagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/mo3789530/hako/internal/factoryconfig"
)

func TestPipelineProcessBudgetReservesServiceResources(t *testing.T) {
	gotMemory, gotCPU, err := pipelineProcessBudget(factoryconfig.ExecutionPlan{
		Pipeline: "integration", MemoryMB: 2048, CPUMillis: 1500, Services: []string{"postgres", "redis"},
	})
	if err != nil || gotMemory != 768 || gotCPU != 500 {
		t.Fatalf("pipelineProcessBudget() = %d MiB, %dm CPU, %v; want 768 MiB, 500m CPU", gotMemory, gotCPU, err)
	}
	if _, _, err := pipelineProcessBudget(factoryconfig.ExecutionPlan{Pipeline: "bad", MemoryMB: 2048, CPUMillis: 1500, Services: []string{"unknown"}}); err == nil {
		t.Fatal("unknown Service received a process resource budget")
	}
	if _, _, err := pipelineProcessBudget(factoryconfig.ExecutionPlan{Pipeline: "bad", MemoryMB: 1024, CPUMillis: 500, Services: []string{"postgres"}}); err == nil {
		t.Fatal("pipeline whose Service consumes its budget was accepted")
	}
}

func TestLinuxJobCgroupManagerFailsClosedWithoutDelegatedCgroupV2(t *testing.T) {
	root := t.TempDir()
	manager := LinuxJobCgroupManager{Root: root}
	if _, err := manager.Open(context.Background(), 1024, 1000); err == nil {
		t.Fatal("regular filesystem was accepted as a cgroup v2 hierarchy")
	}
	for _, limits := range [][2]int{{255, 1000}, {16385, 1000}, {1024, 249}, {1024, 8001}} {
		if _, err := manager.Open(context.Background(), limits[0], limits[1]); err == nil {
			t.Fatalf("out-of-range cgroup limits were accepted: %v", limits)
		}
	}
	if _, err := manager.Open(nil, 1024, 1000); err == nil {
		t.Fatal("nil context was accepted")
	}
	if _, err := (LinuxJobCgroupManager{Root: filepath.Join(root, "..", "bad")}).Open(context.Background(), 1024, 1000); err == nil {
		t.Fatal("non-canonical cgroup root was accepted")
	}
}

func TestCgroupPopulatedRequiresKernelEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cgroup.events")
	if err := os.WriteFile(path, []byte("populated 1\nfrozen 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if populated, err := cgroupPopulated(path); err != nil || !populated {
		t.Fatalf("cgroupPopulated() = %v, %v; want true", populated, err)
	}
	if err := os.WriteFile(path, []byte("populated 0\nfrozen 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if populated, err := cgroupPopulated(path); err != nil || populated {
		t.Fatalf("cgroupPopulated() = %v, %v; want false", populated, err)
	}
	if err := os.WriteFile(path, []byte("frozen 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := cgroupPopulated(path); err == nil {
		t.Fatal("missing populated event was accepted")
	}
}

func TestLinuxJobCgroupManagerRejectsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (LinuxJobCgroupManager{Root: "/sys/fs/cgroup"}).Open(ctx, 1024, 1000); err == nil {
		t.Fatal("canceled cgroup operation was accepted")
	}
}

func TestJobResourceScopeFailsClosedWithoutConfiguredLimiter(t *testing.T) {
	factory := GoTestProcessFactory{}
	if _, err := factory.openResourceScope(context.Background(), 2048, 1000); !errors.Is(err, ErrJobResourceLimitsUnavailable) {
		t.Fatalf("missing Job resource limiter error = %v", err)
	}
	if _, err := factory.openResourceScope(context.Background(), 0, 0); !errors.Is(err, ErrJobResourceLimitsUnavailable) {
		t.Fatalf("missing resource limits error = %v", err)
	}
}

type testJobProcessScope struct{ fd int }

func (s testJobProcessScope) FileDescriptor() int       { return s.fd }
func (testJobProcessScope) Close(context.Context) error { return nil }

func TestAttachJobProcessScopeUsesAtomicCgroupPlacement(t *testing.T) {
	attributes := &syscall.SysProcAttr{Setpgid: true}
	if err := attachJobProcessScope(attributes, testJobProcessScope{fd: 17}); err != nil {
		t.Fatal(err)
	}
	if !attributes.UseCgroupFD || attributes.CgroupFD != 17 || !attributes.Setpgid {
		t.Fatalf("process attributes did not preserve process group and cgroup placement: %+v", attributes)
	}
	if err := attachJobProcessScope(nil, testJobProcessScope{fd: 17}); err == nil {
		t.Fatal("nil SysProcAttr was accepted")
	}
	if err := attachJobProcessScope(&syscall.SysProcAttr{}, testJobProcessScope{fd: -1}); err == nil {
		t.Fatal("invalid cgroup file descriptor was accepted")
	}
}
