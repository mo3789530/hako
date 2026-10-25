package workspacegateway

import (
	"context"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/previewprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestPreviewPortAuthorizerAllowsOnlyPreviewApplicationPort(t *testing.T) {
	policy := PreviewPortAuthorizer{}
	preview := workspacesession.Claims{WorkspaceID: domain.WorkspaceID("prvws_0123456789abcdef0123456789abcdef"),
		Permissions: []workspacesession.Permission{workspacesession.PermissionPortForward}}
	for _, test := range []struct {
		name        string
		ctx         context.Context
		claims      workspacesession.Claims
		port        uint16
		wantAllowed bool
	}{
		{name: "Preview app port", ctx: context.Background(), claims: preview, port: previewprotocol.ApplicationPort, wantAllowed: true},
		{name: "Preview database port", ctx: context.Background(), claims: preview, port: 5432},
		{name: "ordinary Workspace", ctx: context.Background(), claims: workspacesession.Claims{WorkspaceID: "ws_123", Permissions: preview.Permissions}, port: previewprotocol.ApplicationPort},
		{name: "missing forwarding scope", ctx: context.Background(), claims: workspacesession.Claims{WorkspaceID: preview.WorkspaceID}, port: previewprotocol.ApplicationPort},
		{name: "canceled authorization", ctx: canceledPreviewContext(), claims: preview, port: previewprotocol.ApplicationPort},
	} {
		t.Run(test.name, func(t *testing.T) {
			allowed, err := policy.AllowPort(test.ctx, test.claims, test.port)
			if err != nil || allowed != test.wantAllowed {
				t.Fatalf("AllowPort() = %v, %v; want %v, nil", allowed, err, test.wantAllowed)
			}
		})
	}
}

func TestPortAuthorizerChainFallsThroughButStopsOnError(t *testing.T) {
	claims := workspacesession.Claims{WorkspaceID: "ws_123"}
	policy := &testPortAuthorizer{allowed: true}
	chain := PortAuthorizerChain{PreviewPortAuthorizer{}, policy}
	allowed, err := chain.AllowPort(context.Background(), claims, 5432)
	if err != nil || !allowed || policy.calls != 1 {
		t.Fatalf("ordinary Workspace chain result = %v, %v, calls=%d; want true, nil, one call", allowed, err, policy.calls)
	}
	policy = &testPortAuthorizer{err: context.DeadlineExceeded}
	chain = PortAuthorizerChain{PreviewPortAuthorizer{}, policy, &testPortAuthorizer{allowed: true}}
	allowed, err = chain.AllowPort(context.Background(), claims, 5432)
	if err == nil || allowed || policy.calls != 1 {
		t.Fatalf("authorization error did not fail closed: result=%v error=%v calls=%d", allowed, err, policy.calls)
	}
}

type testPortAuthorizer struct {
	allowed bool
	err     error
	calls   int
}

func (p *testPortAuthorizer) AllowPort(context.Context, workspacesession.Claims, uint16) (bool, error) {
	p.calls++
	return p.allowed, p.err
}

func canceledPreviewContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
