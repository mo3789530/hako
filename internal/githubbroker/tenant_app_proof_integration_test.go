//go:build integration

package githubbroker

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantAppProofMTLSRouteResolvesExactTenantCredentialFromPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_broker_proof','Proof',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_broker_proof','proof','',now())`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('proof_binding_broker','tenant_broker_proof',123,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:proof-key','oauth-ref','webhook-ref',now(),'arn:aws:iam::123456789012:role/hako/tenant-app-key-reader','0123456789abcdef0123456789abcdef')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('proof_registration_broker',123,'tenant_owned','tenant_broker_proof','proof-app','client',now())`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('gint_broker_proof','tenant_broker_proof','proof_registration_broker',123,'proof_binding_broker','draft',4,7,'proof-key','request-hash','usr_broker_proof',now(),now())`,
	} {
		if _, err := pool.Exec(ctx, query); err != nil {
			t.Fatal(err)
		}
	}

	verifiedAccess := 0
	service, err := NewTenantAppProofService(NewPostgresTenantAppProofResolver(pool, transaction.DefaultPolicy()),
		func(_ context.Context, access githubapp.TenantPrivateKeyAccess) error {
			verifiedAccess++
			if access.TenantID != "tenant_broker_proof" || access.IntegrationID != "gint_broker_proof" || access.GitHubAppID != 123 ||
				access.ConfigRevision != 4 || access.CredentialGeneration != 7 || access.ReaderRoleARN != "arn:aws:iam::123456789012:role/hako/tenant-app-key-reader" ||
				access.SecretARN != "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:proof-key" || access.SecretVersionID != "0123456789abcdef0123456789abcdef" {
				t.Errorf("Broker resolved unexpected private-key identity: %+v", access)
			}
			return nil
		})
	if err != nil {
		t.Fatal(err)
	}
	broker := WithTenantAppProof(http.NotFoundHandler(), service)
	client := &http.Client{Transport: tenantProofCallerTransport{handler: broker}}
	verifier, err := NewHTTPTenantAppProofVerifier("https://broker.internal", client)
	if err != nil {
		t.Fatal(err)
	}
	credential := githubregistry.AppProofCredential{TenantID: domain.TenantID("tenant_broker_proof"), IntegrationID: "gint_broker_proof",
		GitHubAppID: 123, ConfigRevision: 4, CredentialGeneration: 7,
		PrivateKeySecretARN: "caller-must-not-select-secret", PrivateKeyReaderRoleARN: "caller-must-not-select-role", PrivateKeyVersionID: "caller-must-not-select-version"}
	if err := verifier.VerifyApp(ctx, credential); err != nil || verifiedAccess != 1 {
		t.Fatalf("valid proof result: verified=%d error=%v", verifiedAccess, err)
	}
	credential.CredentialGeneration++
	if err := verifier.VerifyApp(ctx, credential); err == nil || verifiedAccess != 1 {
		t.Fatalf("stale generation proof reached credential reader: verified=%d error=%v", verifiedAccess, err)
	}
}

type tenantProofCallerTransport struct{ handler http.Handler }

func (t tenantProofCallerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	copy := request.Clone(request.Context())
	cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/control-plane"}}}
	copy.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, copy)
	return recorder.Result(), nil
}
