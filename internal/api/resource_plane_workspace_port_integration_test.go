//go:build integration

package api

import (
	"context"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestResourcePlaneWorkspacePortAuthorizationUsesHealthyCurrentServices(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	template, err := servicecatalog.Resolve("postgres")
	if err != nil {
		t.Fatal(err)
	}
	config, err := servicecatalog.CanonicalJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	ticketID := strings.Repeat("a", 48)
	seeds := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_port_auth','Port Auth', $1)`, []any{now}},
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_port_auth','port-auth-user','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_port_auth','aws','ap-northeast-1','["microvm"]')`, nil},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES ('rp_port_auth_other','aws','us-east-1','["microvm"]')`, nil},
		{`INSERT INTO workspaces(id,tenant_id,owner_id,name,runtime_class,image,created_at,updated_at) VALUES ('ws_port_auth','tenant_port_auth','usr_port_auth','port-auth','standard','hako/test',$1,$1)`, []any{now}},
		{`INSERT INTO workspace_status(workspace_id,desired_state,observed_state,updated_at) VALUES ('ws_port_auth','running','running',$1)`, []any{now}},
		{`INSERT INTO placements(workspace_id,resource_plane_id,placed_at) VALUES ('ws_port_auth','rp_port_auth',$1)`, []any{now}},
		{`INSERT INTO resource_bindings(workspace_id,resource_plane_id,kind,external_id,allocation_generation,created_at) VALUES ('ws_port_auth','rp_port_auth','lambda_microvm','microvm-port-auth',3,$1)`, []any{now}},
		{`INSERT INTO services(id,workspace_id,name,catalog_version,config_json,desired_state,observed_state,desired_revision,observed_revision,created_at,updated_at)
			VALUES ('svc_port_auth','ws_port_auth','postgres','postgres/1.0.0',$1,'running','healthy',1,1,$2,$2)`, []any{string(config), now}},
		{`INSERT INTO workspace_service_runtime_generations(service_id,workspace_id,resource_plane_id,allocation_generation,microvm_id,desired_revision,observed_state,retired,updated_at)
			VALUES ('svc_port_auth','ws_port_auth','rp_port_auth',3,'microvm-port-auth',1,'healthy',FALSE,$1)`, []any{now}},
		{`INSERT INTO sessions(id,user_id,tenant_id,workspace_id,resource_plane_id,permissions_json,expires_at,created_at)
			VALUES ($1,'usr_port_auth','tenant_port_auth','ws_port_auth','rp_port_auth','["port-forward"]',$2,$3)`, []any{ticketID, now.Add(2 * time.Minute), now}},
	}
	for _, seed := range seeds {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed port authorization state: %v", err)
		}
	}
	key := []byte(strings.Repeat("k", 32))
	handler := NewHandler(nil, pool, WorkspaceCreateConfig{ResourcePlaneWorkspaceSessionKeyResolver: func(_ context.Context, planeID domain.ResourcePlaneID) ([]byte, error) {
		if planeID != "rp_port_auth" {
			return nil, errors.New("unknown Resource Plane")
		}
		return key, nil
	}})
	server := httptest.NewTLSServer(handler)
	defer server.Close()
	authorizer, err := workspacesession.NewHTTPWorkspacePortAuthorizer(workspacesession.HTTPWorkspacePortAuthorizerConfig{
		Endpoint: server.URL, ResourcePlaneID: "rp_port_auth", HMACKey: key, Transport: server.Client().Transport,
	})
	if err != nil {
		t.Fatal(err)
	}
	claims := workspacesession.Claims{
		Version: 1, Audience: workspacesession.AudienceGateway, TicketID: ticketID, UserID: "usr_port_auth", TenantID: "tenant_port_auth",
		WorkspaceID: "ws_port_auth", ResourcePlane: "rp_port_auth", Permissions: []workspacesession.Permission{workspacesession.PermissionPortForward},
		IssuedAt: now.Unix(), ExpiresAt: now.Add(2 * time.Minute).Unix(),
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 5432); err != nil || !allowed {
		t.Fatalf("healthy current Postgres port authorization = %v, %v; want true", allowed, err)
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 6379); err != nil || allowed {
		t.Fatalf("undeclared Redis port authorization = %v, %v; want false", allowed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_service_runtime_generations SET retired=TRUE WHERE service_id='svc_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 5432); err != nil || allowed {
		t.Fatalf("retired runtime port authorization = %v, %v; want false", allowed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_service_runtime_generations SET retired=FALSE, observed_state='unhealthy' WHERE service_id='svc_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 5432); err != nil || allowed {
		t.Fatalf("unhealthy Service port authorization = %v, %v; want false", allowed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_service_runtime_generations SET observed_state='healthy' WHERE service_id='svc_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_status SET observed_state='suspended' WHERE workspace_id='ws_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 5432); err != nil || allowed {
		t.Fatalf("suspended Workspace port authorization = %v, %v; want false", allowed, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_status SET observed_state='running' WHERE workspace_id='ws_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE placements SET resource_plane_id='rp_port_auth_other' WHERE workspace_id='ws_port_auth'`); err != nil {
		t.Fatal(err)
	}
	if allowed, err := authorizer.AllowPort(ctx, claims, 5432); err != nil || allowed {
		t.Fatalf("old Resource Plane placement port authorization = %v, %v; want false", allowed, err)
	}
	wrongIdentity := claims
	wrongIdentity.UserID = "usr_someone_else"
	if allowed, err := authorizer.AllowPort(ctx, wrongIdentity, 5432); err != nil || allowed {
		t.Fatalf("mismatched session identity port authorization = %v, %v; want false", allowed, err)
	}
}
