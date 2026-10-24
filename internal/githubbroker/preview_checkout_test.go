package githubbroker

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

func previewCheckoutFixture() (PreviewCheckoutIdentity, githubapp.TenantPrivateKeyAccess) {
	identity := PreviewCheckoutIdentity{TenantID: "tenant_preview", InstallationID: 123, RepositoryID: 456,
		PullRequest: 7, Generation: 2, ClaimID: "claim_preview_123", ClaimAttempt: 3,
		ResourcePlaneID: "rp-test", HeadSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	access := githubapp.TenantPrivateKeyAccess{TenantID: "tenant_preview", IntegrationID: "integration_preview", GitHubAppID: 789,
		ConfigRevision: 4, CredentialGeneration: 6, ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/private-key-reader",
		SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/private-key-AbCdEf", SecretVersionID: "0123456789abcdef0123456789abcdef"}
	return identity, access
}

func TestPreviewCheckoutServiceUsesClaimBoundTenantAppPin(t *testing.T) {
	now := time.Date(2026, 10, 25, 3, 0, 0, 0, time.UTC)
	identity, access := previewCheckoutFixture()
	resolveCalls, mintCalls := 0, 0
	service, err := NewPreviewCheckoutService([]string{"rp-test"}, func(_ context.Context, caller string, requested PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
		resolveCalls++
		if caller != "spiffe://hako/resource-plane/rp-test" || requested != identity {
			return PreviewCheckoutResolution{}, ErrCheckoutDenied
		}
		return PreviewCheckoutResolution{Identity: identity, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
	}, func(_ context.Context, got PreviewCheckoutIdentity, gotAccess githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		mintCalls++
		if got != identity || gotAccess != access {
			return "", time.Time{}, ErrCheckoutDenied
		}
		return "ghs_preview_checkout_token_value", now.Add(time.Minute), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	service.now = func() time.Time { return now }
	grant, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-test", identity)
	if err != nil || grant.Token() != "ghs_preview_checkout_token_value" || !grant.ExpiresAt().Equal(now.Add(time.Minute)) ||
		grant.Repository() != (githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}) ||
		resolveCalls != 2 || mintCalls != 1 {
		t.Fatalf("claim-bound Preview checkout grant = %v (%v), resolve=%d mint=%d", grant, err, resolveCalls, mintCalls)
	}
}

func TestPreviewCheckoutServiceFailsClosedForWrongPlaneStaleClaimAndMissingTenantPin(t *testing.T) {
	identity, access := previewCheckoutFixture()
	tests := []struct {
		name    string
		caller  string
		resolve PreviewCheckoutResolver
	}{
		{name: "foreign Resource Plane", caller: "spiffe://hako/resource-plane/rp-other", resolve: func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
			return PreviewCheckoutResolution{Identity: identity, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
		}},
		{name: "claim claims a different Resource Plane", caller: "spiffe://hako/resource-plane/rp-test", resolve: func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
			foreign := identity
			foreign.ResourcePlaneID = "rp-other"
			return PreviewCheckoutResolution{Identity: foreign, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
		}},
		{name: "stale generation", caller: "spiffe://hako/resource-plane/rp-test", resolve: func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
			stale := identity
			stale.Generation--
			return PreviewCheckoutResolution{Identity: stale, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
		}},
		{name: "unavailable Tenant pin", caller: "spiffe://hako/resource-plane/rp-test", resolve: func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
			return PreviewCheckoutResolution{}, errors.New("unavailable")
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			mintCalls := 0
			service, err := NewPreviewCheckoutService([]string{"rp-test"}, test.resolve, func(context.Context, PreviewCheckoutIdentity, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
				mintCalls++
				return "ghs_preview_checkout_token_value", time.Now().Add(time.Minute), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Issue(context.Background(), test.caller, identity); !errors.Is(err, ErrCheckoutDenied) || mintCalls != 0 {
				t.Fatalf("unsafe Preview checkout was not rejected before token mint: err=%v mint calls=%d", err, mintCalls)
			}
		})
	}
}

func TestPreviewCheckoutServiceRejectsRotationDuringGitHubTokenMint(t *testing.T) {
	identity, access := previewCheckoutFixture()
	resolveCalls, mintCalls := 0, 0
	rotated := access
	rotated.CredentialGeneration++
	service, err := NewPreviewCheckoutService([]string{"rp-test"}, func(context.Context, string, PreviewCheckoutIdentity) (PreviewCheckoutResolution, error) {
		resolveCalls++
		if resolveCalls == 1 {
			return PreviewCheckoutResolution{Identity: identity, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
		}
		return PreviewCheckoutResolution{Identity: identity, Access: rotated, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "preview-repo"}}, nil
	}, func(context.Context, PreviewCheckoutIdentity, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		mintCalls++
		return "ghs_preview_checkout_token_value", time.Now().Add(time.Minute), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Issue(context.Background(), "spiffe://hako/resource-plane/rp-test", identity); !errors.Is(err, ErrCheckoutDenied) || resolveCalls != 2 || mintCalls != 1 {
		t.Fatalf("credential rotation during mint was not fenced: err=%v resolve=%d mint=%d", err, resolveCalls, mintCalls)
	}
}

func TestPreviewCheckoutIdentityValidation(t *testing.T) {
	identity, _ := previewCheckoutFixture()
	for name, mutate := range map[string]func(*PreviewCheckoutIdentity){
		"missing tenant":         func(v *PreviewCheckoutIdentity) { v.TenantID = "" },
		"missing claim":          func(v *PreviewCheckoutIdentity) { v.ClaimID = "" },
		"invalid attempt":        func(v *PreviewCheckoutIdentity) { v.ClaimAttempt = 0 },
		"invalid head":           func(v *PreviewCheckoutIdentity) { v.HeadSHA = "not-a-sha" },
		"invalid resource plane": func(v *PreviewCheckoutIdentity) { v.ResourcePlaneID = domain.ResourcePlaneID("bad plane") },
	} {
		t.Run(name, func(t *testing.T) {
			mutate(&identity)
			if validPreviewCheckoutIdentity(identity) {
				t.Fatal("invalid Preview checkout identity was accepted")
			}
		})
		identity, _ = previewCheckoutFixture()
	}
}

func TestHTTPPreviewCheckoutProviderPinsRequestAndReturnsRepository(t *testing.T) {
	identity, _ := previewCheckoutFixture()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != previewCheckoutRoute || r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("Preview checkout request = %s %s headers=%v", r.Method, r.URL.Path, r.Header)
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 2049))
		if err != nil || len(body) > 2048 {
			t.Errorf("read Preview checkout body: length=%d err=%v", len(body), err)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(body, &fields); err != nil || len(fields) != 8 {
			t.Errorf("Preview checkout body fields = %v, err=%v", fields, err)
		}
		if _, found := fields["resource_plane_id"]; found {
			t.Error("caller supplied Resource Plane identity instead of mTLS identity")
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response{Token: "ghs_preview_checkout_token_value", ExpiresAt: time.Now().Add(time.Minute),
			Repository: &repositoryResponse{Owner: "preview-owner", Name: "preview-repo"}})
	}))
	defer server.Close()
	provider, err := NewHTTPPreviewCheckoutProvider(server.URL, server.Client())
	if err != nil {
		t.Fatal(err)
	}
	grant, err := provider.CheckoutPreviewToken(context.Background(), identity)
	if err != nil || grant.Token() != "ghs_preview_checkout_token_value" || grant.Repository() != (githubapp.RepositoryAccess{Owner: "preview-owner", Name: "preview-repo"}) {
		t.Fatalf("HTTP Preview checkout token = %v (%v), repository=%+v", grant, err, grant.Repository())
	}
	if strings.Contains(grant.String(), grant.Token()) {
		t.Fatal("Preview checkout grant String exposed the token")
	}
}
