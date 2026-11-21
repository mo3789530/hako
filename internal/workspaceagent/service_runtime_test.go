package workspaceagent

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

type serviceEngineFake struct {
	ensureCalls int
	stopCalls   int
	removeCalls int
	healthy     bool
	err         error
	secrets     map[string]string
}

func (e *serviceEngineFake) Ensure(_ context.Context, _ domain.ServiceID, _ string, template servicecatalog.Template, secrets map[string]string) error {
	e.ensureCalls++
	if e.err != nil {
		return e.err
	}
	if template.Privileged || template.Network != "workspace-private" {
		return errors.New("unsafe template reached engine")
	}
	if !validServiceSecrets(template, secrets) {
		return ErrServiceSecretPolicy
	}
	e.secrets = make(map[string]string, len(secrets))
	for name, value := range secrets {
		e.secrets[name] = value
	}
	return nil
}

func (e *serviceEngineFake) Stop(context.Context, domain.ServiceID) error {
	e.stopCalls++
	return e.err
}

func (e *serviceEngineFake) Remove(context.Context, domain.ServiceID) error {
	e.removeCalls++
	return e.err
}

func (e *serviceEngineFake) Healthy(context.Context, domain.ServiceID, servicecatalog.HealthCheck) (bool, error) {
	return e.healthy, e.err
}

func serviceCommand(t *testing.T, name string, revision int64, desired string) domain.WorkspaceServiceCommand {
	t.Helper()
	template, err := servicecatalog.Resolve(name)
	if err != nil {
		t.Fatal(err)
	}
	config, err := servicecatalog.CanonicalJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	return domain.WorkspaceServiceCommand{SchemaVersion: 1, TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", ServiceID: "svc_1", Revision: revision, DesiredState: desired,
		CatalogVersion: template.Name + "/" + template.Version, Config: config}
}

func TestServiceReconcilerExecutesAndFencesRevisions(t *testing.T) {
	engine := &serviceEngineFake{healthy: true}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	command := serviceCommand(t, "redis", 1, "running")
	observation, err := reconciler.Reconcile(ctx, command)
	if err != nil || observation.Revision != 1 || observation.ObservedState != "healthy" || !observation.Healthy || engine.ensureCalls != 1 {
		t.Fatalf("apply running Service = %+v, calls=%d, err=%v", observation, engine.ensureCalls, err)
	}
	if replay, err := reconciler.Reconcile(ctx, command); err != nil || replay != observation || engine.ensureCalls != 1 {
		t.Fatalf("completed revision replay was not a no-op: got=%+v calls=%d err=%v", replay, engine.ensureCalls, err)
	}
	stopped, err := reconciler.Reconcile(ctx, serviceCommand(t, "redis", 2, "stopped"))
	if err != nil || stopped.Revision != 2 || stopped.ObservedState != "stopped" || engine.stopCalls != 1 {
		t.Fatalf("stop Service = %+v, calls=%d, err=%v", stopped, engine.stopCalls, err)
	}
	stale, err := reconciler.Reconcile(ctx, command)
	if err != nil || stale != stopped || engine.ensureCalls != 1 {
		t.Fatalf("stale start command changed newer stopped state: got=%+v calls=%d err=%v", stale, engine.ensureCalls, err)
	}
	deleted, err := reconciler.Reconcile(ctx, serviceCommand(t, "redis", 3, "deleted"))
	if err != nil || deleted.Revision != 3 || deleted.ObservedState != "deleted" || engine.removeCalls != 1 {
		t.Fatalf("remove Service = %+v, calls=%d, err=%v", deleted, engine.removeCalls, err)
	}
}

func TestServiceReconcilerRejectsIdentityAndSameRevisionConflicts(t *testing.T) {
	engine := &serviceEngineFake{healthy: true}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "redis", 1, "running")
	wrongPlane := command
	wrongPlane.ResourcePlaneID = "rp_foreign"
	if _, err := reconciler.Reconcile(context.Background(), wrongPlane); !errors.Is(err, ErrServiceCommand) {
		t.Fatalf("foreign Resource Plane command error = %v", err)
	}
	if _, err := reconciler.Reconcile(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	conflict := serviceCommand(t, "redis", 1, "stopped")
	if _, err := reconciler.Reconcile(context.Background(), conflict); !errors.Is(err, ErrServiceRevision) {
		t.Fatalf("same revision with different desired state error = %v", err)
	}
	if engine.ensureCalls != 1 || engine.stopCalls != 0 {
		t.Fatalf("invalid commands reached engine: ensure=%d stop=%d", engine.ensureCalls, engine.stopCalls)
	}
}

