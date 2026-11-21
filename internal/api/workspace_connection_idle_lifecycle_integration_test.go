//go:build integration

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	fakeruntime "github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspacesession"
	"golang.org/x/net/websocket"
)

type idleLifecycleSessionLookup struct{}

func (idleLifecycleSessionLookup) IsActive(context.Context, workspacesession.Claims) (bool, error) {
	return true, nil
}

type idleLifecycleDialer struct {
	reporter workspaceagent.WorkspaceConnectionLeaseReporter
}

func (d idleLifecycleDialer) OpenShell(ctx context.Context, claims workspacesession.Claims) (io.ReadWriteCloser, error) {
	lease, err := d.reporter.Open(ctx, workspaceagent.WorkspaceConnectionLeaseIdentity{
		SessionID: claims.TicketID, TenantID: claims.TenantID, WorkspaceID: claims.WorkspaceID, ResourcePlaneID: claims.ResourcePlane,
		MicroVMID: "vm_idle_lifecycle", AllocationGeneration: 1,
	})
	if err != nil {
		return nil, err
	}
	gatewayEnd, runtimeEnd := net.Pipe()
	go func() {
		_, _ = io.Copy(io.Discard, runtimeEnd)
		_ = runtimeEnd.Close()
	}()
	return &idleLifecycleLeasedPipe{ReadWriteCloser: gatewayEnd, lease: lease}, nil
}

func (d idleLifecycleDialer) OpenExec(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("exec is not part of this test")
}
func (d idleLifecycleDialer) OpenFileTransfer(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("file transfer is not part of this test")
}
func (d idleLifecycleDialer) OpenVSCode(context.Context, workspacesession.Claims) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("VS Code is not part of this test")
}
func (d idleLifecycleDialer) OpenPort(context.Context, workspacesession.Claims, workspacesession.Permission, uint16) (io.ReadWriteCloser, error) {
	return nil, fmt.Errorf("port forwarding is not part of this test")
}

type idleLifecycleLeasedPipe struct {
	io.ReadWriteCloser
	lease workspaceagent.WorkspaceConnectionLeaseHandle
}

func (p *idleLifecycleLeasedPipe) Close() error {
	err := p.ReadWriteCloser.Close()
	if releaseErr := p.lease.Close(); err == nil {
		err = releaseErr
	}
	return err
}

