//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	fakeruntime "github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestAuthenticatedCLIWorkspaceLifecycleThroughFakeRuntime(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	seedCLITenant(t, ctx, pool)

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	accessToken := ""
	issuer := ""
	oauthServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/pool/.well-known/jwks.json":
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
				"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "hako-e2e-key",
				"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
				"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
			}}})
		case "/oauth2/token":
			if err := r.ParseForm(); err != nil || r.Form.Get("grant_type") != "authorization_code" || r.Form.Get("client_id") != "hako-cli" || r.Form.Get("code") != "e2e-code" || len(r.Form.Get("code_verifier")) < 43 {
				http.Error(w, "invalid token request", http.StatusBadRequest)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": accessToken, "token_type": "Bearer", "expires_in": 3600})
		default:
			http.NotFound(w, r)
		}
	}))
	defer oauthServer.Close()
	issuer = oauthServer.URL + "/pool"
	accessToken = signedAccessToken(t, privateKey, issuer)
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatalf("create Cognito verifier: %v", err)
	}
	apiServer := httptest.NewServer(api.NewHandler(verifier, pool, api.WorkspaceCreateConfig{
		RequiredCapabilities: []string{"microvm"}, RuntimeClass: "standard", Image: "hako/e2e:test",
	}))
	defer apiServer.Close()
	t.Setenv("HAKO_API_URL", apiServer.URL)

	credentials := &e2eCredentialStore{}
	cliCredentialStore = credentials
	cliCredentialGetter = credentials
	cliBrowserOpener = func(authorizeURL string) error {
		authorized, err := url.Parse(authorizeURL)
		if err != nil {
			return err
		}
		callback, err := url.Parse(authorized.Query().Get("redirect_uri"))
		if err != nil {
			return err
		}
		query := callback.Query()
		query.Set("code", "e2e-code")
		query.Set("state", authorized.Query().Get("state"))
		callback.RawQuery = query.Encode()
		response, err := http.Get(callback.String())
		if err != nil {
			return err
		}
		defer response.Body.Close()
		if response.StatusCode != http.StatusOK {
			return fmt.Errorf("OAuth callback returned HTTP %d", response.StatusCode)
		}
		return nil
	}
	t.Cleanup(func() {
		cliCredentialStore = auth.OSKeyring{}
		cliCredentialGetter = auth.OSKeyring{}
		cliBrowserOpener = openBrowser
	})
	callbackURL := reserveCLICallbackURL(t)
	if err := runLogin(auth.Config{Domain: oauthServer.URL, ClientID: "hako-cli", CallbackURL: callbackURL, Timeout: 5 * time.Second}, cliCredentialStore, cliBrowserOpener, nil); err != nil {
		t.Fatalf("CLI OAuth login: %v", err)
	}
	if got, err := auth.LoadAccessToken(credentials, time.Now()); err != nil || got != accessToken {
		t.Fatalf("CLI did not persist a usable login: token-match=%t err=%v", got == accessToken, err)
	}
	hidden, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_cli_e2e", OwnerID: "user_cli_other", Name: "private-other-user",
		RuntimeClass: "standard", Image: "hako/e2e:test", ResourcePlaneID: "rp_cli_e2e", IdempotencyKey: "hidden-other-user",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("seed another member Workspace: %v", err)
	}
	if _, err := getWorkspace("tenant_cli_e2e", string(hidden.Workspace.ID)); err == nil {
		t.Fatal("CLI user should not read another Member's Workspace")
	} else {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "not_found" {
			t.Fatalf("hidden Workspace should look like not found, got %v", err)
		}
	}
	if _, err := workspaceAction("suspend", "tenant_cli_e2e", string(hidden.Workspace.ID), "deny-hidden-workspace"); err == nil {
		t.Fatal("CLI user should not act on another Member's Workspace")
	} else {
		var apiErr *api.APIError
		if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusNotFound || apiErr.Code != "not_found" {
			t.Fatalf("hidden Workspace action should look like not found, got %v", err)
		}
	}

	created, err := createWorkspace("tenant_cli_e2e", "lifecycle-e2e", "cli-create-e2e")
	if err != nil {
		t.Fatalf("CLI create Workspace: %v", err)
	}
	if created.ObservedState != string(domain.ObservedWorkspacePending) || created.OperationState != string(domain.OperationPending) {
		t.Fatalf("create should initially be asynchronous, state=%s/%s", created.ObservedState, created.OperationState)
	}
	fakeRuntime := fakeruntime.New()
	runCLIOutboxOperation(t, ctx, pool, domain.OperationID(created.OperationID), fakeRuntime, true)
	assertCLIObserved(t, "tenant_cli_e2e", created.ID, domain.ObservedWorkspaceRunning)
	listed, err := listWorkspaces("tenant_cli_e2e", 20, 0)
	if err != nil || len(listed.Items) != 1 || listed.Items[0].Workspace.ID != created.ID {
		t.Fatalf("CLI list Workspace: items=%+v err=%v", listed.Items, err)
	}

	suspended, err := workspaceAction("suspend", "tenant_cli_e2e", created.ID, "cli-suspend-e2e")
	if err != nil {
		t.Fatalf("CLI suspend Workspace: %v", err)
	}
	if suspended.Workspace.Status.DesiredState != string(domain.DesiredWorkspaceSuspended) || suspended.Workspace.Status.ObservedState != string(domain.ObservedWorkspaceRunning) {
		t.Fatalf("suspend should be accepted before observed state changes: %+v", suspended.Workspace.Status)
	}
	runCLIOutboxOperation(t, ctx, pool, domain.OperationID(suspended.OperationID), fakeRuntime, false)
	assertCLIObserved(t, "tenant_cli_e2e", created.ID, domain.ObservedWorkspaceSuspended)

	resumed, err := workspaceAction("resume", "tenant_cli_e2e", created.ID, "cli-resume-e2e")
	if err != nil {
		t.Fatalf("CLI resume Workspace: %v", err)
	}
	if resumed.Workspace.Status.DesiredState != string(domain.DesiredWorkspaceRunning) || resumed.Workspace.Status.ObservedState != string(domain.ObservedWorkspaceSuspended) {
		t.Fatalf("resume should be accepted before observed state changes: %+v", resumed.Workspace.Status)
	}
	runCLIOutboxOperation(t, ctx, pool, domain.OperationID(resumed.OperationID), fakeRuntime, false)
	assertCLIObserved(t, "tenant_cli_e2e", created.ID, domain.ObservedWorkspaceRunning)

	deleted, err := workspaceAction("delete", "tenant_cli_e2e", created.ID, "cli-delete-e2e")
	if err != nil {
		t.Fatalf("CLI delete Workspace: %v", err)
	}
	if deleted.Workspace.Status.DesiredState != string(domain.DesiredWorkspaceDeleted) || deleted.Workspace.Status.ObservedState != string(domain.ObservedWorkspaceRunning) {
		t.Fatalf("delete should be accepted before observed state changes: %+v", deleted.Workspace.Status)
	}
	runCLIOutboxOperation(t, ctx, pool, domain.OperationID(deleted.OperationID), fakeRuntime, false)
	final, err := getWorkspace("tenant_cli_e2e", created.ID)
	if err != nil || final.Status.DesiredState != string(domain.DesiredWorkspaceDeleted) || final.Status.ObservedState != string(domain.ObservedWorkspaceDeleted) {
		t.Fatalf("CLI final Workspace state: result=%+v err=%v", final, err)
	}
	var quotaSlots int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM workspace_quota_slots WHERE workspace_id = $1", created.ID).Scan(&quotaSlots); err != nil || quotaSlots != 0 {
		t.Fatalf("CLI delete should release quota after observed deletion: slots=%d err=%v", quotaSlots, err)
	}
}

