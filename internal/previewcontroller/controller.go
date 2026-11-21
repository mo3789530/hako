// Package previewcontroller consumes generation-fenced Preview commands in a
// Resource Plane. It has no Control Plane database credentials.
package previewcontroller

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"time"

	"github.com/mo3789530/hako/internal/previewprotocol"
)

var (
	ErrConfiguration         = errors.New("Preview Resource Plane controller configuration is invalid")
	ErrRuntimeUnavailable    = errors.New("Preview runtime provider is unavailable")
	ErrCompensationFailed    = errors.New("Preview runtime compensation failed")
	ErrRuntimeProfileMissing = errors.New("Preview runtime profile is missing")
	ErrMicroVMLaunch         = errors.New("Preview Lambda MicroVM launch failed")
	ErrGatewayRoutePublish   = errors.New("Preview Gateway route publish failed")
	ErrMicroVMTerminate      = errors.New("Preview Lambda MicroVM termination failed")
	ErrGatewayRouteRemove    = errors.New("Preview Gateway route removal failed")
)

var resultPlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

type Binding = previewprotocol.RuntimeBinding

// Runtime operations must be idempotent for (runtime_workspace_id, generation).
// Cleanup may recover an unbound MicroVM only from the Resource Plane's durable
// journal and exact frozen launch request. It must never adopt or delete a
// MicroVM found by scanning. Return nil only after the MicroVM and Gateway
// route are absent. Provision may return a partial Binding together with an
// error.
type Runtime interface {
	Ensure(context.Context, previewprotocol.Command) (Binding, error)
	Cleanup(context.Context, previewprotocol.Command) error
	Compensate(context.Context, previewprotocol.Command, Binding) error
}

type Result = previewprotocol.Result

type ResultSink interface {
	Report(context.Context, Result) error
}

type Controller struct {
	resourcePlaneID string
	runtime         Runtime
	sink            ResultSink
	now             func() time.Time
	timeout         time.Duration
}

func New(resourcePlaneID string, runtime Runtime, sink ResultSink, now func() time.Time, timeout time.Duration) (*Controller, error) {
	if !resultPlaneIDPattern.MatchString(resourcePlaneID) || runtime == nil || sink == nil || timeout < time.Second || timeout > 15*time.Minute {
		return nil, ErrConfiguration
	}
	if now == nil {
		now = time.Now
	}
	return &Controller{resourcePlaneID: resourcePlaneID, runtime: runtime, sink: sink, now: now, timeout: timeout}, nil
}

// Handle verifies both the strict payload and Dispatcher SQS attributes. It
// reports fixed failure classes only; provider error text is never serialized.
func (c *Controller) Handle(ctx context.Context, payload []byte, eventType, aggregateID string) error {
	if c == nil || ctx == nil || ctx.Err() != nil {
		return ErrConfiguration
	}
	command, err := previewprotocol.Decode(payload, eventType, aggregateID)
	if err != nil {
		return err
	}
	if command.ResourcePlaneID != c.resourcePlaneID {
		return errors.New("Preview command is pinned to a different Resource Plane")
	}
	now := c.now().UTC()
	result := resultFor(command, now)
	if !command.LeaseExpiresAt.After(now) {
		result.Status, result.ErrorCode = "failed", "claim_lease_expired"
		return c.sink.Report(ctx, result)
	}
	if command.Action == previewprotocol.ActionProvision && !command.ExpiresAt.After(now) {
		result.Status, result.ErrorCode = "failed", "preview_expired"
		return c.sink.Report(ctx, result)
	}
	deadline := now.Add(c.timeout)
	if command.LeaseExpiresAt.Before(deadline) {
		deadline = command.LeaseExpiresAt
	}
	if command.Action == previewprotocol.ActionProvision && command.ExpiresAt.Before(deadline) {
		deadline = command.ExpiresAt
	}
	workCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	switch command.Action {
	case previewprotocol.ActionProvision:
		binding, provisionErr := c.runtime.Ensure(workCtx, command)
		if provisionErr != nil {
			compensateCtx, compensateCancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
			compensateErr := c.runtime.Compensate(compensateCtx, command, binding)
			compensateCancel()
			if compensateErr != nil {
				return ErrCompensationFailed
			}
			result.Status, result.ErrorCode = "failed", provisionErrorCode(provisionErr)
		} else {
			if !validBinding(binding) {
				compensateCtx, compensateCancel := context.WithTimeout(context.WithoutCancel(ctx), c.timeout)
				compensateErr := c.runtime.Compensate(compensateCtx, command, binding)
				compensateCancel()
				if compensateErr != nil {
					return ErrCompensationFailed
				}
				result.Status, result.ErrorCode = "failed", "invalid_runtime_binding"
			} else {
				result.Status, result.Binding = "succeeded", &binding
			}
		}
	case previewprotocol.ActionCleanup:
		if err := c.runtime.Cleanup(workCtx, command); err != nil {
			result.Status, result.ErrorCode = "failed", cleanupErrorCode(err)
		} else {
			result.Status = "succeeded"
		}
	default:
		return errors.New("unsupported Preview command action")
	}
	result.CompletedAt = c.now().UTC()
	if err := c.sink.Report(ctx, result); err != nil {
		return fmt.Errorf("report Preview runtime result: %w", err)
	}
	return nil
}

func resultFor(command previewprotocol.Command, now time.Time) Result {
	return Result{
		SchemaVersion: command.SchemaVersion, Action: command.Action, TenantID: command.TenantID,
		InstallationID: command.InstallationID, RepositoryID: command.RepositoryID,
		PullRequestNumber: command.PullRequestNumber, Generation: command.Generation,
		RuntimeWorkspaceID: command.RuntimeWorkspaceID, ResourcePlaneID: command.ResourcePlaneID,
		ClaimID: command.ClaimID, ClaimAttempt: command.ClaimAttempt, LeaseExpiresAt: command.LeaseExpiresAt,
		CompletedAt: now,
	}
}

func validBinding(binding Binding) bool {
	return safeProviderID(binding.MicroVMID) && safeProviderID(binding.GatewayRouteID)
}

func validResult(result Result) bool {
	return result.Validate() == nil
}

func safeProviderID(value string) bool {
	if len(value) < 1 || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '-' || char == '_' || char == '.' || char == ':') {
			return false
		}
	}
	return true
}

func provisionErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrRuntimeProfileMissing):
		return "runtime_profile_missing"
	case errors.Is(err, ErrMicroVMLaunch):
		return "microvm_launch_failed"
	case errors.Is(err, ErrGatewayRoutePublish):
		return "gateway_route_publish_failed"
	case errors.Is(err, ErrRuntimeUnavailable):
		return "provider_unavailable"
	default:
		return "provider_unavailable"
	}
}

func cleanupErrorCode(err error) string {
	switch {
	case errors.Is(err, ErrGatewayRouteRemove):
		return "gateway_route_remove_failed"
	case errors.Is(err, ErrMicroVMTerminate):
		return "microvm_terminate_failed"
	case errors.Is(err, ErrRuntimeUnavailable):
		return "provider_unavailable"
	default:
		return "provider_unavailable"
	}
}
