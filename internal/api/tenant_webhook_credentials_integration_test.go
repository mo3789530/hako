//go:build integration

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantWebhookCredentialResolverUsesPinnedTenantWebhookSecretOnly(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedTenantWebhookResolver(t, ctx, pool)

	var assumedRoleCalls, secretCalls int
	secretServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("X-Amz-Target") {
		case "secretsmanager.GetSecretValue":
			var request struct {
				SecretID  string `json:"SecretId"`
				VersionID string `json:"VersionId"`
				Stage     string `json:"VersionStage"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode synthetic Secrets Manager request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("Authorization") == "" || !strings.Contains(r.Header.Get("Authorization"), "Credential=ASIAHAKOSYNTHETIC/") {
				t.Errorf("Secrets Manager request did not use synthetic assumed credentials: %q", r.Header.Get("Authorization"))
			}
			if request.SecretID != "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook" || request.Stage != "" {
				t.Errorf("secret request escaped exact ARN/version pin: %+v", request)
			}
			value := map[string]string{
				"11111111111111111111111111111111": "synthetic-current-webhook-secret",
				"22222222222222222222222222222222": "synthetic-previous-webhook-secret",
			}[request.VersionID]
			if value == "" {
				t.Errorf("unexpected webhook credential version %q", request.VersionID)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			secretCalls++
			w.Header().Set("Content-Type", "application/x-amz-json-1.1")
			_ = json.NewEncoder(w).Encode(map[string]string{
				"ARN": request.SecretID, "Name": "synthetic-webhook", "VersionId": request.VersionID, "SecretString": value,
			})
		default:
			if err := r.ParseForm(); err != nil || r.Form.Get("Action") != "AssumeRole" {
				t.Errorf("unexpected synthetic AWS request: target=%q body=%q", r.Header.Get("X-Amz-Target"), r.URL.Path)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Form.Get("RoleArn") != "arn:aws:iam::123456789012:role/hako/tenant-webhook-reader" || r.Form.Get("DurationSeconds") != "900" {
				t.Errorf("STS assumed wrong role or session duration: %v", r.Form)
			}
			var policy struct {
				Statement []struct {
					Action   []string `json:"Action"`
					Resource []string `json:"Resource"`
				} `json:"Statement"`
			}
			if err := json.Unmarshal([]byte(r.Form.Get("Policy")), &policy); err != nil || len(policy.Statement) != 1 ||
				len(policy.Statement[0].Action) != 1 || policy.Statement[0].Action[0] != "secretsmanager:GetSecretValue" ||
				len(policy.Statement[0].Resource) != 1 || policy.Statement[0].Resource[0] != "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook" {
				t.Errorf("STS session policy was not exact-purpose/exact-secret: %q", r.Form.Get("Policy"))
			}
			assumedRoleCalls++
			w.Header().Set("Content-Type", "text/xml")
			_, _ = fmt.Fprint(w, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>ASIAHAKOSYNTHETIC</AccessKeyId><SecretAccessKey>synthetic-secret-key</SecretAccessKey><SessionToken>synthetic-session-token</SessionToken><Expiration>`+time.Now().Add(10*time.Minute).UTC().Format(time.RFC3339)+`</Expiration></Credentials></AssumeRoleResult><ResponseMetadata><RequestId>synthetic-request</RequestId></ResponseMetadata></AssumeRoleResponse>`)
		}
	}))
	defer secretServer.Close()

	resolver := TenantGitHubWebhookCredentialResolver{Pool: pool, Policy: transaction.DefaultPolicy(), Config: aws.Config{
		Region:      "ap-northeast-1",
		Credentials: credentials.NewStaticCredentialsProvider("LOCALTEST", "local-test-secret", ""),
		EndpointResolverWithOptions: aws.EndpointResolverWithOptionsFunc(func(_, _ string, _ ...interface{}) (aws.Endpoint, error) {
			return aws.Endpoint{URL: secretServer.URL, HostnameImmutable: true}, nil
		}),
	}}
	credential, err := resolver.ResolveWebhookCredential(ctx, "integration_webhook_resolver")
	if err != nil {
		t.Fatalf("ResolveWebhookCredential(): %v", err)
	}
	if credential.IntegrationID != "integration_webhook_resolver" || credential.Generation != 6 ||
		string(credential.CurrentSecret) != "synthetic-current-webhook-secret" ||
		string(credential.PreviousSecret) != "synthetic-previous-webhook-secret" ||
		credential.PreviousUntil.IsZero() || assumedRoleCalls != 2 || secretCalls != 2 {
		t.Fatalf("resolved unexpected Webhook credential: integration=%q generation=%d previous_until=%v calls=%d/%d", credential.IntegrationID, credential.Generation, credential.PreviousUntil, assumedRoleCalls, secretCalls)
	}

	if _, err := (TenantGitHubWebhookCredentialResolver{Pool: pool}).ResolveWebhookCredential(ctx, "unknown_integration"); err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("unknown integration did not fail closed with a redacted error: %v", err)
	}
	if _, err := (TenantGitHubWebhookCredentialResolver{}).ResolveWebhookCredential(ctx, "integration_webhook_resolver"); err == nil {
		t.Fatal("nil database pool accepted Webhook credential resolution")
	}
}

func TestTenantWebhookAttemptLimiterUsesDatabaseAdmission(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	seedTenantWebhookResolver(t, ctx, pool)

	limiter := TenantGitHubWebhookAttemptLimiter{Pool: pool, Policy: transaction.DefaultPolicy(), MaxAttempts: 1}
	now := time.Now().UTC().Truncate(time.Microsecond)
	if allowed, err := limiter.AllowWebhookAttempt(ctx, "integration_webhook_resolver", now); err != nil || !allowed {
		t.Fatalf("first active integration attempt allowed=%v err=%v", allowed, err)
	}
	if allowed, err := limiter.AllowWebhookAttempt(ctx, "integration_webhook_resolver", now); err != nil || allowed {
		t.Fatalf("second attempt exceeded limiter: allowed=%v err=%v", allowed, err)
	}
	if allowed, err := limiter.AllowWebhookAttempt(ctx, "unknown_integration", now); err != nil || allowed {
		t.Fatalf("unknown integration passed limiter: allowed=%v err=%v", allowed, err)
	}
}

func seedTenantWebhookResolver(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()
	for _, query := range []string{
		`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_webhook_resolver','Webhook resolver',now())`,
		`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_webhook_resolver','webhook-resolver','',now())`,
		`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,webhook_secret_reader_role_arn,webhook_secret_version_id,previous_webhook_secret_version_id,previous_webhook_secret_valid_until) VALUES ('binding_webhook_resolver','tenant_webhook_resolver',911,'approved','private-purpose-only','oauth-purpose-only','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook',now(),'arn:aws:iam::123456789012:role/hako/tenant-webhook-reader','11111111111111111111111111111111','22222222222222222222222222222222',now()+interval '5 minutes')`,
		`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_webhook_resolver',911,'tenant_owned','tenant_webhook_resolver','webhook-app','client',now())`,
		`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_webhook_resolver','tenant_webhook_resolver','registration_webhook_resolver',911,'binding_webhook_resolver','active',3,6,'webhook-resolver-key','webhook-resolver-hash','usr_webhook_resolver',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}
}
