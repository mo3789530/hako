//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	fakeruntime "github.com/mo3789530/hako/internal/runtime/fake"
	"github.com/mo3789530/hako/internal/store/dsql"
	storeservices "github.com/mo3789530/hako/internal/store/services"
	"github.com/mo3789530/hako/internal/store/transaction"
	storeworkloads "github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/store/workspaceactivity"
	"github.com/mo3789530/hako/internal/store/workspaces"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantMembershipRouteResolvesUserAndRejectsNonMembers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply embedded migrations: %v", err)
	}
	joinedAt := time.Now().UTC()
	seed := []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_member", "Member Tenant", joinedAt}},
		{`INSERT INTO tenants (id, name, created_at) VALUES ($1, $2, $3)`, []any{"tenant_other", "Other Tenant", joinedAt}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)`, []any{"usr_member", "member-subject", "", joinedAt}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)`, []any{"usr_owner", "owner-subject", "", joinedAt}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)`, []any{"usr_admin", "admin-subject", "", joinedAt}},
		{`INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, $3, $4)`, []any{"usr_other", "other-subject", "", joinedAt}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_member", "usr_member", "member", joinedAt}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_member", "usr_owner", "owner", joinedAt}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_member", "usr_admin", "admin", joinedAt}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_member", "usr_other", "member", joinedAt}},
		{`INSERT INTO tenant_members (tenant_id, user_id, role, joined_at) VALUES ($1, $2, $3, $4)`, []any{"tenant_other", "usr_other", "owner", joinedAt}},
		{`INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ($1, $2, $3, $4)`, []any{"rp_test", "aws", "ap-northeast-1", `["microvm"]`}},
		{`INSERT INTO resource_plane_status (resource_plane_id, status, updated_at) VALUES ($1, $2, $3)`, []any{"rp_test", "active", joinedAt}},
	}
	for _, statement := range seed {
		if _, err := pool.Exec(ctx, statement.query, statement.args...); err != nil {
			t.Fatalf("seed integration data: %v", err)
		}
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatalf("create Cognito verifier: %v", err)
	}
	serviceReportKey := []byte("workspace-service-test-report-key-0123456789")
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{
		RequiredCapabilities: []string{"microvm"}, RuntimeClass: "standard", Image: "hako/go:test",
		ResourcePlaneServiceReportKeyResolver: func(_ context.Context, id domain.ResourcePlaneID) ([]byte, error) {
			if id != "rp_test" {
				return nil, fmt.Errorf("unknown Resource Plane")
			}
			return serviceReportKey, nil
		},
	})

	requestInstallation := func(subject, method, tenantID, body string) *httptest.ResponseRecorder {
		token := signTenantAPIToken(t, privateKey, issuer, subject, "openid hako/api")
		request := httptest.NewRequest(method, "/v1/tenants/"+tenantID+"/github/installations", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	installationBody := `{"installation_id":12345,"account_login":"acme"}`
	if response := requestInstallation("member-subject", http.MethodPost, "tenant_member", installationBody); response.Code != http.StatusNotFound {
		t.Fatalf("tenant member requested GitHub installation: HTTP %d %s", response.Code, response.Body.String())
	}
	requested := requestInstallation("owner-subject", http.MethodPost, "tenant_member", installationBody)
	if requested.Code != http.StatusAccepted || !strings.Contains(requested.Body.String(), `"verified":false`) || !strings.Contains(requested.Body.String(), `"status":"pending"`) {
		t.Fatalf("tenant owner installation request = HTTP %d %s", requested.Code, requested.Body.String())
	}
	listed := requestInstallation("owner-subject", http.MethodGet, "tenant_member", "")
	if listed.Code != http.StatusOK || !strings.Contains(listed.Body.String(), `"installation_id":12345`) {
		t.Fatalf("tenant owner installation list = HTTP %d %s", listed.Code, listed.Body.String())
	}
	repositoryListRequest := httptest.NewRequest(http.MethodGet, "/v1/tenants/tenant_member/github/installations/12345/repositories", nil)
	repositoryListRequest.Header.Set("Authorization", "Bearer "+signTenantAPIToken(t, privateKey, issuer, "owner-subject", "openid hako/api"))
	repositoryListResponse := httptest.NewRecorder()
	handler.ServeHTTP(repositoryListResponse, repositoryListRequest)
	if repositoryListResponse.Code != http.StatusNotFound {
		t.Fatalf("pending Installation exposed repositories: HTTP %d %s", repositoryListResponse.Code, repositoryListResponse.Body.String())
	}
	if response := requestInstallation("owner-subject", http.MethodGet, "tenant_other", ""); response.Code != http.StatusNotFound {
		t.Fatalf("tenant owner read another tenant's GitHub installation: HTTP %d", response.Code)
	}
	var activeBindings int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM github_app_installation_bindings WHERE installation_id = $1`, 12345).Scan(&activeBindings); err != nil {
		t.Fatal(err)
	}
	if activeBindings != 0 {
		t.Fatal("pending installation request must not become an active tenant binding")
	}

	response := requestTenantMembership(t, handler, signTenantAPIToken(t, privateKey, issuer, "member-subject", "openid hako/api"), "tenant_member")
	if response.Code != http.StatusOK {
		t.Fatalf("member request returned HTTP %d: %s", response.Code, response.Body.String())
	}
	var membership tenantMembershipResponse
	if err := json.Unmarshal(response.Body.Bytes(), &membership); err != nil {
		t.Fatalf("decode membership response: %v", err)
	}
	if membership.TenantID != "tenant_member" || membership.UserID != "usr_member" || membership.Role != domain.TenantRoleMember {
		t.Fatalf("unexpected membership response: %+v", membership)
	}

	nonMember := requestTenantMembership(t, handler, signTenantAPIToken(t, privateKey, issuer, "non-member-subject", "openid hako/api"), "tenant_member")
	missingTenant := requestTenantMembership(t, handler, signTenantAPIToken(t, privateKey, issuer, "member-subject", "openid hako/api"), "tenant_missing")
	if nonMember.Code != http.StatusNotFound || missingTenant.Code != http.StatusNotFound || nonMember.Body.String() != missingTenant.Body.String() {
		t.Fatalf("membership denial must hide Tenant existence: nonmember=%d %s missing=%d %s", nonMember.Code, nonMember.Body.String(), missingTenant.Code, missingTenant.Body.String())
	}

	denied := requestTenantMembership(t, handler, signTenantAPIToken(t, privateKey, issuer, "scope-denied-subject", "openid email"), "tenant_member")
	if denied.Code != http.StatusForbidden {
		t.Fatalf("token without API scope returned HTTP %d, want 403", denied.Code)
	}
	var userCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM users WHERE cognito_subject = $1`, "scope-denied-subject").Scan(&userCount); err != nil {
		t.Fatalf("verify scope denial did not create a user: %v", err)
	}
	if userCount != 0 {
		t.Fatal("request without hako/api scope must be rejected before Hako User resolution")
	}

	createToken := signTenantAPIToken(t, privateKey, issuer, "member-subject", "openid hako/api")
	if _, err := pool.Exec(ctx, `INSERT INTO workload_runs (id,tenant_id,kind,runtime_class,trust_level,desired_state,state,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		VALUES ('run_usage_active','tenant_member','job','standard','untrusted','running','pending','usage-active','usage-request',600,$1,1,$2,$2)`, joinedAt.Add(10*time.Minute), joinedAt); err != nil {
		t.Fatalf("seed active Workload usage: %v", err)
	}
	created := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"api-dev"}`, "create-api-dev")
	if created.Code != http.StatusAccepted {
		t.Fatalf("workspace create returned HTTP %d: %s", created.Code, created.Body.String())
	}
	var createResult workspaceCreateResponse
	if err := json.Unmarshal(created.Body.Bytes(), &createResult); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if createResult.Workspace.OwnerID != "usr_member" || createResult.Workspace.Name != "api-dev" || createResult.Status.ObservedState != domain.ObservedWorkspacePending {
		t.Fatalf("unexpected created Workspace: %+v", createResult)
	}
	var operationPayload string
	if err := pool.QueryRow(ctx, `SELECT payload_json FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'operation.requested'`, createResult.OperationID).Scan(&operationPayload); err != nil {
		t.Fatalf("load API-created Operation command: %v", err)
	}
	var operationCorrelation struct {
		CorrelationID string `json:"correlation_id"`
	}
	if json.Unmarshal([]byte(operationPayload), &operationCorrelation) != nil || operationCorrelation.CorrelationID == "" ||
		operationCorrelation.CorrelationID != created.Header().Get("X-Hako-Request-ID") {
		t.Fatalf("API-generated request ID was not persisted with Operation command: payload=%s header=%q", operationPayload, created.Header().Get("X-Hako-Request-ID"))
	}
	usageResponse := requestWorkspaceUsage(t, handler, createToken, "tenant_member")
	var usage workspaces.QuotaUsage
	if usageResponse.Code != http.StatusOK || json.Unmarshal(usageResponse.Body.Bytes(), &usage) != nil || usage.WorkspacesUsed != 1 || usage.WorkspacesLimit != int64(workspaces.MaxWorkspacesPerTenant) || usage.WorkloadRunsUsed != 1 || usage.WorkloadRunsLimit != int64(storeworkloads.MaxConcurrentRunsPerTenant) {
		t.Fatalf("Tenant usage should include the newly reserved Workspace slot: HTTP %d body=%s usage=%+v", usageResponse.Code, usageResponse.Body.String(), usage)
	}
	foreignUsage := requestWorkspaceUsage(t, handler, createToken, "tenant_other")
	if foreignUsage.Code != http.StatusNotFound {
		t.Fatalf("non-member read of another Tenant's usage should be hidden: HTTP %d %s", foreignUsage.Code, foreignUsage.Body.String())
	}
	replayed := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"api-dev"}`, "create-api-dev")
	var replayResult workspaceCreateResponse
	if replayed.Code != http.StatusAccepted || json.Unmarshal(replayed.Body.Bytes(), &replayResult) != nil || replayResult.Workspace.ID != createResult.Workspace.ID {
		t.Fatalf("idempotent create did not return original Workspace: %d %s", replayed.Code, replayed.Body.String())
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workspace.create' AND target_id = $3`, "tenant_member", "usr_member", createResult.Workspace.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("Workspace create and idempotent replay should produce one actor audit event, count=%d error=%v", auditCount, err)
	}
	serviceRequest := func(method, body string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, "/v1/tenants/tenant_member/workspaces/"+string(createResult.Workspace.ID)+"/services", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+createToken)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	ownerToken := signTenantAPIToken(t, privateKey, issuer, "owner-subject", "openid hako/api")
	serviceQuotaSet := requestTenantQuota(t, handler, ownerToken, http.MethodPut, "tenant_member",
		`{"workspaces_limit":10,"workload_runs_limit":5,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":1610612736,"service_milli_cpu_limit":1000}`)
	if serviceQuotaSet.Code != http.StatusOK || !strings.Contains(serviceQuotaSet.Body.String(), `"service_memory_bytes_limit":1610612736`) || !strings.Contains(serviceQuotaSet.Body.String(), `"service_milli_cpu_limit":1000`) {
		t.Fatalf("Tenant Owner should configure lower per-Workspace Service resource limits: HTTP %d %s", serviceQuotaSet.Code, serviceQuotaSet.Body.String())
	}
	catalogRequest := httptest.NewRequest(http.MethodGet, "/v1/service-catalog", nil)
	catalogRequest.Header.Set("Authorization", "Bearer "+createToken)
	catalogResponse := httptest.NewRecorder()
	handler.ServeHTTP(catalogResponse, catalogRequest)
	if catalogResponse.Code != http.StatusOK || !strings.Contains(catalogResponse.Body.String(), `"name":"postgres"`) || !strings.Contains(catalogResponse.Body.String(), `"name":"redis"`) {
		t.Fatalf("authenticated Service Catalog request = HTTP %d %s", catalogResponse.Code, catalogResponse.Body.String())
	}
	addedService := serviceRequest(http.MethodPost, `{"catalog_name":"postgres"}`)
	var postgresService domain.Service
	if addedService.Code != http.StatusAccepted || json.Unmarshal(addedService.Body.Bytes(), &postgresService) != nil || postgresService.Name != "postgres" || postgresService.DesiredState != "running" || postgresService.ObservedState != "pending" || postgresService.DesiredRevision != 1 || postgresService.ObservedRevision != 0 {
		t.Fatalf("Workspace Service add = HTTP %d %s", addedService.Code, addedService.Body.String())
	}
	configuredServiceUsageResponse := requestWorkspaceServiceUsage(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID))
	var configuredServiceUsage storeservices.ResourceUsage
	if configuredServiceUsageResponse.Code != http.StatusOK || json.Unmarshal(configuredServiceUsageResponse.Body.Bytes(), &configuredServiceUsage) != nil ||
		configuredServiceUsage.MemoryBytesUsed != 1<<30 || configuredServiceUsage.MemoryBytesLimit != 1610612736 ||
		configuredServiceUsage.MilliCPUUsed != 750 || configuredServiceUsage.MilliCPULimit != 1000 {
		t.Fatalf("Workspace Service usage should report Tenant-specific caps: HTTP %d %s usage=%+v", configuredServiceUsageResponse.Code, configuredServiceUsageResponse.Body.String(), configuredServiceUsage)
	}
	serviceQuotaBelowUsage := requestTenantQuota(t, handler, ownerToken, http.MethodPut, "tenant_member",
		`{"workspaces_limit":10,"workload_runs_limit":5,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":536870912,"service_milli_cpu_limit":500}`)
	if serviceQuotaBelowUsage.Code != http.StatusConflict || !strings.Contains(serviceQuotaBelowUsage.Body.String(), `"quota_below_current_usage"`) {
		t.Fatalf("Tenant Service cap cannot be lowered below a Workspace's current reservation: HTTP %d %s", serviceQuotaBelowUsage.Code, serviceQuotaBelowUsage.Body.String())
	}
	serviceAtConfiguredLimit := serviceRequest(http.MethodPost, `{"catalog_name":"postgres","name":"postgres-over-config-limit"}`)
	if serviceAtConfiguredLimit.Code != http.StatusConflict || !strings.Contains(serviceAtConfiguredLimit.Body.String(), `"service_resource_quota_exceeded"`) {
		t.Fatalf("Tenant-specific Service resource cap must reject an aggregate overage: HTTP %d %s", serviceAtConfiguredLimit.Code, serviceAtConfiguredLimit.Body.String())
	}
	serviceQuotaRestore := requestTenantQuota(t, handler, ownerToken, http.MethodPut, "tenant_member",
		`{"workspaces_limit":10,"workload_runs_limit":5,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":2147483648,"service_milli_cpu_limit":1500}`)
	if serviceQuotaRestore.Code != http.StatusOK {
		t.Fatalf("raising Service cap while current reservations fit should succeed: HTTP %d %s", serviceQuotaRestore.Code, serviceQuotaRestore.Body.String())
	}
	var serviceCommand domain.WorkspaceServiceCommand
	var serviceCommandPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload_json FROM outbox_events WHERE aggregate_type = 'workspace_service' AND aggregate_id = $1 AND event_type = 'workspace_service.reconcile_requested'`, postgresService.ID).Scan(&serviceCommandPayload); err != nil {
		t.Fatalf("Service desired state must atomically create an Outbox command: %v", err)
	}
	if err := json.Unmarshal(serviceCommandPayload, &serviceCommand); err != nil {
		t.Fatalf("decode Service command: %v", err)
	}
	var placementPlaneID domain.ResourcePlaneID
	if err := pool.QueryRow(ctx, `SELECT resource_plane_id FROM placements WHERE workspace_id = $1`, createResult.Workspace.ID).Scan(&placementPlaneID); err != nil {
		t.Fatalf("load test Workspace placement: %v", err)
	}
	const serviceMicroVMID = "microvm-service-report-test"
	if _, err := pool.Exec(ctx, `INSERT INTO resource_bindings (workspace_id, resource_plane_id, kind, external_id, allocation_generation, created_at)
		VALUES ($1, $2, 'lambda_microvm', $3, 1, $4)`, createResult.Workspace.ID, placementPlaneID, serviceMicroVMID, time.Now().UTC()); err != nil {
		t.Fatalf("seed Workspace MicroVM binding: %v", err)
	}
	if serviceCommand.SchemaVersion != 1 || serviceCommand.TenantID != "tenant_member" || serviceCommand.WorkspaceID != createResult.Workspace.ID ||
		serviceCommand.ResourcePlaneID != placementPlaneID || serviceCommand.ServiceID != postgresService.ID || serviceCommand.Revision != 1 || serviceCommand.CredentialGeneration != 1 ||
		serviceCommand.DesiredState != "running" || serviceCommand.CatalogVersion != "postgres/1.0.0" || !json.Valid(serviceCommand.Config) {
		t.Fatalf("unexpected Service reconciliation command: %+v", serviceCommand)
	}
	serviceDesiredLookup := func(tenantID domain.TenantID, requestID string, signatureOverride string) *httptest.ResponseRecorder {
		body, err := json.Marshal(workspaceServiceDesiredStateRequest{TenantID: tenantID, WorkspaceID: createResult.Workspace.ID})
		if err != nil {
			t.Fatalf("encode Service desired-state lookup: %v", err)
		}
		path := "/v1/internal/resource-planes/" + string(placementPlaneID) + "/workspace-services/" + string(postgresService.ID) + "/desired-state"
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		request.Header.Set(healthReportTimestampHeader, timestamp)
		request.Header.Set(healthReportIDHeader, requestID)
		bodyHash := sha256.Sum256(body)
		message := fmt.Sprintf("POST\n%s\n%s\n%s\n%s", request.URL.EscapedPath(), timestamp, requestID, hex.EncodeToString(bodyHash[:]))
		mac := hmac.New(sha256.New, serviceReportKey)
		_, _ = mac.Write([]byte(message))
		signature := hex.EncodeToString(mac.Sum(nil))
		if signatureOverride != "" {
			signature = signatureOverride
		}
		request.Header.Set(healthReportSignatureHeader, signature)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	currentServiceState := serviceDesiredLookup("tenant_member", strings.Repeat("d", 48), "")
	var currentServiceCommand domain.WorkspaceServiceCommand
	if currentServiceState.Code != http.StatusOK || json.Unmarshal(currentServiceState.Body.Bytes(), &currentServiceCommand) != nil ||
		currentServiceCommand.Revision != 1 || currentServiceCommand.CredentialGeneration != 1 || currentServiceCommand.DesiredState != "running" || currentServiceCommand.ServiceID != postgresService.ID {
		t.Fatalf("Resource Plane current Service desired-state lookup = HTTP %d %s", currentServiceState.Code, currentServiceState.Body.String())
	}
	foreignServiceState := serviceDesiredLookup("tenant_foreign", strings.Repeat("e", 48), "")
	if foreignServiceState.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant desired-state lookup must be hidden: HTTP %d %s", foreignServiceState.Code, foreignServiceState.Body.String())
	}
	invalidServiceState := serviceDesiredLookup("tenant_member", strings.Repeat("f", 48), strings.Repeat("0", 64))
	if invalidServiceState.Code != http.StatusUnauthorized {
		t.Fatalf("invalid desired-state HMAC must be rejected: HTTP %d %s", invalidServiceState.Code, invalidServiceState.Body.String())
	}
	replayedService := serviceRequest(http.MethodPost, `{"catalog_name":"postgres"}`)
	var replayedPostgres domain.Service
	if replayedService.Code != http.StatusAccepted || json.Unmarshal(replayedService.Body.Bytes(), &replayedPostgres) != nil || replayedPostgres.ID != postgresService.ID {
		t.Fatalf("identical Service add must return the existing instance: HTTP %d %s", replayedService.Code, replayedService.Body.String())
	}
	var serviceCommandCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM outbox_events WHERE aggregate_type = 'workspace_service' AND aggregate_id = $1`, postgresService.ID).Scan(&serviceCommandCount); err != nil || serviceCommandCount != 1 {
		t.Fatalf("idempotent Service add must not enqueue a duplicate command, count=%d error=%v", serviceCommandCount, err)
	}
	serviceAllocationGeneration := int64(1)
	serviceReport := func(reportID, microVMID string, revision int64, desiredState, observedState string, observedRevision int64, healthy bool, errorCode string, observedAt time.Time) *httptest.ResponseRecorder {
		reportBody, err := json.Marshal(workspaceServiceRuntimeReport{
			TenantID: "tenant_member", WorkspaceID: createResult.Workspace.ID, ResourcePlaneID: placementPlaneID,
			MicroVMID: microVMID, AllocationGeneration: serviceAllocationGeneration, ServiceID: postgresService.ID, Revision: revision, ObservedRevision: observedRevision,
			DesiredState: desiredState, ObservedState: observedState, Healthy: healthy, ErrorCode: errorCode, ObservedAt: observedAt.UTC(),
		})
		if err != nil {
			t.Fatalf("encode Workspace Service report: %v", err)
		}
		path := "/v1/internal/resource-planes/" + string(placementPlaneID) + "/workspace-services/" + string(postgresService.ID) + "/reports"
		timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(reportBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(healthReportTimestampHeader, timestamp)
		request.Header.Set(healthReportIDHeader, reportID)
		bodyHash := sha256.Sum256(reportBody)
		message := fmt.Sprintf("POST\n%s\n%s\n%s\n%s", request.URL.EscapedPath(), timestamp, reportID, hex.EncodeToString(bodyHash[:]))
		mac := hmac.New(sha256.New, serviceReportKey)
		_, _ = mac.Write([]byte(message))
		request.Header.Set(healthReportSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	wrongMicroVMReport := serviceReport(strings.Repeat("b", 48), "foreign-microvm", 1, "running", "healthy", 1, true, "", time.Now())
	if wrongMicroVMReport.Code != http.StatusOK || !strings.Contains(wrongMicroVMReport.Body.String(), `"accepted":false`) {
		t.Fatalf("report from an unbound MicroVM must be acknowledged but not applied: HTTP %d %s", wrongMicroVMReport.Code, wrongMicroVMReport.Body.String())
	}
	reportObservedAt := time.Now().UTC().Truncate(time.Second)
	firstServiceReport := serviceReport(strings.Repeat("a", 48), serviceMicroVMID, 1, "running", "healthy", 1, true, "", reportObservedAt)
	if firstServiceReport.Code != http.StatusOK || !strings.Contains(firstServiceReport.Body.String(), `"accepted":true`) {
		t.Fatalf("valid Workspace Service runtime report = HTTP %d %s", firstServiceReport.Code, firstServiceReport.Body.String())
	}
	var observedServiceState string
	var observedServiceRevision int64
	if err := pool.QueryRow(ctx, `SELECT observed_state, observed_revision FROM services WHERE id = $1`, postgresService.ID).Scan(&observedServiceState, &observedServiceRevision); err != nil || observedServiceState != "healthy" || observedServiceRevision != 1 {
		t.Fatalf("accepted runtime report must update observed state: state=%s revision=%d error=%v", observedServiceState, observedServiceRevision, err)
	}
	duplicateServiceReport := serviceReport(strings.Repeat("a", 48), serviceMicroVMID, 1, "running", "healthy", 1, true, "", reportObservedAt)
	if duplicateServiceReport.Code != http.StatusOK || !strings.Contains(duplicateServiceReport.Body.String(), `"accepted":true`) {
		t.Fatalf("duplicate accepted Workspace Service report must remain idempotently accepted: HTTP %d %s", duplicateServiceReport.Code, duplicateServiceReport.Body.String())
	}
	replayedServiceReport := serviceReport(strings.Repeat("a", 48), serviceMicroVMID, 1, "running", "healthy", 1, true, "", time.Now().Add(time.Second))
	if replayedServiceReport.Code != http.StatusConflict {
		t.Fatalf("reused Workspace Service report ID with different content must be rejected: HTTP %d %s", replayedServiceReport.Code, replayedServiceReport.Body.String())
	}
	const replacementMicroVMID = "microvm-service-report-replacement"
	if _, err := pool.Exec(ctx, `UPDATE resource_bindings SET external_id = $3, allocation_generation = 2
		WHERE workspace_id = $1 AND resource_plane_id = $2 AND kind = 'lambda_microvm'`, createResult.Workspace.ID, placementPlaneID, replacementMicroVMID); err != nil {
		t.Fatalf("advance Workspace MicroVM binding generation: %v", err)
	}
	serviceListAfterReplacement := serviceRequest(http.MethodGet, "")
	var replacementServiceList struct {
		Items []domain.Service `json:"items"`
	}
	if serviceListAfterReplacement.Code != http.StatusOK || json.Unmarshal(serviceListAfterReplacement.Body.Bytes(), &replacementServiceList) != nil ||
		len(replacementServiceList.Items) != 1 || replacementServiceList.Items[0].ObservedState != "pending" || replacementServiceList.Items[0].ObservedRuntimeGeneration != 0 {
		t.Fatalf("old healthy report must not appear current after MicroVM replacement: HTTP %d %s", serviceListAfterReplacement.Code, serviceListAfterReplacement.Body.String())
	}
	oldGenerationReport := serviceReport(strings.Repeat("9", 48), serviceMicroVMID, 1, "running", "healthy", 1, true, "", reportObservedAt)
	if oldGenerationReport.Code != http.StatusOK || !strings.Contains(oldGenerationReport.Body.String(), `"accepted":false`) {
		t.Fatalf("report from prior allocation generation must not be accepted: HTTP %d %s", oldGenerationReport.Code, oldGenerationReport.Body.String())
	}
	serviceAllocationGeneration = 2
	newGenerationReport := serviceReport(strings.Repeat("8", 48), replacementMicroVMID, 1, "running", "healthy", 1, true, "", reportObservedAt)
	if newGenerationReport.Code != http.StatusOK || !strings.Contains(newGenerationReport.Body.String(), `"accepted":true`) {
		t.Fatalf("report from the active allocation generation must be accepted: HTTP %d %s", newGenerationReport.Code, newGenerationReport.Body.String())
	}
	serviceListAfterCurrentReport := serviceRequest(http.MethodGet, "")
	var currentServiceList struct {
		Items []domain.Service `json:"items"`
	}
	if serviceListAfterCurrentReport.Code != http.StatusOK || json.Unmarshal(serviceListAfterCurrentReport.Body.Bytes(), &currentServiceList) != nil ||
		len(currentServiceList.Items) != 1 || currentServiceList.Items[0].ObservedState != "healthy" || currentServiceList.Items[0].ObservedRuntimeGeneration != 2 {
		t.Fatalf("current generation health report should appear in Workspace Service list: HTTP %d %s", serviceListAfterCurrentReport.Code, serviceListAfterCurrentReport.Body.String())
	}
	serviceAllocationGeneration = 1
	conflictingService := serviceRequest(http.MethodPost, `{"catalog_name":"redis","name":"postgres"}`)
	if conflictingService.Code != http.StatusConflict {
		t.Fatalf("different template must not replace an existing Service: HTTP %d %s", conflictingService.Code, conflictingService.Body.String())
	}
	serviceList := serviceRequest(http.MethodGet, "")
	var initialServiceList struct {
		Items []domain.Service `json:"items"`
	}
	if serviceList.Code != http.StatusOK || json.Unmarshal(serviceList.Body.Bytes(), &initialServiceList) != nil || len(initialServiceList.Items) != 1 ||
		initialServiceList.Items[0].ID != postgresService.ID || initialServiceList.Items[0].ObservedState != "healthy" || initialServiceList.Items[0].ObservedRuntimeGeneration != 2 {
		t.Fatalf("Workspace Service list = HTTP %d %s", serviceList.Code, serviceList.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workspace.service.add' AND target_id = $3`, "tenant_member", "usr_member", postgresService.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("Service creation and retry should produce one audit event, count=%d error=%v", auditCount, err)
	}
	secondPostgres := serviceRequest(http.MethodPost, `{"catalog_name":"postgres","name":"postgres-2"}`)
	if secondPostgres.Code != http.StatusAccepted {
		t.Fatalf("second PostgreSQL should fit exactly within the Workspace Service budget: HTTP %d %s", secondPostgres.Code, secondPostgres.Body.String())
	}
	overBudgetPostgres := serviceRequest(http.MethodPost, `{"catalog_name":"postgres","name":"postgres-3"}`)
	if overBudgetPostgres.Code != http.StatusConflict || !strings.Contains(overBudgetPostgres.Body.String(), `"service_resource_quota_exceeded"`) {
		t.Fatalf("third PostgreSQL should be rejected by the aggregate memory/CPU budget: HTTP %d %s", overBudgetPostgres.Code, overBudgetPostgres.Body.String())
	}
	serviceUsageResponse := requestWorkspaceServiceUsage(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID))
	var serviceUsage storeservices.ResourceUsage
	if serviceUsageResponse.Code != http.StatusOK || json.Unmarshal(serviceUsageResponse.Body.Bytes(), &serviceUsage) != nil ||
		serviceUsage.MemoryBytesUsed != storeservices.MaxWorkspaceServiceMemoryBytes || serviceUsage.MemoryBytesLimit != storeservices.MaxWorkspaceServiceMemoryBytes ||
		serviceUsage.MilliCPUUsed != storeservices.MaxWorkspaceServiceMilliCPU || serviceUsage.MilliCPULimit != storeservices.MaxWorkspaceServiceMilliCPU {
		t.Fatalf("Workspace Service usage should expose exact catalog-request reservations: HTTP %d body=%s usage=%+v", serviceUsageResponse.Code, serviceUsageResponse.Body.String(), serviceUsage)
	}
	foreignServiceUsage := requestWorkspaceServiceUsage(t, handler, createToken, "tenant_other", string(createResult.Workspace.ID))
	if foreignServiceUsage.Code != http.StatusNotFound {
		t.Fatalf("cross-Tenant Workspace Service usage must be hidden: HTTP %d %s", foreignServiceUsage.Code, foreignServiceUsage.Body.String())
	}
	serviceActionRequest := func(serviceName, action string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_member/workspaces/"+string(createResult.Workspace.ID)+"/services/"+serviceName+"/actions", strings.NewReader(`{"action":"`+action+`"}`))
		request.Header.Set("Authorization", "Bearer "+createToken)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	stoppedService := serviceActionRequest("postgres", "stop")
	var stoppedPostgres domain.Service
	if stoppedService.Code != http.StatusAccepted || json.Unmarshal(stoppedService.Body.Bytes(), &stoppedPostgres) != nil || stoppedPostgres.DesiredState != "stopped" || stoppedPostgres.ObservedState != "pending" || stoppedPostgres.DesiredRevision != 2 || stoppedPostgres.ObservedRevision != 1 {
		t.Fatalf("Workspace Service stop = HTTP %d %s", stoppedService.Code, stoppedService.Body.String())
	}
	currentServiceState = serviceDesiredLookup("tenant_member", strings.Repeat("1", 48), "")
	if currentServiceState.Code != http.StatusOK || json.Unmarshal(currentServiceState.Body.Bytes(), &currentServiceCommand) != nil ||
		currentServiceCommand.Revision != 2 || currentServiceCommand.DesiredState != "stopped" {
		t.Fatalf("desired-state resolver must observe newer stop after MicroVM recovery: HTTP %d %s", currentServiceState.Code, currentServiceState.Body.String())
	}
	staleServiceReport := serviceReport(strings.Repeat("c", 48), serviceMicroVMID, 1, "running", "healthy", 1, true, "", reportObservedAt)
	if staleServiceReport.Code != http.StatusOK || !strings.Contains(staleServiceReport.Body.String(), `"accepted":false`) {
		t.Fatalf("stale Workspace Service report should be acknowledged without applying: HTTP %d %s", staleServiceReport.Code, staleServiceReport.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT observed_state, observed_revision FROM services WHERE id = $1`, postgresService.ID).Scan(&observedServiceState, &observedServiceRevision); err != nil || observedServiceState != "pending" || observedServiceRevision != 1 {
		t.Fatalf("stale runtime report must not overwrite newer desired state: state=%s revision=%d error=%v", observedServiceState, observedServiceRevision, err)
	}
	repeatedStop := serviceActionRequest("postgres", "stop")
	var repeatedStoppedPostgres domain.Service
	if repeatedStop.Code != http.StatusAccepted || json.Unmarshal(repeatedStop.Body.Bytes(), &repeatedStoppedPostgres) != nil || repeatedStoppedPostgres.DesiredRevision != stoppedPostgres.DesiredRevision {
		t.Fatalf("identical Service state request should be idempotent: HTTP %d %s", repeatedStop.Code, repeatedStop.Body.String())
	}
	startedService := serviceActionRequest("postgres", "start")
	var startedPostgres domain.Service
	if startedService.Code != http.StatusAccepted || json.Unmarshal(startedService.Body.Bytes(), &startedPostgres) != nil || startedPostgres.DesiredState != "running" || startedPostgres.DesiredRevision != 3 {
		t.Fatalf("Workspace Service start = HTTP %d %s", startedService.Code, startedService.Body.String())
	}
	concurrentStops := make(chan *httptest.ResponseRecorder, 2)
	for range 2 {
		go func() { concurrentStops <- serviceActionRequest("postgres", "stop") }()
	}
	for range 2 {
		if response := <-concurrentStops; response.Code != http.StatusAccepted {
			t.Fatalf("concurrent identical Service action should be idempotent: HTTP %d %s", response.Code, response.Body.String())
		}
	}
	if response := serviceActionRequest("postgres", "start"); response.Code != http.StatusAccepted {
		t.Fatalf("Service should accept start after stop: HTTP %d %s", response.Code, response.Body.String())
	}
	removedService := serviceActionRequest("postgres", "remove")
	if removedService.Code != http.StatusAccepted || !strings.Contains(removedService.Body.String(), `"desired_state":"deleted"`) {
		t.Fatalf("Workspace Service remove = HTTP %d %s", removedService.Code, removedService.Body.String())
	}
	serviceAllocationGeneration = 2
	blockedCleanupReport := serviceReport(strings.Repeat("7", 48), replacementMicroVMID, 6, "deleted", "deleted", 6, false, "", reportObservedAt)
	if blockedCleanupReport.Code != http.StatusOK || !strings.Contains(blockedCleanupReport.Body.String(), `"accepted":false`) {
		t.Fatalf("current MicroVM must not confirm deletion while a prior generation remains unretired: HTTP %d %s", blockedCleanupReport.Code, blockedCleanupReport.Body.String())
	}
	if _, err := pool.Exec(ctx, `UPDATE workspace_service_runtime_generations SET retired = TRUE WHERE service_id = $1 AND allocation_generation = 1`, postgresService.ID); err != nil {
		t.Fatalf("confirm prior MicroVM Service cleanup: %v", err)
	}
	confirmedCleanupReport := serviceReport(strings.Repeat("6", 48), replacementMicroVMID, 6, "deleted", "deleted", 6, false, "", reportObservedAt)
	if confirmedCleanupReport.Code != http.StatusOK || !strings.Contains(confirmedCleanupReport.Body.String(), `"accepted":true`) {
		t.Fatalf("Service deletion should be confirmed after prior generations are retired: HTTP %d %s", confirmedCleanupReport.Code, confirmedCleanupReport.Body.String())
	}
	serviceUsageResponse = requestWorkspaceServiceUsage(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID))
	if serviceUsageResponse.Code != http.StatusOK || json.Unmarshal(serviceUsageResponse.Body.Bytes(), &serviceUsage) != nil ||
		serviceUsage.MemoryBytesUsed != 1<<30 || serviceUsage.MilliCPUUsed != 750 {
		t.Fatalf("confirmed Service deletion should release exactly one quota reservation: HTTP %d body=%s usage=%+v", serviceUsageResponse.Code, serviceUsageResponse.Body.String(), serviceUsage)
	}
	reuseDeletedName := serviceRequest(http.MethodPost, `{"catalog_name":"postgres"}`)
	if reuseDeletedName.Code != http.StatusConflict {
		t.Fatalf("removed Service name must remain reserved as an auditable tombstone: HTTP %d %s", reuseDeletedName.Code, reuseDeletedName.Body.String())
	}
	for _, action := range []string{"stop", "start", "remove"} {
		err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = $3 AND target_id = $4`, "tenant_member", "usr_member", "workspace.service."+action, postgresService.ID).Scan(&auditCount)
		wantAuditCount := 1
		if action == "stop" || action == "start" {
			wantAuditCount = 2 // each state is entered once before and once after the opposite transition
		}
		if err != nil || auditCount != wantAuditCount {
			t.Fatalf("service %s should produce one audit event per state transition, count=%d want=%d error=%v", action, auditCount, wantAuditCount, err)
		}
	}
	var commandCount, distinctRevisions int
	var maxRevision int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*), COUNT(DISTINCT (payload_json::jsonb->>'revision')), MAX((payload_json::jsonb->>'revision')::BIGINT)
		FROM outbox_events WHERE aggregate_type = 'workspace_service' AND aggregate_id = $1`, postgresService.ID).Scan(&commandCount, &distinctRevisions, &maxRevision); err != nil || commandCount != 6 || distinctRevisions != 6 || maxRevision != 6 {
		t.Fatalf("Service transitions must enqueue one command per new revision: count=%d distinct=%d max_revision=%d error=%v", commandCount, distinctRevisions, maxRevision, err)
	}
	rotatedResponse := serviceActionRequest("postgres-2", "rotate-credentials")
	var rotatedService domain.Service
	if rotatedResponse.Code != http.StatusAccepted || json.Unmarshal(rotatedResponse.Body.Bytes(), &rotatedService) != nil ||
		rotatedService.DesiredState != "running" || rotatedService.DesiredRevision != 2 || rotatedService.CredentialGeneration != 2 {
		t.Fatalf("PostgreSQL credential rotation = HTTP %d %s service=%+v", rotatedResponse.Code, rotatedResponse.Body.String(), rotatedService)
	}
	var rotatedPayload string
	if err := pool.QueryRow(ctx, `SELECT payload_json FROM outbox_events WHERE aggregate_type = 'workspace_service' AND aggregate_id = $1 ORDER BY created_at DESC LIMIT 1`, rotatedService.ID).Scan(&rotatedPayload); err != nil {
		t.Fatalf("load credential rotation Outbox command: %v", err)
	}
	var rotatedCommand domain.WorkspaceServiceCommand
	var rotatedPayloadFields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(rotatedPayload), &rotatedCommand); err != nil || json.Unmarshal([]byte(rotatedPayload), &rotatedPayloadFields) != nil ||
		rotatedCommand.CredentialGeneration != 2 || rotatedCommand.Revision != 2 || rotatedPayloadFields["secrets"] != nil || rotatedPayloadFields["secret_value"] != nil {
		t.Fatalf("rotation command did not carry only the credential generation: payload=%s command=%+v err=%v", rotatedPayload, rotatedCommand, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workspace.service.rotate-credentials' AND target_id = $3`, "tenant_member", "usr_member", rotatedService.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("credential rotation should be audit-recorded once: count=%d err=%v", auditCount, err)
	}
	redisForRotation := serviceRequest(http.MethodPost, `{"catalog_name":"redis","name":"redis-rotation-check"}`)
	if redisForRotation.Code != http.StatusAccepted {
		t.Fatalf("create Redis Service for rotation policy check: HTTP %d %s", redisForRotation.Code, redisForRotation.Body.String())
	}
	if response := serviceActionRequest("redis-rotation-check", "rotate-credentials"); response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), "service_credential_rotation_unavailable") {
		t.Fatalf("Service without generated credentials accepted rotation: HTTP %d %s", response.Code, response.Body.String())
	}
	oversized := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"`+strings.Repeat("x", maxJSONRequestBytes)+`"}`, "oversized-create")
	var oversizedError errorResponse
	if oversized.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(oversized.Body.Bytes(), &oversizedError) != nil || oversizedError.Error.Code != "request_too_large" {
		t.Fatalf("oversized request should use the common HTTP 413 error envelope: %d %s", oversized.Code, oversized.Body.String())
	}
	policyToken := signTenantAPIToken(t, privateKey, issuer, "owner-subject", "openid hako/api")
	idlePolicyGet := requestTenantWorkspaceIdlePolicy(t, handler, http.MethodGet, policyToken, "tenant_member", "")
	var idlePolicy workspaceactivity.Policy
	if idlePolicyGet.Code != http.StatusOK || json.Unmarshal(idlePolicyGet.Body.Bytes(), &idlePolicy) != nil || idlePolicy.Enabled ||
		idlePolicy.GracePeriodSeconds != int(workspaceactivity.DefaultIdleGracePeriod.Seconds()) || idlePolicy.Revision != 0 {
		t.Fatalf("Tenant Owner should read the disabled default idle policy: HTTP %d %s policy=%+v", idlePolicyGet.Code, idlePolicyGet.Body.String(), idlePolicy)
	}
	memberIdlePolicy := requestTenantWorkspaceIdlePolicy(t, handler, http.MethodPut, createToken, "tenant_member", `{"enabled":true,"grace_period_seconds":900}`)
	if memberIdlePolicy.Code != http.StatusNotFound {
		t.Fatalf("Tenant Member must not manage idle policy: HTTP %d %s", memberIdlePolicy.Code, memberIdlePolicy.Body.String())
	}
	invalidIdlePolicy := requestTenantWorkspaceIdlePolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"enabled":true,"grace_period_seconds":299}`)
	if invalidIdlePolicy.Code != http.StatusBadRequest {
		t.Fatalf("idle policy below grace minimum should be rejected: HTTP %d %s", invalidIdlePolicy.Code, invalidIdlePolicy.Body.String())
	}
	updatedIdlePolicy := requestTenantWorkspaceIdlePolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"enabled":true,"grace_period_seconds":900}`)
	if updatedIdlePolicy.Code != http.StatusOK || json.Unmarshal(updatedIdlePolicy.Body.Bytes(), &idlePolicy) != nil || !idlePolicy.Enabled || idlePolicy.Revision != 1 {
		t.Fatalf("Tenant Owner should opt into idle suspension: HTTP %d %s policy=%+v", updatedIdlePolicy.Code, updatedIdlePolicy.Body.String(), idlePolicy)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = 'tenant_member' AND actor_user_id = 'usr_owner' AND action = 'tenant.workspace_idle_policy.update'`).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("idle policy update should be audited once, count=%d error=%v", auditCount, err)
	}
	policySet := requestTenantPlacementPolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"resource_plane_ids":["rp_not_allowed"],"allowed_regions":[],"required_capabilities":[],"max_cost_tier":"standard","minimum_isolation_tier":"dedicated"}`)
	if policySet.Code != http.StatusOK {
		t.Fatalf("Tenant Owner should set placement policy: %d %s", policySet.Code, policySet.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'tenant.placement_policy.update' AND target_id = $1`, "tenant_member", "usr_owner").Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("placement policy update should produce one actor audit event, count=%d error=%v", auditCount, err)
	}
	memberPolicySet := requestTenantPlacementPolicy(t, handler, http.MethodPut, createToken, "tenant_member", `{"allowed_regions":[],"resource_plane_ids":[],"required_capabilities":[]}`)
	if memberPolicySet.Code != http.StatusNotFound {
		t.Fatalf("Tenant Member must not manage placement policy: %d %s", memberPolicySet.Code, memberPolicySet.Body.String())
	}
	policyGet := requestTenantPlacementPolicy(t, handler, http.MethodGet, policyToken, "tenant_member", "")
	var storedPolicy map[string]any
	if policyGet.Code != http.StatusOK || json.Unmarshal(policyGet.Body.Bytes(), &storedPolicy) != nil || storedPolicy["max_cost_tier"] != "standard" || storedPolicy["minimum_isolation_tier"] != "dedicated" {
		t.Fatalf("Tenant Owner should read placement policy: %d %s", policyGet.Code, policyGet.Body.String())
	}
	blockedByTenantPolicy := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"tenant-policy-blocked"}`, "tenant-policy-blocked")
	if blockedByTenantPolicy.Code != http.StatusServiceUnavailable {
		t.Fatalf("Scheduler must enforce Tenant Resource Plane constraints: %d %s", blockedByTenantPolicy.Code, blockedByTenantPolicy.Body.String())
	}
	invalidPolicy := requestTenantPlacementPolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"allowed_regions":["us-east-1","US-EAST-1"],"resource_plane_ids":[],"required_capabilities":[]}`)
	if invalidPolicy.Code != http.StatusBadRequest {
		t.Fatalf("duplicate normalized policy selectors must be rejected: %d %s", invalidPolicy.Code, invalidPolicy.Body.String())
	}
	invalidTier := requestTenantPlacementPolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"allowed_regions":[],"resource_plane_ids":[],"required_capabilities":[],"max_cost_tier":"free"}`)
	if invalidTier.Code != http.StatusBadRequest {
		t.Fatalf("unsupported Cost tier must be rejected: %d %s", invalidTier.Code, invalidTier.Body.String())
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_placement_policies WHERE tenant_id = $1`, "tenant_member"); err != nil {
		t.Fatalf("clear test placement policy: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE resource_plane_status SET status = 'disabled' WHERE resource_plane_id = $1`, "rp_test"); err != nil {
		t.Fatalf("disable Resource Plane for placement failure case: %v", err)
	}
	noPlacement := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"no-placement"}`, "no-placement")
	if _, err := pool.Exec(ctx, `UPDATE resource_plane_status SET status = 'active' WHERE resource_plane_id = $1`, "rp_test"); err != nil {
		t.Fatalf("restore Resource Plane status: %v", err)
	}
	var noPlacementError errorResponse
	if noPlacement.Code != http.StatusServiceUnavailable || json.Unmarshal(noPlacement.Body.Bytes(), &noPlacementError) != nil || noPlacementError.Error.Code != "resource_plane_unavailable" {
		t.Fatalf("no eligible Resource Plane should return stable 503: %d %s", noPlacement.Code, noPlacement.Body.String())
	}
	var orphanCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspaces WHERE tenant_id = $1 AND name = $2`, "tenant_member", "no-placement").Scan(&orphanCount); err != nil || orphanCount != 0 {
		t.Fatalf("failed placement must not leave a Workspace: count=%d error=%v", orphanCount, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_quota_slots WHERE tenant_id = $1`, "tenant_member").Scan(&orphanCount); err != nil || orphanCount != 1 {
		t.Fatalf("failed placement must not consume a quota slot: slots=%d error=%v", orphanCount, err)
	}
	otherWorkspace, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_member", OwnerID: "usr_other", Name: "other-dev",
		RuntimeClass: "standard", Image: "hako/go:test", ResourcePlaneID: "rp_test", IdempotencyKey: "other-workspace",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("create another member's Workspace: %v", err)
	}
	otherMemberServices := httptest.NewRequest(http.MethodGet, "/v1/tenants/tenant_member/workspaces/"+string(otherWorkspace.Workspace.ID)+"/services", nil)
	otherMemberServices.Header.Set("Authorization", "Bearer "+createToken)
	otherMemberServicesResponse := httptest.NewRecorder()
	handler.ServeHTTP(otherMemberServicesResponse, otherMemberServices)
	if otherMemberServicesResponse.Code != http.StatusNotFound {
		t.Fatalf("Tenant member read another member's Workspace Services: HTTP %d %s", otherMemberServicesResponse.Code, otherMemberServicesResponse.Body.String())
	}
	foreignWorkspace, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_other", OwnerID: "usr_other", Name: "foreign-dev",
		RuntimeClass: "standard", Image: "hako/go:test", ResourcePlaneID: "rp_test", IdempotencyKey: "foreign-workspace",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("create other Tenant Workspace: %v", err)
	}
	otherOwnerToken := signTenantAPIToken(t, privateKey, issuer, "other-subject", "openid hako/api")
	otherMemberQuotaWrite := requestTenantQuota(t, handler, createToken, http.MethodPut, "tenant_other", `{"workspaces_limit":1,"workload_runs_limit":3,"artifact_bytes_limit":10737418240,"service_memory_bytes_limit":2147483648,"service_milli_cpu_limit":1500}`)
	if otherMemberQuotaWrite.Code != http.StatusNotFound {
		t.Fatalf("non-owner from another Tenant must not configure quotas: HTTP %d %s", otherMemberQuotaWrite.Code, otherMemberQuotaWrite.Body.String())
	}
	defaultQuota := requestTenantQuota(t, handler, otherOwnerToken, http.MethodGet, "tenant_other", "")
	var configuredLimits struct {
		TenantID           string `json:"tenant_id"`
		Workspaces         int16  `json:"workspaces_limit"`
		WorkloadRuns       int16  `json:"workload_runs_limit"`
		QueuedWorkloadRuns int16  `json:"queued_workload_runs_limit"`
		PreviewRuns        int16  `json:"preview_runs_limit"`
		PreviewLifetime    int64  `json:"preview_lifetime_seconds_limit"`
		ArtifactBytes      int64  `json:"artifact_bytes_limit"`
		ServiceMemoryBytes int64  `json:"service_memory_bytes_limit"`
		ServiceMilliCPU    int64  `json:"service_milli_cpu_limit"`
	}
	if defaultQuota.Code != http.StatusOK || json.Unmarshal(defaultQuota.Body.Bytes(), &configuredLimits) != nil || configuredLimits.Workspaces != 10 || configuredLimits.WorkloadRuns != 5 || configuredLimits.QueuedWorkloadRuns != 5 || configuredLimits.PreviewRuns != 2 || configuredLimits.PreviewLifetime != 86400 || configuredLimits.ArtifactBytes != 10<<30 || configuredLimits.ServiceMemoryBytes != 2<<30 || configuredLimits.ServiceMilliCPU != 1500 {
		t.Fatalf("Tenant without override should receive platform quota defaults: HTTP %d %s", defaultQuota.Code, defaultQuota.Body.String())
	}
	setQuota := requestTenantQuota(t, handler, otherOwnerToken, http.MethodPut, "tenant_other", `{"workspaces_limit":2,"workload_runs_limit":3,"queued_workload_runs_limit":1,"preview_runs_limit":1,"preview_lifetime_seconds_limit":3600,"artifact_bytes_limit":2147483648,"service_memory_bytes_limit":1073741824,"service_milli_cpu_limit":750}`)
	if setQuota.Code != http.StatusOK || json.Unmarshal(setQuota.Body.Bytes(), &configuredLimits) != nil || configuredLimits.Workspaces != 2 || configuredLimits.WorkloadRuns != 3 || configuredLimits.QueuedWorkloadRuns != 1 || configuredLimits.PreviewRuns != 1 || configuredLimits.PreviewLifetime != 3600 || configuredLimits.ArtifactBytes != 2147483648 || configuredLimits.ServiceMemoryBytes != 1073741824 || configuredLimits.ServiceMilliCPU != 750 {
		t.Fatalf("Tenant Owner should configure lower Tenant quotas: HTTP %d %s", setQuota.Code, setQuota.Body.String())
	}
	secondOtherWorkspace := requestCreateWorkspace(t, handler, otherOwnerToken, "tenant_other", `{"name":"quota-two"}`, "quota-two")
	if secondOtherWorkspace.Code != http.StatusAccepted {
		t.Fatalf("Workspace admission should honor configured Tenant limit of two: HTTP %d %s", secondOtherWorkspace.Code, secondOtherWorkspace.Body.String())
	}
	previewCreatedAt := time.Now().UTC().Truncate(time.Second)
	previewRun := domain.WorkloadRun{ID: "run_tenant_quota_preview", TenantID: "tenant_other", RequestedBy: "usr_other",
		Kind: domain.WorkloadPreview, RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		IdempotencyKey: "tenant-quota-preview", TimeoutSeconds: 1800, TimeoutAt: previewCreatedAt.Add(30 * time.Minute), CreatedAt: previewCreatedAt}
	if _, err := transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		stored, _, err := storeworkloads.CreateOrGet(ctx, tx, previewRun)
		return stored, err
	}); err != nil {
		t.Fatalf("create Preview Run under Tenant Preview quota: %v", err)
	}
	quotaBelowUsage := requestTenantQuota(t, handler, otherOwnerToken, http.MethodPut, "tenant_other", `{"workspaces_limit":1,"workload_runs_limit":3,"artifact_bytes_limit":2147483648,"service_memory_bytes_limit":1073741824,"service_milli_cpu_limit":750}`)
	if quotaBelowUsage.Code != http.StatusConflict || !strings.Contains(quotaBelowUsage.Body.String(), `"quota_below_current_usage"`) {
		t.Fatalf("Tenant quota update below current reservations must conflict: HTTP %d %s", quotaBelowUsage.Code, quotaBelowUsage.Body.String())
	}
	otherUsage := requestWorkspaceUsage(t, handler, otherOwnerToken, "tenant_other")
	var reducedUsage workspaces.QuotaUsage
	if otherUsage.Code != http.StatusOK || json.Unmarshal(otherUsage.Body.Bytes(), &reducedUsage) != nil || reducedUsage.WorkspacesUsed != 2 || reducedUsage.WorkspacesLimit != 2 || reducedUsage.WorkloadRunsUsed != 1 || reducedUsage.WorkloadRunsLimit != 3 || reducedUsage.PreviewRunsUsed != 1 || reducedUsage.PreviewRunsLimit != 1 || reducedUsage.PreviewLifetimeLimit != 3600 || reducedUsage.ArtifactBytesUsed != 0 || reducedUsage.ArtifactBytesReserved != 100<<20 || reducedUsage.ArtifactBytesLimit != 2147483648 {
		t.Fatalf("usage must report effective Tenant-specific limits: HTTP %d %s usage=%+v", otherUsage.Code, otherUsage.Body.String(), reducedUsage)
	}
	previewQuotaBelowUsage := requestTenantQuota(t, handler, otherOwnerToken, http.MethodPut, "tenant_other", `{"workspaces_limit":2,"workload_runs_limit":3,"preview_runs_limit":1,"preview_lifetime_seconds_limit":900,"artifact_bytes_limit":2147483648,"service_memory_bytes_limit":1073741824,"service_milli_cpu_limit":750}`)
	if previewQuotaBelowUsage.Code != http.StatusConflict || !strings.Contains(previewQuotaBelowUsage.Body.String(), `"quota_below_current_usage"`) {
		t.Fatalf("Preview lifetime update below active Run timeout must conflict: HTTP %d %s", previewQuotaBelowUsage.Code, previewQuotaBelowUsage.Body.String())
	}
	overConfiguredWorkspaceLimit := requestCreateWorkspace(t, handler, otherOwnerToken, "tenant_other", `{"name":"over-configured-quota"}`, "over-configured-quota")
	if overConfiguredWorkspaceLimit.Code != http.StatusConflict || !strings.Contains(overConfiguredWorkspaceLimit.Body.String(), `"workspace_quota_exceeded"`) {
		t.Fatalf("Workspace admission must honor Tenant-specific limit: HTTP %d %s", overConfiguredWorkspaceLimit.Code, overConfiguredWorkspaceLimit.Body.String())
	}
	memberList := requestWorkspaceList(t, handler, createToken, "tenant_member", "")
	var memberPage workspaces.ListResult
	if memberList.Code != http.StatusOK || json.Unmarshal(memberList.Body.Bytes(), &memberPage) != nil || memberPage.Total != 1 || len(memberPage.Items) != 1 || memberPage.Items[0].Workspace.OwnerID != "usr_member" {
		t.Fatalf("member should list only own Workspace: %d %s", memberList.Code, memberList.Body.String())
	}
	invalidPage := requestWorkspaceList(t, handler, createToken, "tenant_member", "?limit=101")
	if invalidPage.Code != http.StatusBadRequest {
		t.Fatalf("out-of-range page size returned HTTP %d, want 400", invalidPage.Code)
	}
	memberOtherGet := requestWorkspaceGet(t, handler, createToken, "tenant_member", string(otherWorkspace.Workspace.ID))
	memberOwnGet := requestWorkspaceGet(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID))
	memberMissingGet := requestWorkspaceGet(t, handler, createToken, "tenant_member", "ws_missing")
	memberForeignGet := requestWorkspaceGet(t, handler, createToken, "tenant_member", string(foreignWorkspace.Workspace.ID))
	if memberOtherGet.Code != http.StatusNotFound || memberOwnGet.Code != http.StatusOK || memberMissingGet.Code != http.StatusNotFound || memberForeignGet.Code != http.StatusNotFound {
		t.Fatalf("member get policy mismatch: own=%d other=%d %s", memberOwnGet.Code, memberOtherGet.Code, memberOtherGet.Body.String())
	}
	if memberOtherGet.Body.String() != memberMissingGet.Body.String() || memberForeignGet.Body.String() != memberMissingGet.Body.String() {
		t.Fatalf("hidden or foreign Workspace must be indistinguishable from missing: hidden=%s missing=%s foreign=%s", memberOtherGet.Body.String(), memberMissingGet.Body.String(), memberForeignGet.Body.String())
	}
	memberOtherAction := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(otherWorkspace.Workspace.ID), "suspend", "hidden-action")
	if memberOtherAction.Code != http.StatusNotFound {
		t.Fatalf("member should not act on another member's Workspace: %d %s", memberOtherAction.Code, memberOtherAction.Body.String())
	}
	for _, caller := range []struct {
		subject string
		role    domain.TenantRole
	}{{"owner-subject", domain.TenantRoleOwner}, {"admin-subject", domain.TenantRoleAdmin}} {
		token := signTenantAPIToken(t, privateKey, issuer, caller.subject, "openid hako/api")
		page := requestWorkspaceList(t, handler, token, "tenant_member", "?limit=1&offset=0")
		var result workspaces.ListResult
		if page.Code != http.StatusOK || json.Unmarshal(page.Body.Bytes(), &result) != nil || result.Total != 2 || len(result.Items) != 1 || result.Limit != 1 {
			t.Fatalf("Tenant %s should list all Workspaces with pagination: %d %s", caller.role, page.Code, page.Body.String())
		}
		workspaceGet := requestWorkspaceGet(t, handler, token, "tenant_member", string(otherWorkspace.Workspace.ID))
		if workspaceGet.Code != http.StatusOK {
			t.Fatalf("Tenant %s should get another member's Workspace: %d %s", caller.role, workspaceGet.Code, workspaceGet.Body.String())
		}
	}

	// Execute the actual Outbox command through Fake Runtime and consume its
	// result back into the Control Plane before requesting another action.
	fakeRuntime := fakeruntime.New()
	runOperationThroughFakeRuntime(t, ctx, pool, createResult.OperationID, fakeRuntime)
	assertWorkspaceObservedState(t, handler, createToken, "tenant_member", createResult.Workspace.ID, domain.ObservedWorkspaceRunning)
	suspend := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "suspend", "suspend-api-dev")
	var suspendResult workspaceActionResponse
	if suspend.Code != http.StatusAccepted || json.Unmarshal(suspend.Body.Bytes(), &suspendResult) != nil || suspendResult.OperationType != domain.OperationSuspend || suspendResult.Workspace.Status.DesiredState != domain.DesiredWorkspaceSuspended {
		t.Fatalf("suspend action should be accepted asynchronously: %d %s", suspend.Code, suspend.Body.String())
	}
	if err := pool.QueryRow(ctx, `SELECT payload_json FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'operation.requested'`, suspendResult.OperationID).Scan(&operationPayload); err != nil {
		t.Fatalf("load API-created Workspace action command: %v", err)
	}
	if json.Unmarshal([]byte(operationPayload), &operationCorrelation) != nil || operationCorrelation.CorrelationID == "" ||
		operationCorrelation.CorrelationID != suspend.Header().Get("X-Hako-Request-ID") {
		t.Fatalf("API-generated request ID was not persisted with Workspace action: payload=%s header=%q", operationPayload, suspend.Header().Get("X-Hako-Request-ID"))
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workspace.suspend' AND target_id = $3`, "tenant_member", "usr_member", createResult.Workspace.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("Workspace suspend should produce one actor audit event, count=%d error=%v", auditCount, err)
	}
	suspendReplay := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "suspend", "suspend-api-dev")
	var suspendReplayResult workspaceActionResponse
	if suspendReplay.Code != http.StatusAccepted || json.Unmarshal(suspendReplay.Body.Bytes(), &suspendReplayResult) != nil || suspendReplayResult.OperationID != suspendResult.OperationID {
		t.Fatalf("same action key should return original Operation: %d %s", suspendReplay.Code, suspendReplay.Body.String())
	}
	keyConflict := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "resume", "suspend-api-dev")
	var keyConflictError errorResponse
	if keyConflict.Code != http.StatusConflict || json.Unmarshal(keyConflict.Body.Bytes(), &keyConflictError) != nil || keyConflictError.Error.Code != "idempotency_conflict" {
		t.Fatalf("reusing an action key for a different action should conflict: %d %s", keyConflict.Code, keyConflict.Body.String())
	}
	inProgress := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "resume", "resume-too-soon")
	var inProgressError errorResponse
	if inProgress.Code != http.StatusConflict || json.Unmarshal(inProgress.Body.Bytes(), &inProgressError) != nil || inProgressError.Error.Code != "operation_in_progress" {
		t.Fatalf("conflicting action should be rejected while an Operation is active: %d %s", inProgress.Code, inProgress.Body.String())
	}
	runOperationThroughFakeRuntime(t, ctx, pool, suspendResult.OperationID, fakeRuntime)
	assertWorkspaceObservedState(t, handler, createToken, "tenant_member", createResult.Workspace.ID, domain.ObservedWorkspaceSuspended)
	resume := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "resume", "resume-api-dev")
	var resumeResult workspaceActionResponse
	if resume.Code != http.StatusAccepted || json.Unmarshal(resume.Body.Bytes(), &resumeResult) != nil || resumeResult.Workspace.Status.DesiredState != domain.DesiredWorkspaceRunning {
		t.Fatalf("resume action should be accepted asynchronously: %d %s", resume.Code, resume.Body.String())
	}
	runOperationThroughFakeRuntime(t, ctx, pool, resumeResult.OperationID, fakeRuntime)
	assertWorkspaceObservedState(t, handler, createToken, "tenant_member", createResult.Workspace.ID, domain.ObservedWorkspaceRunning)
	delete := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(createResult.Workspace.ID), "delete", "delete-api-dev")
	var deleteResult workspaceActionResponse
	if delete.Code != http.StatusAccepted || json.Unmarshal(delete.Body.Bytes(), &deleteResult) != nil || deleteResult.Workspace.Status.DesiredState != domain.DesiredWorkspaceDeleted {
		t.Fatalf("delete action should be accepted asynchronously: %d %s", delete.Code, delete.Body.String())
	}
	var quotaSlots int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_quota_slots WHERE workspace_id = $1`, createResult.Workspace.ID).Scan(&quotaSlots); err != nil || quotaSlots != 1 {
		t.Fatalf("delete request must retain quota until runtime reports deletion: slots=%d error=%v", quotaSlots, err)
	}
	usageResponse = requestWorkspaceUsage(t, handler, createToken, "tenant_member")
	if usageResponse.Code != http.StatusOK || json.Unmarshal(usageResponse.Body.Bytes(), &usage) != nil || usage.WorkspacesUsed != 2 {
		t.Fatalf("pending deletion must remain in aggregate usage: HTTP %d body=%s usage=%+v", usageResponse.Code, usageResponse.Body.String(), usage)
	}
	runOperationThroughFakeRuntime(t, ctx, pool, deleteResult.OperationID, fakeRuntime)
	assertWorkspaceObservedState(t, handler, createToken, "tenant_member", createResult.Workspace.ID, domain.ObservedWorkspaceDeleted)
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_quota_slots WHERE workspace_id = $1`, createResult.Workspace.ID).Scan(&quotaSlots); err != nil || quotaSlots != 0 {
		t.Fatalf("observed deletion should release quota: slots=%d error=%v", quotaSlots, err)
	}
	usageResponse = requestWorkspaceUsage(t, handler, createToken, "tenant_member")
	if usageResponse.Code != http.StatusOK || json.Unmarshal(usageResponse.Body.Bytes(), &usage) != nil || usage.WorkspacesUsed != 1 {
		t.Fatalf("confirmed deletion should release aggregate usage: HTTP %d body=%s usage=%+v", usageResponse.Code, usageResponse.Body.String(), usage)
	}
	invalidAction := sendWorkspaceAction(t, handler, createToken, "tenant_member", string(otherWorkspace.Workspace.ID), "restart", "bad-action")
	if invalidAction.Code != http.StatusBadRequest {
		t.Fatalf("unsupported lifecycle action should return 400: %d %s", invalidAction.Code, invalidAction.Body.String())
	}

	for i := 1; i < workspaces.MaxWorkspacesPerTenant; i++ {
		_, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
			TenantID: "tenant_member", OwnerID: "usr_member", Name: fmt.Sprintf("fill-%02d", i),
			RuntimeClass: "standard", Image: "hako/go:test", ResourcePlaneID: "rp_test",
			IdempotencyKey: fmt.Sprintf("fill-key-%02d", i),
		}, transaction.DefaultPolicy())
		if err != nil {
			t.Fatalf("fill Tenant quota slot %d: %v", i+1, err)
		}
	}
	quotaExceeded := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"eleventh"}`, "create-eleventh")
	var quotaError errorResponse
	if quotaExceeded.Code != http.StatusConflict || json.Unmarshal(quotaExceeded.Body.Bytes(), &quotaError) != nil || quotaError.Error.Code != "workspace_quota_exceeded" {
		t.Fatalf("quota overflow should return stable HTTP 409: %d %s", quotaExceeded.Code, quotaExceeded.Body.String())
	}
	usageResponse = requestWorkspaceUsage(t, handler, createToken, "tenant_member")
	if usageResponse.Code != http.StatusOK || json.Unmarshal(usageResponse.Body.Bytes(), &usage) != nil || usage.WorkspacesUsed != int64(workspaces.MaxWorkspacesPerTenant) {
		t.Fatalf("quota usage should expose aggregate reserved slot count: HTTP %d body=%s usage=%+v", usageResponse.Code, usageResponse.Body.String(), usage)
	}
	if _, err := pool.Exec(ctx, `DROP TABLE audit_events`); err != nil {
		t.Fatalf("remove audit sink for rollback verification: %v", err)
	}
	auditFailure := requestTenantPlacementPolicy(t, handler, http.MethodPut, policyToken, "tenant_member", `{"allowed_regions":["ap-northeast-1"],"resource_plane_ids":[],"required_capabilities":[]}`)
	if auditFailure.Code != http.StatusServiceUnavailable {
		t.Fatalf("audit persistence failure should fail the policy update: %d %s", auditFailure.Code, auditFailure.Body.String())
	}
	var policyExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM tenant_placement_policies WHERE tenant_id = $1)`, "tenant_member").Scan(&policyExists); err != nil || policyExists {
		t.Fatalf("policy mutation must roll back when its audit event cannot be recorded, exists=%t error=%v", policyExists, err)
	}
}

