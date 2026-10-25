package workspacegateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type artifactResolverFunc func(context.Context, workloadartifactaccess.Claims) (workloadartifactaccess.Grant, error)

func (f artifactResolverFunc) Resolve(ctx context.Context, claims workloadartifactaccess.Claims) (workloadartifactaccess.Grant, error) {
	return f(ctx, claims)
}

type artifactReaderFunc func(context.Context, workloadartifactaccess.Grant) (io.ReadCloser, error)

func (f artifactReaderFunc) Open(ctx context.Context, grant workloadartifactaccess.Grant) (io.ReadCloser, error) {
	return f(ctx, grant)
}

func TestGatewayArtifactDownloadRequiresBoundShortLivedTicketAndStreamsBytes(t *testing.T) {
	key := []byte(strings.Repeat("k", 32))
	data := []byte("private workload artifact")
	digest := sha256.Sum256(data)
	grant := workloadartifactaccess.Grant{StorageRef: "s3://bucket/workloads/tenant/run/artifact", SHA256: hex.EncodeToString(digest[:]),
		SizeBytes: int64(len(data)), Kind: "logs"}
	var resolved int
	resolver := artifactResolverFunc(func(_ context.Context, claims workloadartifactaccess.Claims) (workloadartifactaccess.Grant, error) {
		resolved++
		if claims.ArtifactID != "artifact_1" || claims.TenantID != "tenant_1" || claims.WorkloadRunID != "run_1" {
			t.Errorf("resolver received unbound identity: %+v", claims)
		}
		return grant, nil
	})
	reader := artifactReaderFunc(func(_ context.Context, actual workloadartifactaccess.Grant) (io.ReadCloser, error) {
		if actual != grant {
			t.Errorf("reader grant = %+v, want %+v", actual, grant)
		}
		return io.NopCloser(bytes.NewReader(data)), nil
	})
	gateway, err := New(Config{ResourcePlaneID: domain.ResourcePlaneID("rp_test"), Verifier: &workspacesession.Verifier{},
		Dialer: &testDialer{}, MaxConnections: 2, MaxConnectionsPerWorkspace: 1,
		ArtifactAccess: &ArtifactAccessConfig{TicketKey: key, Resolver: resolver, Reader: reader}})
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewTLSServer(gateway.Handler())
	defer server.Close()
	client := server.Client()

	token, _, err := workloadartifactaccess.Issue(key, workloadartifactaccess.Claims{UserID: "usr_1", TenantID: "tenant_1",
		WorkloadRunID: "run_1", ArtifactID: "artifact_1", ResourcePlane: "rp_test"}, time.Now().UTC(), nil)
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/workload-artifacts/artifact_1", nil)
	request.Header.Set("Authorization", "Bearer "+token)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK || string(body) != string(data) || response.Header.Get("Content-Length") != strconv.Itoa(len(data)) ||
		response.Header.Get("X-Hako-Artifact-SHA256") != grant.SHA256 || response.Header.Get("Cache-Control") != "no-store" || resolved != 1 {
		t.Fatalf("artifact response status=%d headers=%v body=%q resolved=%d", response.StatusCode, response.Header, body, resolved)
	}

	badToken, _, _ := workloadartifactaccess.Issue(key, workloadartifactaccess.Claims{UserID: "usr_1", TenantID: "tenant_1",
		WorkloadRunID: "run_1", ArtifactID: "artifact_other", ResourcePlane: "rp_test"}, time.Now().UTC(), nil)
	badRequest, _ := http.NewRequest(http.MethodGet, server.URL+"/v1/workload-artifacts/artifact_1", nil)
	badRequest.Header.Set("Authorization", "Bearer "+badToken)
	badResponse, err := client.Do(badRequest)
	if err != nil {
		t.Fatal(err)
	}
	_ = badResponse.Body.Close()
	if badResponse.StatusCode != http.StatusUnauthorized || resolved != 1 {
		t.Fatalf("mismatched artifact ticket status=%d resolver calls=%d", badResponse.StatusCode, resolved)
	}

	if !validArtifactGrant(grant) {
		t.Fatal("valid bounded S3 grant was rejected")
	}
	grant.SizeBytes = workloadartifactaccess.MaxArtifactSize + 1
	if validArtifactGrant(grant) {
		t.Fatal("artifact grant above content integrity bound was accepted")
	}
}

func TestGatewayArtifactRouteIsAbsentUnlessConfigured(t *testing.T) {
	gateway, err := New(Config{ResourcePlaneID: "rp_test", Verifier: &workspacesession.Verifier{},
		Dialer: &testDialer{}, MaxConnections: 1, MaxConnectionsPerWorkspace: 1})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	gateway.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/workload-artifacts/artifact_1", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("disabled artifact route returned HTTP %d", response.Code)
	}
}
