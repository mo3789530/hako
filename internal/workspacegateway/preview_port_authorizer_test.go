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

func canceledPreviewContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