func TestResourcePlaneHealthRequiresPlatformAdminAndAuditsUpdates(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	now := time.Now().UTC()
	if _, err := pool.Exec(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ('usr_platform_admin', 'platform-admin-sub', '', $1), ('usr_regular', 'regular-sub', '', $1)`, now); err != nil {
		t.Fatalf("seed platform users: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO resource_planes (id, provider, region, capabilities_json) VALUES ('rp-health-api', 'aws', 'ap-northeast-1', '["microvm"]')`); err != nil {
		t.Fatalf("seed Resource Plane: %v", err)
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()),
			"e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatalf("create Cognito verifier: %v", err)
	}
	handler := NewHandler(verifier, pool)
	regular := signAPITokenWithGroups(t, privateKey, issuer, "regular-sub", "openid hako/api", nil)
	for _, method := range []string{http.MethodGet, http.MethodPut} {
		response := requestResourcePlaneHealth(t, handler, method, regular, `{"status":"unhealthy","reason":"test"}`)
		var apiError errorResponse
		if response.Code != http.StatusForbidden || json.Unmarshal(response.Body.Bytes(), &apiError) != nil || apiError.Error.Code != "insufficient_platform_role" {
			t.Fatalf("non-admin %s health request = %d %s", method, response.Code, response.Body.String())
		}
	}
	admin := signAPITokenWithGroups(t, privateKey, issuer, "platform-admin-sub", "openid hako/api", []string{"hako-admin"})
	initial := requestResourcePlaneHealth(t, handler, http.MethodGet, admin, "")
	var health struct {
		Status          string `json:"status"`
		EffectiveStatus string `json:"effective_status"`
	}
	if initial.Code != http.StatusOK || json.Unmarshal(initial.Body.Bytes(), &health) != nil || health.Status != "unknown" || health.EffectiveStatus != "healthy" {
		t.Fatalf("unreported health response = %d %s", initial.Code, initial.Body.String())
	}
	updated := requestResourcePlaneHealth(t, handler, http.MethodPut, admin, `{"status":"degraded","reason":"queue latency elevated"}`)
	if updated.Code != http.StatusOK || json.Unmarshal(updated.Body.Bytes(), &health) != nil || health.Status != "degraded" || health.EffectiveStatus != "degraded" {
		t.Fatalf("admin health update = %d %s", updated.Code, updated.Body.String())
	}
	var audits int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM platform_audit_events WHERE actor_user_id = 'usr_platform_admin' AND target_id = 'rp-health-api'`).Scan(&audits); err != nil || audits != 1 {
		t.Fatalf("platform audit count = %d, error=%v", audits, err)
	}
	bad := requestResourcePlaneHealth(t, handler, http.MethodPut, admin, `{"status":"unknown","reason":"invalid"}`)
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("invalid health update returned HTTP %d: %s", bad.Code, bad.Body.String())
	}
	missing := requestForResourcePlane(t, handler, http.MethodGet, admin, "rp-missing", "")
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing Resource Plane returned HTTP %d: %s", missing.Code, missing.Body.String())
	}

	sharedKey := []byte("0123456789abcdef0123456789abcdef")
	reporter := NewHandler(nil, pool, WorkspaceCreateConfig{
		ResourcePlaneHealthReportKeyResolver: func(_ context.Context, id domain.ResourcePlaneID) ([]byte, error) {
			if id != "rp-health-api" {
				return nil, fmt.Errorf("unknown Resource Plane")
			}
			return sharedKey, nil
		},
	})
	observedAt := time.Now().UTC().Truncate(time.Second)
	reportBody := fmt.Sprintf(`{"status":"unhealthy","reason":"provider probe failed","observed_at":%q}`, observedAt.Format(time.RFC3339Nano))
	postReport := func(id, key string) *httptest.ResponseRecorder {
		timestamp := strconv.FormatInt(time.Now().UTC().Unix(), 10)
		request := httptest.NewRequest(http.MethodPost, "/v1/internal/resource-planes/rp-health-api/health", strings.NewReader(reportBody))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set(healthReportTimestampHeader, timestamp)
		request.Header.Set(healthReportIDHeader, id)
		bodyHash := sha256.Sum256([]byte(reportBody))
		message := fmt.Sprintf("POST\n%s\n%s\n%s\n%s", request.URL.EscapedPath(), timestamp, id, hex.EncodeToString(bodyHash[:]))
		mac := hmac.New(sha256.New, []byte(key))
		_, _ = mac.Write([]byte(message))
		request.Header.Set(healthReportSignatureHeader, hex.EncodeToString(mac.Sum(nil)))
		recorder := httptest.NewRecorder()
		reporter.ServeHTTP(recorder, request)
		return recorder
	}
	forged := postReport("probe-api-001", "wrong-key-012345678901234567890123")
	if forged.Code != http.StatusUnauthorized {
		t.Fatalf("forged machine report returned HTTP %d: %s", forged.Code, forged.Body.String())
	}
	firstReport := postReport("probe-api-001", string(sharedKey))
	if firstReport.Code != http.StatusOK || !strings.Contains(firstReport.Body.String(), `"accepted":true`) {
		t.Fatalf("valid machine report = %d %s", firstReport.Code, firstReport.Body.String())
	}
	duplicateReport := postReport("probe-api-001", string(sharedKey))
	if duplicateReport.Code != http.StatusOK || !strings.Contains(duplicateReport.Body.String(), `"accepted":false`) {
		t.Fatalf("duplicate machine report = %d %s", duplicateReport.Code, duplicateReport.Body.String())
	}
}

func requestResourcePlaneHealth(t *testing.T, handler http.Handler, method, token, body string) *httptest.ResponseRecorder {
	return requestForResourcePlane(t, handler, method, token, "rp-health-api", body)
}

func requestForResourcePlane(t *testing.T, handler http.Handler, method, token, resourcePlaneID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/v1/admin/resource-planes/"+resourcePlaneID+"/health", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestTenantPlacementPolicy(t *testing.T, handler http.Handler, method, token, tenantID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/v1/tenants/"+tenantID+"/placement-policy", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestTenantWorkspaceIdlePolicy(t *testing.T, handler http.Handler, method, token, tenantID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/v1/tenants/"+tenantID+"/workspace-idle-policy", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestTenantMembership(t *testing.T, handler http.Handler, token, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID+"/membership", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestCreateWorkspace(t *testing.T, handler http.Handler, token, tenantID, body, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+tenantID+"/workspaces", bytes.NewBufferString(body))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestWorkspaceList(t *testing.T, handler http.Handler, token, tenantID, query string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID+"/workspaces"+query, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestWorkspaceUsage(t *testing.T, handler http.Handler, token, tenantID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID+"/usage", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestTenantQuota(t *testing.T, handler http.Handler, token, method, tenantID, body string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, "/v1/tenants/"+tenantID+"/quotas", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+token)
	if method == http.MethodPut {
		request.Header.Set("Content-Type", "application/json")
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestWorkspaceServiceUsage(t *testing.T, handler http.Handler, token, tenantID, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID+"/workspaces/"+workspaceID+"/services/usage", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func requestWorkspaceGet(t *testing.T, handler http.Handler, token, tenantID, workspaceID string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/v1/tenants/"+tenantID+"/workspaces/"+workspaceID, nil)
	request.Header.Set("Authorization", "Bearer "+token)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func sendWorkspaceAction(t *testing.T, handler http.Handler, token, tenantID, workspaceID, action, idempotencyKey string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/tenants/"+tenantID+"/workspaces/"+workspaceID+"/actions", bytes.NewBufferString(`{"action":"`+action+`"}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", idempotencyKey)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}

func runOperationThroughFakeRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id domain.OperationID, runtime *fakeruntime.Runtime) {
	t.Helper()
	var command []byte
	if err := pool.QueryRow(ctx, "SELECT payload_json FROM outbox_events WHERE aggregate_id = $1 AND event_type = 'operation.requested'", id).Scan(&command); err != nil {
		t.Fatalf("load Workspace operation Outbox command %s: %v", id, err)
	}
	resultsQueue := &fakeOperationResultsQueue{}
	controller, err := resourcecontroller.New("rp_test", runtime, resultsQueue, nil)
	if err != nil {
		t.Fatalf("create Fake Resource Controller: %v", err)
	}
	if err := controller.Handle(ctx, command); err != nil {
		t.Fatalf("execute Workspace operation %s in Fake Runtime: %v", id, err)
	}
	consumer, err := operationresults.New(operationresults.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, resultsQueue, operationresults.Config{BatchSize: 1, WaitTime: time.Second})
	if err != nil {
		t.Fatalf("create Operation result consumer: %v", err)
	}
	stats, err := consumer.RunOnce(ctx)
	if err != nil || stats.Applied != 1 {
		t.Fatalf("consume Fake Runtime result for %s: stats=%+v err=%v", id, stats, err)
	}
}

func assertWorkspaceObservedState(t *testing.T, handler http.Handler, token, tenantID string, workspaceID domain.WorkspaceID, want domain.ObservedWorkspaceState) {
	t.Helper()
	response := requestWorkspaceGet(t, handler, token, tenantID, string(workspaceID))
	var result workspaces.View
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &result) != nil || result.Status.ObservedState != want {
		t.Fatalf("Workspace observed state: got HTTP %d body=%s, want %s", response.Code, response.Body.String(), want)
	}
}

