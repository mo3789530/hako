package workloadagent

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/factoryconfig"
	"github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type fakeJobServiceEngine struct {
	networks, services, removedServices, removedNetworks []string
	secretSeen                                           bool
	healthy                                              bool
	removeFailures                                       int
	activeNetworks                                       map[string]bool
	activeServices                                       map[string]bool
}

func (e *fakeJobServiceEngine) EnsureNetwork(_ context.Context, name, owner string) error {
	if e.activeNetworks == nil {
		e.activeNetworks = make(map[string]bool)
	}
	e.activeNetworks[name] = true
	e.networks = append(e.networks, name+":"+owner)
	return nil
}

func (e *fakeJobServiceEngine) EnsureService(_ context.Context, name, network, owner string, template servicecatalog.Template, secrets map[string]string) error {
	if e.activeServices == nil {
		e.activeServices = make(map[string]bool)
	}
	e.activeServices[name] = true
	e.services = append(e.services, name+":"+network+":"+owner+":"+template.Name)
	if password := secrets["POSTGRES_PASSWORD"]; password != "" {
		e.secretSeen = true
	}
	return nil
}

func (e *fakeJobServiceEngine) Healthy(_ context.Context, _ string, _ servicecatalog.HealthCheck) (bool, error) {
	return e.healthy, nil
}

func (e *fakeJobServiceEngine) RemoveService(_ context.Context, name, _ string) error {
	if !e.activeServices[name] {
		return nil
	}
	e.removedServices = append(e.removedServices, name)
	if e.removeFailures > 0 {
		e.removeFailures--
		return errors.New("synthetic cleanup failure")
	}
	delete(e.activeServices, name)
	return nil
}

func (e *fakeJobServiceEngine) RemoveNetwork(_ context.Context, name, _ string) error {
	if !e.activeNetworks[name] {
		return nil
	}
	e.removedNetworks = append(e.removedNetworks, name)
	delete(e.activeNetworks, name)
	return nil
}

func TestCatalogJobServiceManagerScopesEphemeralServicesAndCredentials(t *testing.T) {
	id := testIdentity()
	engine := &fakeJobServiceEngine{healthy: true}
	manager, err := NewCatalogJobServiceManager(engine, bytes.NewReader(bytes.Repeat([]byte{7}, 64)), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	pipeline := factoryconfig.ExecutionPlan{Pipeline: "integration", RuntimeClass: "standard", TimeoutSecond: 60,
		MemoryMB: 2048, CPUMillis: 1500, Network: "registry-only", Services: []string{"redis", "postgres"},
		Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}}}
	session, err := manager.Start(context.Background(), id, pipeline)
	if err != nil {
		t.Fatal(err)
	}
	env := strings.Join(session.Environment(), "\n")
	secrets := session.SecretValues()
	if len(engine.networks) != 1 || len(engine.services) != 2 || !engine.secretSeen || len(secrets) != 1 ||
		!strings.Contains(env, "PGHOST=127.0.0.1") || !strings.Contains(env, "DATABASE_URL=postgres://postgres:"+secrets[0]+"@127.0.0.1:5432/postgres") ||
		!strings.Contains(env, "REDIS_ADDR=127.0.0.1:6379") {
		t.Fatalf("unexpected provisioned Job Services: networks=%v services=%v env=%q secrets=%d", engine.networks, engine.services, env, len(secrets))
	}
	if strings.Contains(engine.networks[0], id.TenantID) || strings.Contains(engine.services[0], id.WorkloadRunID) {
		t.Fatal("raw tenant or Run identity appeared in a runtime name")
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(engine.removedServices) != 2 || len(engine.removedNetworks) != 1 {
		t.Fatalf("ephemeral resources were not removed: containers=%v networks=%v", engine.removedServices, engine.removedNetworks)
	}
	if err := session.Close(context.Background()); err != nil {
		t.Fatalf("idempotent close failed: %v", err)
	}
	for _, secret := range secrets {
		if strings.Contains(strings.Join(session.Environment(), "\n"), secret) {
			t.Fatal("closed session retained the generated credential in its environment")
		}
	}
}

