package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/softwarecatalog"
)

func TestSoftwareCatalogClientListsAndUpsertsTenantScopedMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer token" {
			t.Errorf("Authorization = %q", r.Header.Get("Authorization"))
		}
		w.Header().Set("Content-Type", "application/json")
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/tenants/tenant_a/software-catalog":
			_, _ = fmt.Fprint(w, `{"items":[{"tenant_id":"tenant_a","service_id":"payments-api","owner_team":"payments","installation_id":12,"github_repository_id":34,"repository":"acme/payments","runtime":"lambda-microvm","language":"go","dependencies":[]}]}`)
		case "PUT /v1/tenants/tenant_a/software-catalog/payments-api":
			var request map[string]any
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request["owner_team"] != "payments" || request["runtime"] != "lambda-microvm" {
				t.Errorf("PUT body = %#v, err=%v", request, err)
			}
			_, _ = fmt.Fprint(w, `{"tenant_id":"tenant_a","service_id":"payments-api","owner_team":"payments","installation_id":12,"github_repository_id":34,"repository":"acme/payments","runtime":"lambda-microvm","language":"go","dependencies":[]}`)
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	listed, err := ListSoftwareCatalog(context.Background(), server.URL, "token", "tenant_a", server.Client())
	if err != nil || len(listed.Items) != 1 || listed.Items[0].Repository != "acme/payments" {
		t.Fatalf("ListSoftwareCatalog() = %+v, error=%v", listed, err)
	}
	item := softwarecatalog.Service{TenantID: "tenant_a", ServiceID: "payments-api", OwnerTeam: "payments", InstallationID: 12,
		GitHubRepositoryID: 34, Runtime: "lambda-microvm", Language: "go", Dependencies: []string{}}
	stored, err := UpsertSoftwareCatalogService(context.Background(), server.URL, "token", item, server.Client())
	if err != nil || stored.Repository != "acme/payments" {
		t.Fatalf("UpsertSoftwareCatalogService() = %+v, error=%v", stored, err)
	}
}

func TestSoftwareCatalogClientRejectsCrossTenantAndOversizedResponse(t *testing.T) {
	for name, response := range map[string]string{
		"cross Tenant":  `{"items":[{"tenant_id":"tenant_b","service_id":"api","owner_team":"team","installation_id":1,"github_repository_id":2,"repository":"acme/api","runtime":"lambda-microvm","language":"go","dependencies":[]}]}`,
		"trailing JSON": `{"items":[]} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = fmt.Fprint(w, response) }))
			defer server.Close()
			if _, err := ListSoftwareCatalog(context.Background(), server.URL, "token", "tenant_a", server.Client()); err == nil {
				t.Fatal("unsafe response was accepted")
			}
		})
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = fmt.Fprint(w, `{"items":[]}`+strings.Repeat(" ", maxAPIResponseSize))
	}))
	defer server.Close()
	if _, err := ListSoftwareCatalog(context.Background(), server.URL, "token", "tenant_a", server.Client()); err == nil {
		t.Fatal("oversized response was accepted")
	}
}
