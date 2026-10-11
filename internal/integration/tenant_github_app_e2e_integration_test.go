//go:build integration

package integration_test

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/awslabs/aurora-dsql-connectors/go/pgx/occretry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	storewebhook "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantGitHubAppTwoTenantSetupWebhookRunCheckoutE2E(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	statements := []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_e2e_a','Tenant A',$1),('tenant_e2e_b','Tenant B',$1),('tenant_e2e_shared','Legacy shared',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_e2e_a','e2e-a','',$1),('usr_e2e_b','e2e-b','',$1),('usr_e2e_shared','e2e-shared','',$1)`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_e2e_a','usr_e2e_a','owner',$1),('tenant_e2e_b','usr_e2e_b','owner',$1)`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id,oauth_reader_role_arn,oauth_secret_version_id,webhook_secret_reader_role_arn,webhook_secret_version_id)
		 VALUES ('binding_e2e_a','tenant_e2e_a',801,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/private-a-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/oauth-a-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/webhook-a-AbCdEf',$1,'arn:aws:iam::123456789012:role/hako/e2e/private-a','11111111111111111111111111111111','arn:aws:iam::123456789012:role/hako/e2e/oauth-a','22222222222222222222222222222222','arn:aws:iam::123456789012:role/hako/e2e/webhook-a','33333333333333333333333333333333'),
		       ('binding_e2e_b','tenant_e2e_b',802,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/private-b-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/oauth-b-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:e2e/webhook-b-AbCdEf',$1,'arn:aws:iam::123456789012:role/hako/e2e/private-b','44444444444444444444444444444444','arn:aws:iam::123456789012:role/hako/e2e/oauth-b','55555555555555555555555555555555','arn:aws:iam::123456789012:role/hako/e2e/webhook-b','66666666666666666666666666666666')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES
		 ('registration_e2e_a',801,'tenant_owned','tenant_e2e_a','e2e-app-a','client-a',$1),('registration_e2e_b',802,'tenant_owned','tenant_e2e_b','e2e-app-b','client-b',$1)`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES
		 ('integration_e2e_a','tenant_e2e_a','registration_e2e_a',801,'binding_e2e_a','active',4,9,'e2e-key-a','e2e-hash-a','usr_e2e_a',$1,$1),
		 ('integration_e2e_b','tenant_e2e_b','registration_e2e_b',802,'binding_e2e_b','active',7,12,'e2e-key-b','e2e-hash-b','usr_e2e_b',$1,$1)`,
		`INSERT INTO tenant_github_default_integrations(tenant_id,integration_id,revision) VALUES ('tenant_e2e_a','integration_e2e_a',4),('tenant_e2e_b','integration_e2e_b',7)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,github_app_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_e2e_shared',8301,700,'legacy-shared','active','usr_e2e_shared',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8301,'tenant_e2e_shared',$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES ('tenant_e2e_shared',8301,8302,'legacy-shared','legacy-repo','main',$1)`,
	}
	for _, query := range statements {
		var err error
		if strings.Contains(query, "$1") {
			_, err = pool.Exec(ctx, query, now)
		} else {
			_, err = pool.Exec(ctx, query)
		}
		if err != nil {
			t.Fatal(err)
		}
	}

	tenants := []e2eTenantFixture{
		{tenant: "tenant_e2e_a", user: "usr_e2e_a", integration: "integration_e2e_a", rawState: "setup-state-tenant-a", account: "acme-a", secret: "tenant-a-webhook-secret", appID: 801, installationID: 8101, repositoryID: 8201, revision: 4, generation: 9},
		{tenant: "tenant_e2e_b", user: "usr_e2e_b", integration: "integration_e2e_b", rawState: "setup-state-tenant-b", account: "acme-b", secret: "tenant-b-webhook-secret", appID: 802, installationID: 8102, repositoryID: 8202, revision: 7, generation: 12},
	}
	completeSetup := func(tenant e2eTenantFixture, beginPool transaction.Beginner) error {
		stateHash := githubregistry.HashSetupState(tenant.rawState)
		if _, err := transaction.Within(ctx, beginPool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.Installation, error) {
			pin, err := githubregistry.ResolveSetupAppPin(ctx, tx, domain.TenantID(tenant.tenant), tenant.integration)
			if err != nil {
				return githubregistry.Installation{}, err
			}
			if pin.GitHubAppID != tenant.appID || pin.ConfigRevision != tenant.revision || pin.CredentialGeneration != tenant.generation {
				return githubregistry.Installation{}, errors.New("setup resolved an unexpected integration pin")
			}
			if err := githubregistry.CreateTenantAppSetupState(ctx, tx, pin, domain.UserID(tenant.user), stateHash, strings.Repeat("v", 43), now, now.Add(10*time.Minute)); err != nil {
				return githubregistry.Installation{}, err
			}
			if err := githubregistry.SetSetupInstallation(ctx, tx, stateHash, tenant.installationID, now); err != nil {
				return githubregistry.Installation{}, err
			}
			return githubregistry.CompleteSetupState(ctx, tx, stateHash, tenant.installationID, tenant.account, now, tenant.appID)
		}); err != nil {
			return err
		}
		return nil
	}
	var callbackGroup sync.WaitGroup
	callbackErrors := make(chan error, len(tenants))
	retryPool := &retryOnceBeginner{inner: pool}
	for _, tenant := range tenants {
		callbackGroup.Add(1)
		go func(tenant e2eTenantFixture) {
			defer callbackGroup.Done()
			var beginPool transaction.Beginner = pool
			if tenant.tenant == "tenant_e2e_a" {
				beginPool = retryPool
			}
			if err := completeSetup(tenant, beginPool); err != nil {
				callbackErrors <- fmt.Errorf("verified setup callback for %s: %w", tenant.tenant, err)
			}
		}(tenant)
	}
	callbackGroup.Wait()
	close(callbackErrors)
	for err := range callbackErrors {
		t.Fatal(err)
	}
	retryPool.mu.Lock()
	retries := retryPool.tries
	retryPool.mu.Unlock()
	if retries != 2 {
		t.Fatalf("transient setup database conflict was not retried: Begin attempts=%d", retries)
	}

	policy := transaction.DefaultPolicy()
	authorizer := githubregistry.CredentialAuthorizer{Pool: pool, Policy: policy}
	inbox := storewebhook.Inbox{Pool: pool, Policy: policy}
	webhookTenants := make([]testTenantWebhook, 0, len(tenants))
	for _, tenant := range tenants {
		webhookTenants = append(webhookTenants, testTenantWebhook{integration: tenant.integration, secret: tenant.secret, appID: tenant.appID, generation: tenant.generation})
	}
	resolver := e2eTenantWebhookResolver{tenants: webhookTenants}
	webhookHandler, err := githubwebhook.NewTenantHTTPHandler(resolver, authorizer, inbox, githubwebhook.DefaultMaxBodyBytes)
	if err != nil {
		t.Fatal(err)
	}
	for _, tenant := range tenants {
		payload := e2eInstallationRepositoriesPayload(tenant, tenant.repositoryID)
		response := e2eSendTenantWebhook(webhookHandler, tenant.integration, tenant.secret, "8aeebf79-6a2b-42ab-b840-"+strings.Repeat("a", 12), payload)
		if response.Code != http.StatusAccepted || strings.Contains(response.Body.String(), tenant.secret) {
			t.Fatalf("Tenant webhook was not safely accepted for %s: HTTP %d %s", tenant.tenant, response.Code, response.Body.String())
		}
		duplicate := e2eSendTenantWebhook(webhookHandler, tenant.integration, tenant.secret, "8aeebf79-6a2b-42ab-b840-"+strings.Repeat("a", 12), payload)
		if duplicate.Code != http.StatusAccepted || !strings.Contains(duplicate.Body.String(), `"duplicate":true`) {
			t.Fatalf("Tenant webhook replay was not deduplicated for %s: %d %s", tenant.tenant, duplicate.Code, duplicate.Body.String())
		}
		conflicting := e2eSendTenantWebhook(webhookHandler, tenant.integration, tenant.secret, "8aeebf79-6a2b-42ab-b840-"+strings.Repeat("a", 12), e2eInstallationRepositoriesPayload(tenant, tenant.repositoryID+1))
		if conflicting.Code != http.StatusConflict {
			t.Fatalf("same Delivery ID with different body was accepted for %s: HTTP %d", tenant.tenant, conflicting.Code)
		}
		crossApp := tenant
		crossApp.appID++
		wrongApp := e2eSendTenantWebhook(webhookHandler, tenant.integration, tenant.secret, "19aeebf7-6a2b-42ab-b840-"+strings.Repeat("b", 12), e2eInstallationRepositoriesPayload(crossApp, tenant.repositoryID))
		if wrongApp.Code != http.StatusUnauthorized {
			t.Fatalf("different GitHub App event was accepted for %s: HTTP %d", tenant.tenant, wrongApp.Code)
		}
		if _, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (bool, error) {
			return storewebhook.ProcessInstallationRepositoryDeliveryForIntegration(ctx, tx, tenant.integration, "8aeebf79-6a2b-42ab-b840-"+strings.Repeat("a", 12), now)
		}); err != nil {
			t.Fatalf("process authorized Repository event for %s: %v", tenant.tenant, err)
		}
	}

	runs := make([]domain.WorkloadRun, 0, len(tenants))
	for _, tenant := range tenants {
		run, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
			created, _, err := workloads.CreateOrGet(ctx, tx, domain.WorkloadRun{ID: domain.WorkloadRunID("run_" + tenant.tenant), TenantID: domain.TenantID(tenant.tenant), Kind: domain.WorkloadJob,
				RequestedBy: domain.UserID(tenant.user), GitHubInstallation: tenant.installationID, GitHubRepository: tenant.repositoryID,
				CommitSHA: strings.Repeat("a", 40), Ref: "refs/heads/main", RuntimeClass: "standard", TrustLevel: domain.WorkloadTrusted,
				IdempotencyKey: "run-e2e-" + tenant.tenant, CreatedAt: now, TimeoutAt: now.Add(time.Hour)})
			return created, err
		})
		if err != nil || run.GitHubIntegrationID != tenant.integration || run.GitHubConfigRevision != tenant.revision || run.GitHubCredentialGeneration != tenant.generation {
			t.Fatalf("Workload Run pin for %s: %+v %v", tenant.tenant, run, err)
		}
		runs = append(runs, run)
	}
	checkoutResolver := githubbroker.NewPostgresCheckoutRunResolver(pool, policy)
	for index, tenant := range tenants {
		run, access, err := checkoutResolver(ctx, domain.TenantID(tenant.tenant), runs[index].ID)
		if err != nil || run.GitHubIntegrationID != tenant.integration || access.TenantID != tenant.tenant || access.GitHubAppID != tenant.appID || access.ConfigRevision != tenant.revision || access.CredentialGeneration != tenant.generation {
			t.Fatalf("Run-pinned checkout resolver for %s returned %+v (%v)", tenant.tenant, access, err)
		}
		other := tenants[(index+1)%len(tenants)]
		if _, _, err := checkoutResolver(ctx, domain.TenantID(other.tenant), runs[index].ID); !errors.Is(err, githubbroker.ErrCheckoutDenied) {
			t.Fatalf("cross-Tenant checkout for %s was not denied: %v", tenant.tenant, err)
		}
	}
	sharedRun, err := transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.WorkloadRun, error) {
		created, _, err := workloads.CreateOrGet(ctx, tx, domain.WorkloadRun{ID: domain.WorkloadRunID("run_legacy_shared"), TenantID: "tenant_e2e_shared", Kind: domain.WorkloadJob,
			RequestedBy: "usr_e2e_shared", GitHubInstallation: 8301, GitHubRepository: 8302, CommitSHA: strings.Repeat("c", 40),
			RuntimeClass: "standard", TrustLevel: domain.WorkloadTrusted, IdempotencyKey: "run-legacy-shared", CreatedAt: now, TimeoutAt: now.Add(time.Hour)})
		return created, err
	})
	if err != nil || sharedRun.GitHubIntegrationID != "" || sharedRun.GitHubCredentialGeneration != 0 {
		t.Fatalf("shared legacy Run was unexpectedly pinned to a Tenant App: %+v %v", sharedRun, err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=credential_generation+1 WHERE id='integration_e2e_b'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkoutResolver(ctx, "tenant_e2e_b", runs[1].ID); !errors.Is(err, githubbroker.ErrCheckoutDenied) {
		t.Fatalf("stale Run was accepted after credential rotation: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET status='disabled' WHERE id='integration_e2e_a'`); err != nil {
		t.Fatal(err)
	}
	if _, _, err := checkoutResolver(ctx, "tenant_e2e_a", runs[0].ID); !errors.Is(err, githubbroker.ErrCheckoutDenied) {
		t.Fatalf("disabled integration still issued checkout credentials: %v", err)
	}
}

type e2eTenantWebhookResolver struct{ tenants []testTenantWebhook }

type testTenantWebhook struct {
	integration, secret string
	appID, generation   int64
}

func (r e2eTenantWebhookResolver) ResolveWebhookCredential(_ context.Context, integrationID string) (githubwebhook.WebhookCredential, error) {
	for _, tenant := range r.tenants {
		if tenant.integration == integrationID {
			return githubwebhook.WebhookCredential{IntegrationID: tenant.integration, Generation: tenant.generation, CurrentSecret: []byte(tenant.secret)}, nil
		}
	}
	return githubwebhook.WebhookCredential{}, errors.New("unknown integration")
}

type e2eTenantFixture struct {
	tenant, user, integration, rawState, account, secret string
	appID, installationID, repositoryID                  int64
	revision, generation                                 int64
}

type retryOnceBeginner struct {
	inner transaction.Beginner
	mu    sync.Mutex
	tries int
}

func (b *retryOnceBeginner) Begin(ctx context.Context) (pgx.Tx, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tries++
	if b.tries == 1 {
		return nil, &pgconn.PgError{Code: occretry.ErrorCodeMutation, Message: "synthetic transient DSQL serialization conflict"}
	}
	return b.inner.Begin(ctx)
}

func e2eInstallationRepositoriesPayload(tenant e2eTenantFixture, repositoryID int64) []byte {
	return []byte(fmt.Sprintf(`{"action":"added","installation":{"id":%d,"app_id":%d},"repositories_added":[{"id":%d,"name":"repo","owner":{"login":"owner"},"default_branch":"main"}]}`,
		tenant.installationID, tenant.appID, repositoryID))
}

func e2eSendTenantWebhook(handler http.Handler, integrationID, secret, deliveryID string, body []byte) *httptest.ResponseRecorder {
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write(body)
	req := httptest.NewRequest(http.MethodPost, "/v1/integrations/github/apps/"+integrationID+"/webhook", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GitHub-Event", "installation_repositories")
	req.Header.Set("X-GitHub-Delivery", deliveryID)
	req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(mac.Sum(nil)))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	return response
}