func TestWorkspaceGatewayDisconnectReconnectFencesIdleSuspendEndToEnd(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_idle_e2e', 'idle e2e', $1)`, []any{now}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('user_idle_e2e', 'idle-e2e', '', $1)`, []any{now}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp_idle_e2e', 'aws', 'ap-northeast-1', '[]')`, nil},
		{`INSERT INTO workspaces (id, tenant_id, owner_id, name, runtime_class, image, created_at, updated_at)
			VALUES ('ws_idle_e2e', 'tenant_idle_e2e', 'user_idle_e2e', 'idle-e2e', 'standard', 'image:test', $1, $1)`, []any{now}},
		{`INSERT INTO placements (workspace_id, resource_plane_id, placed_at) VALUES ('ws_idle_e2e', 'rp_idle_e2e', $1)`, []any{now}},
		{`INSERT INTO workspace_status (workspace_id, desired_state, observed_state, updated_at)
			VALUES ('ws_idle_e2e', 'running', 'running', $1)`, []any{now}},
		{`INSERT INTO resource_bindings (workspace_id, resource_plane_id, kind, external_id, created_at, allocation_generation)
			VALUES ('ws_idle_e2e', 'rp_idle_e2e', 'lambda_microvm', 'vm_idle_lifecycle', $1, 1)`, []any{now}},
		{`INSERT INTO tenant_workspace_idle_policies (tenant_id, enabled, grace_period_seconds, revision, updated_at)
			VALUES ('tenant_idle_e2e', TRUE, 300, 1, $1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed idle lifecycle: %v\nquery: %s", err, statement.query)
		}
	}

	key := []byte("workspace-idle-lifecycle-e2e-hmac-key-000000000000")
	resolveKey := func(_ context.Context, id domain.ResourcePlaneID) ([]byte, error) {
		if id != "rp_idle_e2e" {
			return nil, fmt.Errorf("unknown Resource Plane")
		}
		return key, nil
	}
	api := echo.New()
	api.PUT("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id",
		workspaceConnectionLeaseHandler(pool, resolveKey, false))
	api.POST("/v1/internal/resource-planes/:resource_plane_id/workspace-connection-leases/:lease_id/release",
		workspaceConnectionLeaseHandler(pool, resolveKey, true))
	apiServer := httptest.NewTLSServer(api)
	defer apiServer.Close()
	reporter, err := workspaceagent.NewSignedWorkspaceConnectionLeaseReporter(apiServer.URL, key, &http.Client{
		Timeout:   5 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, // Test server uses an ephemeral certificate.
	})
	if err != nil {
		t.Fatalf("create lease reporter: %v", err)
	}

	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	issuer := &workspacesession.Ed25519Issuer{KeyID: "idle-e2e", PrivateKey: privateKey, TTL: time.Minute}
	token, claims, err := issuer.Issue(workspacesession.Claims{UserID: "user_idle_e2e", TenantID: "tenant_idle_e2e",
		WorkspaceID: "ws_idle_e2e", ResourcePlane: "rp_idle_e2e", Permissions: []workspacesession.Permission{workspacesession.PermissionShell}})
	if err != nil {
		t.Fatalf("issue Gateway ticket: %v", err)
	}
	permissionsJSON, err := json.Marshal(claims.Permissions)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO sessions (id, user_id, tenant_id, workspace_id, resource_plane_id, permissions_json, expires_at, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`, claims.TicketID, claims.UserID, claims.TenantID, claims.WorkspaceID,
		claims.ResourcePlane, string(permissionsJSON), time.Unix(claims.ExpiresAt, 0).UTC(), now); err != nil {
		t.Fatalf("persist signed Gateway Session: %v", err)
	}
	gateway, err := workspacegateway.New(workspacegateway.Config{
		ResourcePlaneID: "rp_idle_e2e",
		Verifier: &workspacesession.Verifier{Keys: map[string]ed25519.PublicKey{"idle-e2e": publicKey},
			Audience: workspacesession.AudienceGateway, Lookup: idleLifecycleSessionLookup{}},
		Dialer: idleLifecycleDialer{reporter: reporter}, AllowedOrigins: []string{"https://ide.example.test"},
		MaxConnections: 2, MaxConnectionsPerWorkspace: 1,
	})
	if err != nil {
		t.Fatalf("create Workspace Gateway: %v", err)
	}
	gatewayServer := httptest.NewTLSServer(gateway.Handler())
	defer gatewayServer.Close()

	connect := func() *websocket.Conn {
		t.Helper()
		location := "wss" + strings.TrimPrefix(gatewayServer.URL, "https") + "/v1/workspaces/ws_idle_e2e/shell"
		config, err := websocket.NewConfig(location, "https://ide.example.test")
		if err != nil {
			t.Fatal(err)
		}
		config.Protocol = []string{"hako.workspace.v1"}
		config.Header.Set("Authorization", "Bearer "+token)
		config.TlsConfig = &tls.Config{InsecureSkipVerify: true, MinVersion: tls.VersionTLS12} // Test server uses an ephemeral certificate.
		client, err := websocket.DialConfig(config)
		if err != nil {
			t.Fatalf("connect Workspace WSS: %v", err)
		}
		return client
	}
	waitForLeaseCount := func(want int) {
		t.Helper()
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			var count int
			if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_connection_leases
				WHERE workspace_id='ws_idle_e2e' AND released_at IS NULL AND expires_at > now()`).Scan(&count); err == nil && count == want {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		var count int
		if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_connection_leases
			WHERE workspace_id='ws_idle_e2e' AND released_at IS NULL AND expires_at > now()`).Scan(&count); err != nil {
			t.Fatal(err)
		}
		t.Fatalf("active lease count=%d, want %d", count, want)
	}

	first := connect()
	waitForLeaseCount(1)
	if err := first.Close(); err != nil {
		t.Fatalf("disconnect first WSS stream: %v", err)
	}
	waitForLeaseCount(0)
	if _, err := pool.Exec(ctx, `UPDATE workspace_connection_leases SET created_at = now() - interval '7 minutes',
		renewed_at = now() - interval '6 minutes', released_at = now() - interval '6 minutes'
		WHERE workspace_id='ws_idle_e2e' AND released_at IS NOT NULL`); err != nil {
		t.Fatalf("advance first disconnect beyond grace for reconnect race: %v", err)
	}

	// Reusing the same short-lived Hako ticket is allowed only after the first
	// connection closes; the new Gateway stream must create a fresh DB lease.
	second := connect()
	waitForLeaseCount(1)
	stats, err := workspaceactivity.RunIdleSweep(ctx, pool, time.Now().UTC(), 10)
	if err != nil || stats.Suspended != 0 {
		t.Fatalf("reconnect with an active lease must fence idle suspension: stats=%+v err=%v", stats, err)
	}
	var desired string
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM workspace_status WHERE workspace_id='ws_idle_e2e'`).Scan(&desired); err != nil || desired != "running" {
		t.Fatalf("Workspace state after reconnect = %q, err=%v", desired, err)
	}
	if err := second.Close(); err != nil {
		t.Fatalf("disconnect reconnected WSS stream: %v", err)
	}
	waitForLeaseCount(0)
	stats, err = workspaceactivity.RunIdleSweep(ctx, pool, time.Now().UTC(), 10)
	if err != nil || stats.Suspended != 0 {
		t.Fatalf("recent disconnect must honor grace period: stats=%+v err=%v", stats, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_connection_leases SET created_at = now() - interval '7 minutes',
		renewed_at = now() - interval '301 seconds', released_at = now() - interval '301 seconds'
		WHERE workspace_id='ws_idle_e2e' AND released_at IS NOT NULL`); err != nil {
		t.Fatalf("advance disconnected lease beyond grace: %v", err)
	}
	stats, err = workspaceactivity.RunIdleSweep(ctx, pool, time.Now().UTC(), 10)
	if err != nil || stats.Suspended != 1 {
		t.Fatalf("expired idle grace must create one suspend operation: stats=%+v err=%v", stats, err)
	}
	if err := pool.QueryRow(ctx, `SELECT desired_state FROM workspace_status WHERE workspace_id='ws_idle_e2e'`).Scan(&desired); err != nil || desired != "suspended" {
		t.Fatalf("Workspace state after idle suspend = %q, err=%v", desired, err)
	}
	var operationCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operations WHERE workspace_id='ws_idle_e2e' AND type='suspend' AND status='pending'`).Scan(&operationCount); err != nil || operationCount != 1 {
		t.Fatalf("idle suspend Operation count=%d, err=%v", operationCount, err)
	}
	var eventCount, outboxCount, auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM operation_events e JOIN operations o ON o.id=e.operation_id
		WHERE o.workspace_id='ws_idle_e2e' AND o.type='suspend' AND e.event_type='operation.requested'`).Scan(&eventCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events e JOIN operations o ON o.id=e.aggregate_id
		WHERE o.workspace_id='ws_idle_e2e' AND o.type='suspend' AND e.event_type='operation.requested' AND e.published_at IS NULL`).Scan(&outboxCount); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id='tenant_idle_e2e'
		AND actor_user_id IS NULL AND actor_type='system' AND actor_name=$1 AND action='workspace.suspend'`,
		workspaceactivity.IdleSuspendSystemActor).Scan(&auditCount); err != nil {
		t.Fatal(err)
	}
	if eventCount != 1 || outboxCount != 1 || auditCount != 1 {
		t.Fatalf("idle suspend side effects event=%d outbox=%d system-audit=%d, want exactly one each", eventCount, outboxCount, auditCount)
	}

	// Carry the system-generated Operation through the same transactional
	// Outbox Dispatcher, Resource Controller, and result consumer used for
	// ordinary Workspace actions. The runtime is fake; no provider API is called.
	publisher := &idleLifecyclePublisher{}
	operationDispatcher, err := dispatcher.New(
		dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, publisher,
		map[string]string{"rp_idle_e2e": "memory://rp_idle_e2e"}, dispatcher.Config{BatchSize: 1},
	)
	if err != nil {
		t.Fatalf("create idle Operation Dispatcher: %v", err)
	}
	dispatchStats, err := operationDispatcher.RunOnce(ctx)
	if err != nil || dispatchStats.Claimed != 1 || dispatchStats.Published != 1 || len(publisher.events) != 1 {
		t.Fatalf("dispatch idle suspend Operation: stats=%+v events=%d err=%v", dispatchStats, len(publisher.events), err)
	}
	if publisher.queueURL != "memory://rp_idle_e2e" || publisher.events[0].AggregateType != "operation" || publisher.events[0].AggregateID == "" {
		t.Fatalf("idle suspend was not routed to its Placement Resource Plane queue: queue=%q event=%+v", publisher.queueURL, publisher.events[0])
	}
	var dispatched bool
	if err := pool.QueryRow(ctx, `SELECT published_at IS NOT NULL FROM outbox_events WHERE id=$1`, publisher.events[0].ID).Scan(&dispatched); err != nil || !dispatched {
		t.Fatalf("Dispatcher did not acknowledge published idle command: published=%t err=%v", dispatched, err)
	}
	resultQueue := &fakeOperationResultsQueue{}
	controller, err := resourcecontroller.New("rp_idle_e2e", fakeruntime.New(), resultQueue, nil)
	if err != nil {
		t.Fatalf("create idle Resource Controller: %v", err)
	}
	if err := controller.Handle(ctx, publisher.events[0].Payload); err != nil {
		t.Fatalf("execute idle suspend in fake Resource Plane runtime: %v", err)
	}
	resultConsumer, err := operationresults.New(operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, resultQueue,
		operationresults.Config{BatchSize: 1, WaitTime: time.Second})
	if err != nil {
		t.Fatalf("create idle Operation result consumer: %v", err)
	}
	resultStats, err := resultConsumer.RunOnce(ctx)
	if err != nil || resultStats.Applied != 1 || resultStats.Failed != 0 {
		t.Fatalf("apply fake idle suspend result: stats=%+v err=%v", resultStats, err)
	}
	var operationStatus, observedState string
	if err := pool.QueryRow(ctx, `SELECT o.status,s.observed_state FROM operations o JOIN workspace_status s ON s.workspace_id=o.workspace_id
		WHERE o.workspace_id='ws_idle_e2e' AND o.type='suspend'`).Scan(&operationStatus, &observedState); err != nil || operationStatus != "succeeded" || observedState != "suspended" {
		t.Fatalf("idle suspend result projection status=%q observed=%q err=%v", operationStatus, observedState, err)
	}
}

type idleLifecyclePublisher struct {
	queueURL string
	events   []outbox.Event
}

func (p *idleLifecyclePublisher) Publish(_ context.Context, queueURL string, event outbox.Event) error {
	p.queueURL = queueURL
	p.events = append(p.events, event)
	return nil
}

var _ workspacegateway.StreamDialer = idleLifecycleDialer{}
var _ io.ReadWriteCloser = (*idleLifecycleLeasedPipe)(nil)
