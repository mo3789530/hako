package workspaceruntime

import (
	"context"
	"errors"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

func TestStaticSpecResolverReturnsDefensiveResourcePlaneProfile(t *testing.T) {
	spec := validSpec()
	spec.RunHookPayload = ""
	resolver, err := NewStaticSpecResolver(spec)
	if err != nil {
		t.Fatal(err)
	}
	spec.EgressNetworkConnectors[0] = "caller-mutated"
	command := resourcecontroller.Command{TenantID: domain.TenantID("tenant_1"), WorkspaceID: domain.WorkspaceID("ws_1"),
		ResourcePlaneID: domain.ResourcePlaneID("rp_1"), WorkspaceRevision: 1, OperationID: domain.OperationID("op_1")}
	resolved, err := resolver.Resolve(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.EgressNetworkConnectors[0] != validSpec().EgressNetworkConnectors[0] {
		t.Fatalf("resolver profile was mutated through constructor input: %#v", resolved.EgressNetworkConnectors)
	}
	resolved.EgressNetworkConnectors[0] = "result-mutated"
	resolvedAgain, err := resolver.Resolve(context.Background(), command)
	if err != nil || resolvedAgain.EgressNetworkConnectors[0] != validSpec().EgressNetworkConnectors[0] {
		t.Fatalf("resolver did not return an independent profile: %#v err=%v", resolvedAgain.EgressNetworkConnectors, err)
	}
}

func TestStaticSpecResolverRejectsInvalidProfilesAndUntrustedCommandIdentity(t *testing.T) {
	profile := validSpec()
	profile.RunHookPayload = "{\"workspace_id\":\"caller-controlled\"}"
	if _, err := NewStaticSpecResolver(profile); err == nil {
		t.Fatal("profile accepted a fixed Workspace-specific launch hook")
	}
	profile = validSpec()
	profile.RunHookPayload = ""
	profile.EgressNetworkConnectors = nil
	if _, err := NewStaticSpecResolver(profile); !errors.Is(err, ErrSpecification) {
		t.Fatalf("invalid profile error=%v", err)
	}
	profile = validSpec()
	profile.RunHookPayload = ""
	resolver, err := NewStaticSpecResolver(profile)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.Resolve(context.Background(), resourcecontroller.Command{WorkspaceID: "ws_1"}); !errors.Is(err, ErrSpecification) {
		t.Fatalf("incomplete command error=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	command := resourcecontroller.Command{TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_1", WorkspaceRevision: 1, OperationID: "op_1"}
	if _, err := resolver.Resolve(ctx, command); !errors.Is(err, ErrSpecification) {
		t.Fatalf("cancelled context error=%v", err)
	}
}
