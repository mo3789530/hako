package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/deploypolicy"
)

func TestSoftwareCatalogDeployPolicyClientShowsAndSetsDormantPolicy(t *testing.T) {
	policy := deploypolicy.Policy{Enabled: true, Audience: deploypolicy.AWSSTSAudience, AllowedRefs: []string{"refs/heads/main"},
		Environments: []string{"production"}, AllowedWorkflowRefs: []string{"acme/payments/.github/workflows/deploy.yml@refs/heads/main"}, RequireHumanApproval: true}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		if r.URL.Path != "/v1/tenants/tenant_a/software-catalog/payments-api/deploy-policy" {
			t.Errorf("path = %q", r.URL.Path)
		}
		if r.Method == http.MethodPut {
			var got deploypolicy.Policy
			if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got.Validate() != nil || got.Audience != policy.Audience {
				t.Errorf("PUT policy = %+v err=%v", got, err)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"service_id":"payments-api","policy":{"enabled":true,"audience":"sts.amazonaws.com","allowed_refs":["refs/heads/main"],"environments":["production"],"allowed_workflow_refs":["acme/payments/.github/workflows/deploy.yml@refs/heads/main"],"require_human_approval":true},"execution_enabled":false}`)
	}))
	defer server.Close()
	for _, call := range []func() (SoftwareCatalogDeployPolicyResult, error){
		func() (SoftwareCatalogDeployPolicyResult, error) {
			return GetSoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", server.Client())
		},
		func() (SoftwareCatalogDeployPolicyResult, error) {
			return SetSoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", policy, server.Client())
		},
	} {
		result, err := call()
		if err != nil || result.ExecutionEnabled || result.Policy.Validate() != nil {
			t.Fatalf("deploy policy API call = %+v, error=%v", result, err)
		}
	}
}

func TestSoftwareCatalogDeployPolicyClientRejectsUnsafeResponses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"service_id":"payments-api","policy":{"enabled":false},"execution_enabled":true}`)
	}))
	defer server.Close()
	if _, err := GetSoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", server.Client()); err == nil {
		t.Fatal("client accepted an execution-enabled response")
	}
	if _, err := SetSoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", deploypolicy.Policy{Enabled: true}, server.Client()); err == nil {
		t.Fatal("client accepted an invalid policy")
	}
}

func TestVerifySoftwareCatalogDeployPolicyClientKeepsExecutionDisabled(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/tenants/tenant_a/software-catalog/payments-api/deploy-policy/verify" {
			t.Errorf("request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		var body struct {
			Token string `json:"token"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Token != "header.payload.signature" {
			t.Errorf("verification body token = %q, error=%v", body.Token, err)
		}
		_, _ = fmt.Fprint(w, `{"valid":true,"execution_enabled":false,"repository":"acme/payments","repository_id":"1234","ref":"refs/heads/main","environment":"production","workflow_ref":"acme/payments/.github/workflows/deploy.yml@refs/heads/main","event_name":"workflow_dispatch","run_id":"7","run_attempt":"1"}`)
	}))
	defer server.Close()
	result, err := VerifySoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", "header.payload.signature", server.Client())
	if err != nil || !result.Valid || result.ExecutionEnabled || result.RepositoryID != "1234" {
		t.Fatalf("VerifySoftwareCatalogDeployPolicy() = %+v, %v", result, err)
	}
	if _, err := VerifySoftwareCatalogDeployPolicy(context.Background(), server.URL, "token", "tenant_a", "payments-api", strings.Repeat("x", 64<<10+1), server.Client()); err == nil {
		t.Fatal("client accepted oversized OIDC token")
	}
}