func seedCLITenant(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	now := time.Now().UTC()
	statements := []struct {
		query string
		args  []any
	}{
		{"INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)", []any{"tenant_cli_e2e", "CLI E2E", now}},
		{"INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)", []any{"user_cli_e2e", "cli-e2e-subject", "", now}},
		{"INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)", []any{"user_cli_other", "cli-other-subject", "", now}},
		{"INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)", []any{"tenant_cli_e2e", "user_cli_e2e", "member", now}},
		{"INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)", []any{"tenant_cli_e2e", "user_cli_other", "member", now}},
		{"INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ($1, $2, $3, $4)", []any{"rp_cli_e2e", "aws", "local", `["microvm"]`}},
		{"INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ($1, $2, $3)", []any{"rp_cli_e2e", "active", now}},
	}
	for _, statement := range statements {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed CLI E2E: %v", err)
		}
	}
}

func signedAccessToken(t *testing.T, key *rsa.PrivateKey, issuer string) string {
	t.Helper()
	claims := auth.CognitoAccessClaims{
		ClientID: "hako-cli", TokenUse: "access", Scope: "openid email profile hako/api",
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: "cli-e2e-subject",
			IssuedAt: jwt.NewNumericDate(time.Now()), ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "hako-e2e-key"
	encoded, err := token.SignedString(key)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func reserveCLICallbackURL(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "http://127.0.0.1:" + strconv.Itoa(port) + "/callback"
}

func runCLIOutboxOperation(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id domain.OperationID, runtime *fakeruntime.Runtime, duplicateAndRetry bool) {
	t.Helper()
	var command []byte
	if err := pool.QueryRow(ctx, "SELECT payload_json FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'operation.requested'", id).Scan(&command); err != nil {
		t.Fatalf("load CLI Outbox command: %v", err)
	}
	resultsQueue := &cliResultsQueue{}
	controller, err := resourcecontroller.New("rp_cli_e2e", runtime, resultsQueue, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := controller.Handle(ctx, command); err != nil {
		t.Fatalf("run CLI Operation in Fake Runtime: %v", err)
	}
	batchSize := 1
	if duplicateAndRetry {
		if err := controller.Handle(ctx, command); err != nil {
			t.Fatalf("redeliver duplicate CLI Operation: %v", err)
		}
		if runtime.ProcessedOperationCount() != 1 {
			t.Fatalf("duplicate command should execute Fake Runtime once, processed=%d", runtime.ProcessedOperationCount())
		}
		resultsQueue.failDeletes = 1
		batchSize = 2
	}
	consumer, err := operationresults.New(operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, resultsQueue, operationresults.Config{BatchSize: batchSize, WaitTime: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	stats, err := consumer.RunOnce(ctx)
	if duplicateAndRetry {
		if err == nil || stats.Failed != 1 || stats.Duplicate != 1 {
			t.Fatalf("failed result ack should preserve a retry while duplicate is ignored: stats=%+v err=%v", stats, err)
		}
		retryStats, retryErr := consumer.RunOnce(ctx)
		if retryErr != nil || retryStats.Duplicate != 1 {
			t.Fatalf("redelivered committed result should be a duplicate no-op: stats=%+v err=%v", retryStats, retryErr)
		}
		var terminalEvents int
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM operation_events WHERE operation_id = $1 AND event_type = 'operation.succeeded'", id).Scan(&terminalEvents); err != nil || terminalEvents != 1 {
			t.Fatalf("duplicate result should append one terminal event, count=%d err=%v", terminalEvents, err)
		}
		return
	}
	if err != nil || stats.Applied != 1 {
		t.Fatalf("consume CLI Operation result: stats=%+v err=%v", stats, err)
	}
}

func assertCLIObserved(t *testing.T, tenantID, workspaceID string, want domain.ObservedWorkspaceState) {
	t.Helper()
	workspace, err := getWorkspace(tenantID, workspaceID)
	if err != nil || workspace.Status.ObservedState != string(want) {
		t.Fatalf("CLI get observed state: got=%+v err=%v want=%s", workspace, err, want)
	}
}

type e2eCredentialStore struct{ secret string }

func (s *e2eCredentialStore) Set(_, _, secret string) error { s.secret = secret; return nil }
func (s *e2eCredentialStore) Get(_, _ string) (string, error) {
	if s.secret == "" {
		return "", errors.New("credential missing")
	}
	return s.secret, nil
}

type cliResultsQueue struct {
	deliveries  []operationresults.Delivery
	next        int
	failDeletes int
}

func (q *cliResultsQueue) Report(_ context.Context, result resourcecontroller.Result) error {
	payload, err := json.Marshal(result)
	if err != nil {
		return err
	}
	q.next++
	q.deliveries = append(q.deliveries, operationresults.Delivery{Body: payload, ReceiptHandle: strconv.Itoa(q.next)})
	return nil
}
func (q *cliResultsQueue) Receive(context.Context, int, time.Duration) ([]operationresults.Delivery, error) {
	return append([]operationresults.Delivery(nil), q.deliveries...), nil
}
func (q *cliResultsQueue) Delete(_ context.Context, receipt string) error {
	if q.failDeletes > 0 {
		q.failDeletes--
		return errors.New("simulated result acknowledgement failure")
	}
	for i := range q.deliveries {
		if q.deliveries[i].ReceiptHandle == receipt {
			q.deliveries = append(q.deliveries[:i], q.deliveries[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("receipt %s not found", receipt)
}