func TestCatalogJobServiceManagerFailsClosedAndRetriesCleanup(t *testing.T) {
	id := testIdentity()
	pipeline := factoryconfig.ExecutionPlan{Pipeline: "integration", RuntimeClass: "standard", TimeoutSecond: 60,
		MemoryMB: 2048, CPUMillis: 1500, Network: "registry-only", Services: []string{"postgres"},
		Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}}}
	t.Run("health failure cleans up", func(t *testing.T) {
		engine := &fakeJobServiceEngine{healthy: false}
		nowCalls := 0
		manager, _ := NewCatalogJobServiceManager(engine, bytes.NewReader(bytes.Repeat([]byte{1}, 32)), func() time.Time {
			nowCalls++
			return time.Unix(int64(nowCalls*60), 0)
		})
		if _, err := manager.Start(context.Background(), id, pipeline); !errors.Is(err, ErrJobServicesUnavailable) {
			t.Fatalf("unhealthy service accepted: %v", err)
		}
		if len(engine.removedServices) != 1 || len(engine.removedNetworks) != 1 {
			t.Fatalf("failed startup leaked resources: containers=%v networks=%v", engine.removedServices, engine.removedNetworks)
		}
	})
	t.Run("cleanup failure stays visible and retryable", func(t *testing.T) {
		engine := &fakeJobServiceEngine{healthy: true, removeFailures: 1}
		manager, _ := NewCatalogJobServiceManager(engine, bytes.NewReader(bytes.Repeat([]byte{2}, 32)), time.Now)
		session, err := manager.Start(context.Background(), id, pipeline)
		if err != nil {
			t.Fatal(err)
		}
		if err := session.Close(context.Background()); !errors.Is(err, ErrJobServiceCleanup) {
			t.Fatalf("cleanup failure hidden: %v", err)
		}
		if err := session.Close(context.Background()); err != nil {
			t.Fatalf("cleanup retry failed: %v", err)
		}
	})
}

type fixedJobServiceSession struct {
	environment []string
	secrets     []string
	closed      int
	closeErr    error
}

func (s *fixedJobServiceSession) Environment() []string {
	return append([]string(nil), s.environment...)
}
func (s *fixedJobServiceSession) SecretValues() []string { return append([]string(nil), s.secrets...) }
func (s *fixedJobServiceSession) Close(context.Context) error {
	s.closed++
	return s.closeErr
}

type fixedJobServiceProvisioner struct{ session *fixedJobServiceSession }

func (p fixedJobServiceProvisioner) Start(context.Context, workloadjournal.Command, factoryconfig.ExecutionPlan) (JobServiceSession, error) {
	return p.session, nil
}

func TestFactoryPipelineRunsWithJobServicesAndRedactsCredentials(t *testing.T) {
	secret := "synthetic-postgres-password"
	command := filepath.Join(t.TempDir(), "go-test")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' \"$PGPASSWORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	session := &fixedJobServiceSession{environment: []string{"PGPASSWORD=" + secret, "DATABASE_URL=postgres://postgres:" + secret + "@127.0.0.1:5432/postgres"}, secrets: []string{secret}}
	plan := ExecutionPlan{Identity: testIdentity(), RepositoryPath: t.TempDir(), FactoryPipelines: []factoryconfig.ExecutionPlan{{
		Pipeline: "integration", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Services: []string{"postgres"}, Commands: []factoryconfig.Command{{
			Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."},
		}},
	}}}
	output := &boundedOutput{limit: 4096}
	process, err := (GoTestProcessFactory{GoBinary: command, JobServices: fixedJobServiceProvisioner{session: session}}).
		startFactoryPipelines(context.Background(), plan, output)
	if err != nil {
		t.Fatal(err)
	}
	code, err := process.Wait()
	if err != nil || code != 0 || session.closed != 1 {
		t.Fatalf("pipeline did not complete and cleanup: code=%d err=%v closes=%d", code, err, session.closed)
	}
	provider := process.(interface{ SecretValues() []string })
	secrets := provider.SecretValues()
	logs, _ := output.snapshot()
	redacted := fake.RedactJobLogs(logs, secrets)
	if strings.Contains(string(redacted), secret) || !strings.Contains(string(redacted), "[REDACTED]") {
		t.Fatalf("service credential was not redacted from logs: %q", redacted)
	}
}

