//go:build integration

package api

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestTenantSetupCallbackUsesMTLSBrokerAndLivePostgresPin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for _, query := range []string{
		`INSERT INTO tenants (id,name,created_at) VALUES ('tenant_broker_callback','Broker callback',$1)`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_broker_callback','broker-callback-sub','',$1)`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_broker_callback','usr_broker_callback','owner',$1)`,
		`INSERT INTO github_credential_bindings (id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id,oauth_reader_role_arn,oauth_secret_version_id)
		 VALUES ('binding_broker_callback','tenant_broker_callback',911,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/private-key-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/oauth-secret-AbCdEf','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/webhook-secret-AbCdEf',$1,'arn:aws:iam::123456789012:role/hako/private-key-reader','0123456789abcdef0123456789abcdef','arn:aws:iam::123456789012:role/hako/oauth-reader','11111111111111111111111111111111')`,
		`INSERT INTO github_app_registrations (id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at)
		 VALUES ('registration_broker_callback',911,'tenant_owned','tenant_broker_callback','broker-callback-app','Iv1.callback',$1)`,
		`INSERT INTO tenant_github_integrations (id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at)
		 VALUES ('integration_broker_callback','tenant_broker_callback','registration_broker_callback',911,'binding_broker_callback','active',4,8,'broker-callback-key','request-hash','usr_broker_callback',$1,$1)`,
	} {
		if _, err := pool.Exec(ctx, query, now); err != nil {
			t.Fatal(err)
		}
	}

	var readCalls int
	pin := githubregistry.SetupAppPin{TenantID: "tenant_broker_callback", IntegrationID: "integration_broker_callback", AppRegistrationID: "registration_broker_callback", GitHubAppID: 911, ConfigRevision: 4, CredentialGeneration: 8}
	identityVerifier := &integrationGitHubSetupVerifier{appID: 911}
	oauth, err := githubbroker.NewOAuthServiceWithVerifierFactory(func(ctx context.Context, got githubregistry.SetupAppPin) error {
		caller, ok := githubbroker.CallerFromContext(ctx)
		if !ok || caller != "spiffe://hako/control-plane" || got != pin {
			return githubbroker.ErrUnavailable
		}
		return nil
	}, func(ctx context.Context, got githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
		return transaction.Within(ctx, pool, transaction.DefaultPolicy(), func(ctx context.Context, tx pgx.Tx) (githubregistry.SetupCredential, error) {
			return githubregistry.ResolveSetupCredential(ctx, tx, got)
		})
	}, func(_ context.Context, credential githubregistry.SetupCredential) (string, error) {
		readCalls++
		if credential.Pin != pin || credential.VersionID != "11111111111111111111111111111111" {
			t.Fatal("broker did not resolve the exact OAuth-purpose credential version")
		}
		return "synthetic-oauth-secret-not-real", nil
	}, "https://hako.example/v1/integrations/github/setup/callback", func(config githubapp.SetupConfig) (githubapp.SetupVerifier, error) {
		if config.AppID != 911 {
			t.Fatalf("broker used wrong App ID %d", config.AppID)
		}
		if config.ClientSecret == "synthetic-oauth-secret-not-real" {
			if config.CallbackURL != "https://hako.example/v1/integrations/github/setup/callback" {
				t.Fatal("broker used an unexpected callback URL")
			}
			return identityVerifier, nil
		}
		if config.ClientSecret == "unused-for-public-URLs" {
			return identityVerifier, nil
		}
		t.Fatal("broker supplied unexpected OAuth credential")
		return nil, githubbroker.ErrUnavailable
	})
	if err != nil {
		t.Fatal(err)
	}

	brokerServerTLS, brokerRoots, clientCertificate := testBrokerCertificates(t)
	server := httptest.NewUnstartedServer(githubbroker.Handler(oauth))
	server.TLS = brokerServerTLS
	server.StartTLS()
	defer server.Close()
	client, err := githubbroker.NewMTLSClient(clientCertificate, brokerRoots)
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := githubbroker.NewHTTPResolver(server.URL, client)
	if err != nil {
		t.Fatal(err)
	}

	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	keys := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"keys": []any{map[string]string{
			"kty": "RSA", "use": "sig", "alg": "RS256", "kid": "tenant-api-key",
			"n": base64.RawURLEncoding.EncodeToString(privateKey.N.Bytes()), "e": base64.RawURLEncoding.EncodeToString([]byte{1, 0, 1}),
		}}})
	}))
	defer keys.Close()
	issuer := keys.URL + "/pool"
	apiVerifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: "hako-cli"})
	if err != nil {
		t.Fatal(err)
	}
	apiHandler := NewHandlerWithGitHubApp(apiVerifier, pool, nil, nil, WorkspaceCreateConfig{
		GitHubAppManagementEnabled: true, TenantGitHubSetupResolver: resolver,
	})
	token := signTenantAPIToken(t, privateKey, issuer, "broker-callback-sub", "openid hako/api")
	start := httptest.NewRequest(http.MethodPost, "/v1/tenants/tenant_broker_callback/github/apps/integration_broker_callback/setup", strings.NewReader(`{}`))
	start.Header.Set("Authorization", "Bearer "+token)
	start.Header.Set("Content-Type", "application/json")
	started := httptest.NewRecorder()
	apiHandler.ServeHTTP(started, start)
	if started.Code != http.StatusCreated || strings.Contains(started.Body.String(), "synthetic-oauth-secret-not-real") || readCalls != 0 {
		t.Fatalf("start setup over mTLS = HTTP %d reads=%d body=%s", started.Code, readCalls, started.Body.String())
	}
	var setup struct {
		SetupURL string `json:"setup_url"`
	}
	if err := json.Unmarshal(started.Body.Bytes(), &setup); err != nil {
		t.Fatal(err)
	}
	setupURL, err := url.Parse(setup.SetupURL)
	if err != nil || setupURL.Query().Get("state") == "" {
		t.Fatalf("setup URL=%q err=%v", setup.SetupURL, err)
	}
	state := setupURL.Query().Get("state")
	installation := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?installation_id=9911&setup_action=install&state="+url.QueryEscape(state), nil)
	redirect := httptest.NewRecorder()
	apiHandler.ServeHTTP(redirect, installation)
	if redirect.Code != http.StatusFound || !strings.Contains(redirect.Header().Get("Location"), "code_challenge_method=S256") || readCalls != 0 {
		t.Fatalf("OAuth redirect over broker = HTTP %d reads=%d location=%q", redirect.Code, readCalls, redirect.Header().Get("Location"))
	}
	callback := httptest.NewRequest(http.MethodGet, "/v1/integrations/github/setup/callback?code=valid-code&state="+url.QueryEscape(state), nil)
	completed := httptest.NewRecorder()
	apiHandler.ServeHTTP(completed, callback)
	if completed.Code != http.StatusOK || readCalls != 1 || identityVerifier.verifiedInstallationID != 9911 || len(identityVerifier.verifiedCodeVerifier) < 43 ||
		strings.Contains(completed.Body.String(), "synthetic-oauth-secret-not-real") {
		t.Fatalf("OAuth callback over broker = HTTP %d reads=%d body=%s", completed.Code, readCalls, completed.Body.String())
	}
	var active string
	if err := pool.QueryRow(ctx, `SELECT status FROM tenant_github_installations WHERE tenant_id='tenant_broker_callback' AND installation_id=9911`).Scan(&active); err != nil || active != "active" {
		t.Fatalf("verified Installation status=%q err=%v", active, err)
	}
}

