//go:build integration

package api

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5/pgxpool"
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
	handler := NewHandler(verifier, pool, WorkspaceCreateConfig{RequiredCapabilities: []string{"microvm"}, RuntimeClass: "standard", Image: "hako/go:test"})

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
	replayed := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"api-dev"}`, "create-api-dev")
	var replayResult workspaceCreateResponse
	if replayed.Code != http.StatusAccepted || json.Unmarshal(replayed.Body.Bytes(), &replayResult) != nil || replayResult.Workspace.ID != createResult.Workspace.ID {
		t.Fatalf("idempotent create did not return original Workspace: %d %s", replayed.Code, replayed.Body.String())
	}
	var auditCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_events WHERE tenant_id = $1 AND actor_user_id = $2 AND action = 'workspace.create' AND target_id = $3`, "tenant_member", "usr_member", createResult.Workspace.ID).Scan(&auditCount); err != nil || auditCount != 1 {
		t.Fatalf("Workspace create and idempotent replay should produce one actor audit event, count=%d error=%v", auditCount, err)
	}
	oversized := requestCreateWorkspace(t, handler, createToken, "tenant_member", `{"name":"`+strings.Repeat("x", maxJSONRequestBytes)+`"}`, "oversized-create")
	var oversizedError errorResponse
	if oversized.Code != http.StatusRequestEntityTooLarge || json.Unmarshal(oversized.Body.Bytes(), &oversizedError) != nil || oversizedError.Error.Code != "request_too_large" {
		t.Fatalf("oversized request should use the common HTTP 413 error envelope: %d %s", oversized.Code, oversized.Body.String())
	}
	policyToken := signTenantAPIToken(t, privateKey, issuer, "owner-subject", "openid hako/api")
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
	foreignWorkspace, err := workspaces.Create(ctx, pool, workspaces.CreateInput{
		TenantID: "tenant_other", OwnerID: "usr_other", Name: "foreign-dev",
		RuntimeClass: "standard", Image: "hako/go:test", ResourcePlaneID: "rp_test", IdempotencyKey: "foreign-workspace",
	}, transaction.DefaultPolicy())
	if err != nil {
		t.Fatalf("create other Tenant Workspace: %v", err)
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
	runOperationThroughFakeRuntime(t, ctx, pool, deleteResult.OperationID, fakeRuntime)
	assertWorkspaceObservedState(t, handler, createToken, "tenant_member", createResult.Workspace.ID, domain.ObservedWorkspaceDeleted)
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM workspace_quota_slots WHERE workspace_id = $1`, createResult.Workspace.ID).Scan(&quotaSlots); err != nil || quotaSlots != 0 {
		t.Fatalf("observed deletion should release quota: slots=%d error=%v", quotaSlots, err)
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