func TestFactoryPipelineCleanupFailureFailsTheRun(t *testing.T) {
	session := &fixedJobServiceSession{closeErr: ErrJobServiceCleanup}
	plan := ExecutionPlan{Identity: testIdentity(), RepositoryPath: t.TempDir(), FactoryPipelines: []factoryconfig.ExecutionPlan{{
		Pipeline: "integration", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048, CPUMillis: 1000,
		Network: "registry-only", Services: []string{"redis"}, Commands: []factoryconfig.Command{{
			Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."},
		}},
	}}}
	process, err := (GoTestProcessFactory{GoBinary: "/bin/true", JobServices: fixedJobServiceProvisioner{session: session}}).
		startFactoryPipelines(context.Background(), plan, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	code, err := process.Wait()
	results := process.(interface {
		PipelineResults() []workloadjobs.WorkloadPipelineResult
	}).PipelineResults()
	if code != 1 || !errors.Is(err, ErrJobServiceCleanup) || len(results) != 1 || results[0].ErrorCode != "service_cleanup_incomplete" {
		t.Fatalf("cleanup failure did not fail closed: code=%d err=%v results=%+v", code, err, results)
	}
}

type factoryPipelineAdapter struct{ factory GoTestProcessFactory }

func (a factoryPipelineAdapter) Start(ctx context.Context, plan ExecutionPlan, output io.Writer) (ManagedProcess, error) {
	return a.factory.startFactoryPipelines(ctx, plan, output)
}

func TestProcessBackendRedactsJobServicePasswordFromPersistedLogs(t *testing.T) {
	secret := "synthetic-pg-password-from-job-service"
	command := filepath.Join(t.TempDir(), "go-test")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf '%s\\n' \"$PGPASSWORD\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	id := testIdentity()
	plan := ExecutionPlan{Identity: id, Kind: domain.WorkloadJob, RepositoryPath: t.TempDir(),
		SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Minute),
		FactoryPipelines: []factoryconfig.ExecutionPlan{{
			Pipeline: "integration", RuntimeClass: "standard", TimeoutSecond: 30, MemoryMB: 2048,
			CPUMillis: 1000, Network: "registry-only", Services: []string{"postgres"},
			Commands: []factoryconfig.Command{{Step: "go-test", Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}},
		}}}
	session := &fixedJobServiceSession{environment: []string{"PGPASSWORD=" + secret}, secrets: []string{secret}}
	backend, err := NewProcessBackend(ProcessConfig{Resolver: fixedExecutionResolver{plan: plan}, Factory: factoryPipelineAdapter{
		factory: GoTestProcessFactory{GoBinary: command, JobServices: fixedJobServiceProvisioner{session: session}},
	}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer backend.Close(context.Background())
	claims := planTicketClaims("microvm_1", id, OperationStart, plan)
	request := StartRequest{Identity: IdentityFromCommand(id), SourceDigest: plan.SourceDigest, CommitSHA: plan.CommitSHA,
		Deadline: plan.Deadline, Kind: domain.WorkloadJob}
	if err := backend.Start(context.Background(), claims, request); err != nil {
		t.Fatal(err)
	}
	waitForAgentTerminal(t, backend, id)
	logs, err := backend.Logs(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationLogs})
	if err != nil || strings.Contains(string(logs), secret) || !strings.Contains(string(logs), "[REDACTED]") {
		t.Fatalf("persisted Job logs contain a service credential: logs=%q err=%v", logs, err)
	}
	result, err := backend.Result(context.Background(), TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationResult})
	if err != nil || result.Status != "succeeded" || len(result.PipelineResults) != 1 || result.PipelineResults[0].Status != "succeeded" {
		t.Fatalf("unexpected Job result after redaction: %+v err=%v", result, err)
	}
}