func TestServiceReconcilerRejectsUnapprovedConfigurationAndGeneratedSecretTemplates(t *testing.T) {
	engine := &serviceEngineFake{healthy: true}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "redis", 1, "running")
	command.Config = []byte(`{"schema_version":1,"name":"redis","version":"1.0.0","image":"attacker.invalid/root:latest","resources":{"memory_bytes":999999999999,"milli_cpu":1},"ports":[],"volumes":[],"healthcheck":{"command":["sh","-c","id"],"interval":"1s","timeout":"1s","retries":1},"network":"host","privileged":true}`)
	if _, err := reconciler.Reconcile(context.Background(), command); !errors.Is(err, ErrServiceCommand) {
		t.Fatalf("arbitrary image/command config error = %v", err)
	}
	postgres := serviceCommand(t, "postgres", 1, "running")
	observation, err := reconciler.Reconcile(context.Background(), postgres)
	if !errors.Is(err, ErrServiceSecretPolicy) || observation.ErrorCode != "secret_provider_unavailable" || engine.ensureCalls != 0 {
		t.Fatalf("PostgreSQL must fail closed until secure generated-secret support exists: observation=%+v calls=%d err=%v", observation, engine.ensureCalls, err)
	}
	stopped, err := reconciler.Reconcile(context.Background(), serviceCommand(t, "postgres", 2, "stopped"))
	if err != nil || stopped.ObservedState != "stopped" || engine.stopCalls != 1 || engine.ensureCalls != 0 {
		t.Fatalf("PostgreSQL stop must remain available without generated-secret support: observation=%+v stop_calls=%d ensure_calls=%d err=%v", stopped, engine.stopCalls, engine.ensureCalls, err)
	}
}

func TestServiceReconcilerAcceptsOnlyCatalogGeneratedSecretValues(t *testing.T) {
	engine := &serviceEngineFake{healthy: true}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "postgres", 1, "running")
	secret := "0123456789012345678901234567890123456789012"
	observation, err := reconciler.ReconcileWithSecrets(context.Background(), command, map[string]string{"POSTGRES_PASSWORD": secret})
	if err != nil || observation.ObservedState != "healthy" || engine.secrets["POSTGRES_PASSWORD"] != secret {
		t.Fatalf("PostgreSQL generated credential was not reconciled: observation=%+v secrets=%v err=%v", observation, engine.secrets, err)
	}
	if _, err := reconciler.ReconcileWithSecrets(context.Background(), serviceCommand(t, "postgres", 2, "running"), map[string]string{"POSTGRES_PASSWORD": "bad\nvalue"}); !errors.Is(err, ErrServiceSecretPolicy) {
		t.Fatalf("unsafe generated credential was accepted: %v", err)
	}
}

func TestServiceReconcilerRetriesFailedRevisionWithoutAllowingOlderCommands(t *testing.T) {
	engine := &serviceEngineFake{healthy: true, err: errors.New("engine unavailable")}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	newer := serviceCommand(t, "redis", 2, "stopped")
	first, err := reconciler.Reconcile(ctx, newer)
	if err == nil || first.ObservedState != "failed" || first.Revision != 2 || first.ObservedRevision != 0 {
		t.Fatalf("engine failure should not advance observed revision: %+v err=%v", first, err)
	}
	older := serviceCommand(t, "redis", 1, "running")
	if got, err := reconciler.Reconcile(ctx, older); err != nil || got != first || engine.ensureCalls != 0 {
		t.Fatalf("older command was not fenced by failed newer revision: got=%+v ensure=%d err=%v", got, engine.ensureCalls, err)
	}
	engine.err = nil
	retried, err := reconciler.Reconcile(ctx, newer)
	if err != nil || retried.Revision != 2 || retried.ObservedRevision != 2 || retried.ObservedState != "stopped" || engine.stopCalls != 2 {
		t.Fatalf("failed revision retry = %+v stop_calls=%d err=%v", retried, engine.stopCalls, err)
	}
}

func TestServiceReconcilerSerializesConcurrentDuplicateRevision(t *testing.T) {
	engine := &serviceEngineFake{healthy: true}
	reconciler, err := NewServiceReconciler("tenant_1", "ws_1", "rp_1", engine)
	if err != nil {
		t.Fatal(err)
	}
	command := serviceCommand(t, "redis", 1, "running")
	const callers = 12
	var group sync.WaitGroup
	errorsSeen := make(chan error, callers)
	for range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			_, err := reconciler.Reconcile(context.Background(), command)
			errorsSeen <- err
		}()
	}
	group.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		if err != nil {
			t.Errorf("concurrent duplicate reconciliation failed: %v", err)
		}
	}
	if engine.ensureCalls != 1 {
		t.Fatalf("concurrent duplicate command executed %d times", engine.ensureCalls)
	}
}
