package workspaceagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

var (
	ErrServiceCommand      = errors.New("invalid Workspace Service command")
	ErrServiceRevision     = errors.New("Workspace Service revision conflict")
	ErrServiceSecretPolicy = errors.New("Workspace Service template requires an unavailable generated-secret provider")
)

// ServiceEngine is the narrow boundary to the container runtime inside a
// Workspace Lambda MicroVM. Implementations must expose no host/public ports.
type ServiceEngine interface {
	Ensure(context.Context, domain.ServiceID, string, servicecatalog.Template, map[string]string) error
	Stop(context.Context, domain.ServiceID) error
	Remove(context.Context, domain.ServiceID) error
	Healthy(context.Context, domain.ServiceID, servicecatalog.HealthCheck) (bool, error)
}

type ServiceObservation struct {
	ServiceID        domain.ServiceID `json:"service_id"`
	Revision         int64            `json:"revision"`
	ObservedRevision int64            `json:"observed_revision"`
	DesiredState     string           `json:"desired_state"`
	ObservedState    string           `json:"observed_state"`
	Healthy          bool             `json:"healthy"`
	ErrorCode        string           `json:"error_code,omitempty"`
}

type serviceFence struct {
	highestRevision int64
	commandDigest   [32]byte
	observation     ServiceObservation
}

// ServiceReconciler validates a command against the immutable built-in
// catalog and serializes side effects with an in-memory per-Workspace fence.
// Production consumers must additionally verify the signed MicroVM-bound
// capability and report this observation to Control Plane with revision CAS.
type ServiceReconciler struct {
	mu              sync.Mutex
	tenantID        domain.TenantID
	workspaceID     domain.WorkspaceID
	resourcePlaneID domain.ResourcePlaneID
	engine          ServiceEngine
	fences          map[domain.ServiceID]serviceFence
}

func NewServiceReconciler(tenantID domain.TenantID, workspaceID domain.WorkspaceID, resourcePlaneID domain.ResourcePlaneID, engine ServiceEngine) (*ServiceReconciler, error) {
	if tenantID == "" || workspaceID == "" || resourcePlaneID == "" || engine == nil {
		return nil, errors.New("Workspace Service reconciler identity and engine are required")
	}
	return &ServiceReconciler{tenantID: tenantID, workspaceID: workspaceID, resourcePlaneID: resourcePlaneID, engine: engine,
		fences: make(map[domain.ServiceID]serviceFence)}, nil
}

// Reconcile applies one desired Service revision. Replays of completed
// revisions are no-ops; old revisions cannot overwrite newer desired state.
func (r *ServiceReconciler) Reconcile(ctx context.Context, command domain.WorkspaceServiceCommand) (ServiceObservation, error) {
	return r.ReconcileWithSecrets(ctx, command, nil)
}

