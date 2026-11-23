package main

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
)

func TestOpenDatabaseRejectsAmbiguousOrMissingConfiguration(t *testing.T) {
	t.Setenv("HAKO_DATABASE_URL", "postgres://local/db")
	t.Setenv("HAKO_DSQL_HOST", "cluster.dsql.us-east-1.on.aws")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("both database modes should be rejected")
	}
	t.Setenv("HAKO_DATABASE_URL", "")
	t.Setenv("HAKO_DSQL_HOST", "")
	if _, err := openDatabase(context.Background()); err == nil {
		t.Fatal("missing DSQL host should fail validation")
	}
}

func TestShouldStartLambdaRuntime(t *testing.T) {
	tests := []struct {
		name       string
		runtimeAPI string
		httpMode   string
		want       bool
	}{
		{name: "local HTTP", want: false},
		{name: "zip Lambda runtime", runtimeAPI: "127.0.0.1:9001", want: true},
		{name: "Lambda Web Adapter image", runtimeAPI: "127.0.0.1:9001", httpMode: "true", want: false},
		{name: "HTTP mode outside Lambda", httpMode: "true", want: false},
		{name: "whitespace values", runtimeAPI: "  ", httpMode: " true ", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := shouldStartLambdaRuntime(test.runtimeAPI, test.httpMode); got != test.want {
				t.Fatalf("shouldStartLambdaRuntime(%q, %q) = %t, want %t", test.runtimeAPI, test.httpMode, got, test.want)
			}
		})
	}
}

func TestAPITLSFilesRequireCertificateAndPrivateKeyTogether(t *testing.T) {
	t.Setenv("HAKO_API_TLS_CERT_FILE", "")
	t.Setenv("HAKO_API_TLS_KEY_FILE", "")
	if cert, key, err := apiTLSFilesFromEnv(); err != nil || cert != "" || key != "" {
		t.Fatalf("unset TLS files = %q %q %v", cert, key, err)
	}
	t.Setenv("HAKO_API_TLS_CERT_FILE", "/private/gateway-cert.pem")
	if _, _, err := apiTLSFilesFromEnv(); err == nil {
		t.Fatal("partial TLS configuration must be rejected")
	}
	t.Setenv("HAKO_API_TLS_KEY_FILE", "/private/gateway-key.pem")
	if cert, key, err := apiTLSFilesFromEnv(); err != nil || cert == "" || key == "" {
		t.Fatalf("complete TLS files = %q %q %v", cert, key, err)
	}
}

func TestResourcePlaneWorkspaceSessionKeyResolverIsOptionalAndValidatesARNMap(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS", "")
	resolver, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(context.Background())
	if err != nil || resolver != nil {
		t.Fatalf("unset session key map resolver=%v err=%v", resolver, err)
	}
	t.Setenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS", `{"rp_a":"not-an-arn"}`)
	if _, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(context.Background()); err == nil {
		t.Fatal("invalid Resource Plane session Secret map was accepted")
	}
}

func TestLocalWorkspaceSessionKeysConfigureIssuerAndPlaneScopedResolver(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	privatePath := filepath.Join(directory, "session-private.pem")
	privateBytes, err := x509.MarshalPKCS8PrivateKey(privateKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(privatePath, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: privateBytes}), 0600); err != nil {
		t.Fatal(err)
	}
	hmacPath := filepath.Join(directory, "session-hmac.key")
	if err := os.WriteFile(hmacPath, []byte("0123456789abcdef0123456789abcdef"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAKO_LOCAL_WORKSPACE_SESSION_PRIVATE_KEY_FILE", privatePath)
	t.Setenv("HAKO_WORKSPACE_SESSION_KEY_ID", "local_session")
	t.Setenv("HAKO_LOCAL_RESOURCE_PLANE_SESSION_KEY_FILE", hmacPath)
	t.Setenv("HAKO_LOCAL_RESOURCE_PLANE_ID", "rp-local")
	t.Setenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS", "")
	issuer, err := workspaceSessionIssuerFromEnv()
	if err != nil || issuer == nil {
		t.Fatalf("local Workspace Session issuer=%v err=%v", issuer, err)
	}
	resolver, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(context.Background())
	if err != nil || resolver == nil {
		t.Fatalf("local Workspace Session resolver=%v err=%v", resolver, err)
	}
	key, err := resolver(context.Background(), domain.ResourcePlaneID("rp-local"))
	if err != nil || string(key) != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("local Workspace Session key=%q err=%v", key, err)
	}
	if _, err := resolver(context.Background(), domain.ResourcePlaneID("rp-other")); err == nil {
		t.Fatal("local session key resolver must reject another Resource Plane")
	}
}

func TestLocalWorkspaceSessionIssuerRejectsInsecureKeyFiles(t *testing.T) {
	directory := t.TempDir()
	keyPath := filepath.Join(directory, "bad.pem")
	if err := os.WriteFile(keyPath, []byte("not-a-key"), 0644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HAKO_LOCAL_WORKSPACE_SESSION_PRIVATE_KEY_FILE", keyPath)
	t.Setenv("HAKO_WORKSPACE_SESSION_KEY_ID", "local_session")
	if _, err := workspaceSessionIssuerFromEnv(); err == nil {
		t.Fatal("insecure private key file must be rejected")
	}
}

func TestResourcePlaneArtifactCleanupKeyResolverIsOptionalAndValidatesARNMap(t *testing.T) {
	t.Setenv("HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS", "")
	resolver, err := resourcePlaneArtifactCleanupKeyResolverFromEnv(context.Background())
	if err != nil || resolver != nil {
		t.Fatalf("unset cleanup key map resolver=%v err=%v", resolver, err)
	}
	t.Setenv("HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS", `{"rp_a":"not-an-arn"}`)
	if _, err := resourcePlaneArtifactCleanupKeyResolverFromEnv(context.Background()); err == nil {
		t.Fatal("invalid Resource Plane Artifact cleanup Secret map was accepted")
	}
}