func testBrokerCertificates(t *testing.T) (*tls.Config, *x509.CertPool, tls.Certificate) {
	t.Helper()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	rootTemplate := &x509.Certificate{SerialNumber: big.NewInt(100), Subject: pkix.Name{CommonName: "broker integration CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
	rootDER, err := x509.CreateCertificate(rand.Reader, rootTemplate, rootTemplate, rootPublic, rootPrivate)
	if err != nil {
		t.Fatal(err)
	}
	root, err := x509.ParseCertificate(rootDER)
	if err != nil {
		t.Fatal(err)
	}
	issue := func(serial int64, uri string, server bool) tls.Certificate {
		publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		var uris []*url.URL
		if uri != "" {
			parsed, err := url.Parse(uri)
			if err != nil {
				t.Fatal(err)
			}
			uris = []*url.URL{parsed}
		}
		extUsage := x509.ExtKeyUsageClientAuth
		dnsNames := []string(nil)
		ips := []net.IP(nil)
		if server {
			extUsage, dnsNames = x509.ExtKeyUsageServerAuth, []string{"localhost"}
			ips = []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")}
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "hako integration workload"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{extUsage}, URIs: uris, DNSNames: dnsNames, IPAddresses: ips}
		der, err := x509.CreateCertificate(rand.Reader, template, root, publicKey, rootPrivate)
		if err != nil {
			t.Fatal(err)
		}
		leaf, err := x509.ParseCertificate(der)
		if err != nil {
			t.Fatal(err)
		}
		return tls.Certificate{Certificate: [][]byte{der, rootDER}, PrivateKey: privateKey, Leaf: leaf}
	}
	serverCertificate := issue(101, "", true)
	clientCertificate := issue(102, "spiffe://hako/control-plane", false)
	clientCAs := x509.NewCertPool()
	clientCAs.AddCert(root)
	serverTLS, err := githubbroker.ServerTLSConfig(clientCAs)
	if err != nil {
		t.Fatal(err)
	}
	serverTLS.Certificates = []tls.Certificate{serverCertificate}
	serverRoots := x509.NewCertPool()
	serverRoots.AddCert(root)
	return serverTLS, serverRoots, clientCertificate
}