type fakeOperationResultsQueue struct {
	deliveries []operationresults.Delivery
	next       int
}

func (q *fakeOperationResultsQueue) Report(_ context.Context, result resourcecontroller.Result) error {
	encoded, err := json.Marshal(result)
	if err != nil {
		return err
	}
	q.next++
	q.deliveries = append(q.deliveries, operationresults.Delivery{Body: encoded, ReceiptHandle: fmt.Sprintf("result-%d", q.next)})
	return nil
}

func (q *fakeOperationResultsQueue) Receive(context.Context, int, time.Duration) ([]operationresults.Delivery, error) {
	return append([]operationresults.Delivery(nil), q.deliveries...), nil
}

func (q *fakeOperationResultsQueue) Delete(_ context.Context, receipt string) error {
	for i := range q.deliveries {
		if q.deliveries[i].ReceiptHandle == receipt {
			q.deliveries = append(q.deliveries[:i], q.deliveries[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("result receipt %q not found", receipt)
}

func signTenantAPIToken(t *testing.T, privateKey *rsa.PrivateKey, issuer, subject, scope string) string {
	return signAPITokenWithGroups(t, privateKey, issuer, subject, scope, nil)
}

func signAPITokenWithGroups(t *testing.T, privateKey *rsa.PrivateKey, issuer, subject, scope string, groups []string) string {
	t.Helper()
	claims := auth.CognitoAccessClaims{
		ClientID: "hako-cli", TokenUse: "access", Scope: scope, Groups: groups,
		RegisteredClaims: jwt.RegisteredClaims{
			Issuer: issuer, Subject: subject, IssuedAt: jwt.NewNumericDate(time.Now()),
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, claims)
	token.Header["kid"] = "tenant-api-key"
	signed, err := token.SignedString(privateKey)
	if err != nil {
		t.Fatalf("sign API token: %v", err)
	}
	return signed
}
