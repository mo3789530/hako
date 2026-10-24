//go:build integration

package githubbroker

import (
	"context"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	storegithubwebhook "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestPostgresAgentIssueContextResolverRevalidatesTenantAndCredentialPins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Microsecond)
	statements := []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_agent_broker','Agent broker',$1),('tenant_agent_foreign','Foreign',$1)`,
		`INSERT INTO users(id,cognito_subject,email,created_at,github_user_id,github_login) VALUES ('usr_agent_broker','agent-broker','',$1,77101,'agent-author'),('usr_agent_foreign','agent-foreign','',$1,77102,'foreign-author')`,
		`INSERT INTO tenant_members(tenant_id,user_id,role,joined_at) VALUES ('tenant_agent_broker','usr_agent_broker','member',$1),('tenant_agent_foreign','usr_agent_foreign','owner',$1)`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id)
		 VALUES ('binding_agent_broker','tenant_agent_broker',77103,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/app-key-AbCdEf','oauth-ref','webhook-ref',$1,'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_agent_broker',77103,'tenant_owned','tenant_agent_broker','agent-app','client',$1)`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_agent_broker','tenant_agent_broker','registration_agent_broker',77103,'binding_agent_broker','active',2,5,'agent-key','agent-hash','usr_agent_broker',$1,$1)`,
		`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at)
		 VALUES ('tenant_agent_broker',77104,'agent-owner','active','usr_agent_broker',$1,$1)`,
		`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (77104,'tenant_agent_broker',$1)`,
		`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_agent_broker','integration_agent_broker',77104,$1)`,
		`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,factory_enabled,factory_enabled_at)
		 VALUES ('tenant_agent_broker',77104,77105,'agent-owner','agent-repo','main',$1::timestamptz,true,$1::timestamptz-interval '1 minute')`,
	}
	for _, query := range statements {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}
	delivery := githubwebhook.VerifiedDelivery{IntegrationID: "integration_agent_broker", CredentialGeneration: 5,
		DeliveryID: "a5b2c3d4-e5f6-4789-8abc-def012345678", Event: "issue_comment", Action: "created", Payload: []byte(`{"action":"created","installation":{"id":77104,"app_id":77103},"repository":{"id":77105,"name":"agent-repo","default_branch":"main","owner":{"login":"agent-owner"}},"issue":{"number":12},"comment":{"id":34,"body":"/hako implement","author_association":"MEMBER"},"sender":{"id":77101,"login":"agent-author"}}`)}
	inbox := storegithubwebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
	if inserted, err := inbox.Record(ctx, delivery, now); err != nil || !inserted {
		t.Fatalf("record Issue comment: inserted=%v error=%v", inserted, err)
	}
	resolver := NewPostgresAgentIssueContextResolver(pool, transaction.DefaultPolicy())
	pin, err := resolver(ctx, delivery.IntegrationID, delivery.DeliveryID)
	if err != nil || pin.Authorization.TenantID != "tenant_agent_broker" || pin.Authorization.UserID != "usr_agent_broker" ||
		pin.Authorization.IssueNumber != 12 || pin.App.GitHubAppID != 77103 || pin.App.ConfigRevision != 2 || pin.App.CredentialGeneration != 5 || pin.App.SecretVersionID != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("resolved Agent issue pin = %+v error=%v", pin, err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM tenant_members WHERE tenant_id='tenant_agent_broker' AND user_id='usr_agent_broker'`); err != nil {
		t.Fatal(err)
	}
	if _, err := resolver(ctx, delivery.IntegrationID, delivery.DeliveryID); err == nil {
		t.Fatal("revoked Tenant membership still resolved Issue context")
	}
}