// ReconcileWithSecrets accepts only variables declared by the immutable
// template. Values are excluded from the revision digest and observations.
func (r *ServiceReconciler) ReconcileWithSecrets(ctx context.Context, command domain.WorkspaceServiceCommand, secrets map[string]string) (ServiceObservation, error) {
	if r == nil || ctx == nil || command.SchemaVersion != 1 || command.TenantID != r.tenantID ||
		command.WorkspaceID != r.workspaceID || command.ResourcePlaneID != r.resourcePlaneID || command.ServiceID == "" ||
		command.Revision < 1 || (command.DesiredState != "running" && command.DesiredState != "stopped" && command.DesiredState != "deleted") ||
		command.CatalogVersion == "" || len(command.Config) == 0 {
		return ServiceObservation{}, ErrServiceCommand
	}
	template, err := validatedServiceTemplate(command)
	if err != nil {
		return ServiceObservation{}, err
	}
	if command.DesiredState != "running" && len(secrets) != 0 {
		return ServiceObservation{}, ErrServiceSecretPolicy
	}
	digest, err := serviceCommandDigest(command)
	if err != nil {
		return ServiceObservation{}, ErrServiceCommand
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	fence := r.fences[command.ServiceID]
	if command.Revision < fence.highestRevision {
		return fence.observation, nil
	}
	if command.Revision == fence.highestRevision {
		if digest != fence.commandDigest {
			return fence.observation, ErrServiceRevision
		}
		if fence.observation.ObservedRevision == command.Revision && fence.observation.ObservedState != "failed" {
			return fence.observation, nil
		}
	} else {
		fence.highestRevision = command.Revision
		fence.commandDigest = digest
	}
	if command.DesiredState == "running" && !validServiceSecrets(template, secrets) {
		observation := ServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision, ObservedRevision: fence.observation.ObservedRevision,
			DesiredState: command.DesiredState, ObservedState: "failed", ErrorCode: "secret_provider_unavailable"}
		fence.observation = observation
		r.fences[command.ServiceID] = fence
		return observation, ErrServiceSecretPolicy
	}
	observation := ServiceObservation{ServiceID: command.ServiceID, Revision: command.Revision, ObservedRevision: fence.observation.ObservedRevision,
		DesiredState: command.DesiredState, ObservedState: "failed", ErrorCode: "runtime_error"}
	switch command.DesiredState {
	case "running":
		if err := r.engine.Ensure(ctx, command.ServiceID, template.Name, template, secrets); err != nil {
			fence.observation = observation
			r.fences[command.ServiceID] = fence
			return observation, err
		}
		healthy, err := r.engine.Healthy(ctx, command.ServiceID, template.HealthCheck)
		if err != nil {
			fence.observation = observation
			r.fences[command.ServiceID] = fence
			return observation, err
		}
		observation.ObservedRevision = command.Revision
		observation.Healthy = healthy
		if healthy {
			observation.ObservedState = "healthy"
			observation.ErrorCode = ""
		} else {
			observation.ObservedState = "unhealthy"
			observation.ErrorCode = ""
		}
	case "stopped":
		if err := r.engine.Stop(ctx, command.ServiceID); err != nil {
			fence.observation = observation
			r.fences[command.ServiceID] = fence
			return observation, err
		}
		observation.ObservedRevision = command.Revision
		observation.ObservedState = "stopped"
		observation.ErrorCode = ""
	case "deleted":
		if err := r.engine.Remove(ctx, command.ServiceID); err != nil {
			fence.observation = observation
			r.fences[command.ServiceID] = fence
			return observation, err
		}
		observation.ObservedRevision = command.Revision
		observation.ObservedState = "deleted"
		observation.ErrorCode = ""
	}
	fence.observation = observation
	r.fences[command.ServiceID] = fence
	return observation, nil
}

func validServiceSecrets(template servicecatalog.Template, secrets map[string]string) bool {
	if len(template.GeneratedSecrets) != len(secrets) {
		return false
	}
	for _, name := range template.GeneratedSecrets {
		value, ok := secrets[name]
		if !ok || len(value) != 43 {
			return false
		}
		for _, char := range value {
			if !(char >= 'A' && char <= 'Z') && !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
				return false
			}
		}
	}
	return true
}

func validatedServiceTemplate(command domain.WorkspaceServiceCommand) (servicecatalog.Template, error) {
	template, err := servicecatalog.Decode(bytes.NewReader(command.Config))
	if err != nil || command.CatalogVersion != template.Name+"/"+template.Version {
		return servicecatalog.Template{}, ErrServiceCommand
	}
	builtin, err := servicecatalog.Resolve(template.Name)
	if err != nil {
		return servicecatalog.Template{}, ErrServiceCommand
	}
	canonical, err := servicecatalog.CanonicalJSON(builtin)
	if err != nil || !bytes.Equal(canonical, command.Config) {
		return servicecatalog.Template{}, fmt.Errorf("%w: catalog configuration is not the approved built-in version", ErrServiceCommand)
	}
	return template, nil
}

func serviceCommandDigest(command domain.WorkspaceServiceCommand) ([32]byte, error) {
	payload, err := json.Marshal(command)
	if err != nil {
		return [32]byte{}, err
	}
	return sha256.Sum256(payload), nil
}
