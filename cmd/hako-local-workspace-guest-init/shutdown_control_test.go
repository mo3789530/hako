//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestLocalGuestShutdownControlRequiresExactIdentity(t *testing.T) {
	config := guestConfig{TenantID: "tenant_a", WorkspaceID: "workspace_a", ResourcePlaneID: "rp_a", MicroVMID: "fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	shutdown := make(chan struct{}, 1)
	handler := localGuestShutdownHandler(config, shutdown)
	valid, err := json.Marshal(localGuestShutdownIdentity{TenantID: config.TenantID, WorkspaceID: config.WorkspaceID,
		ResourcePlaneID: config.ResourcePlaneID, MicroVMID: config.MicroVMID})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/internal/shutdown", bytes.NewReader(valid))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("valid shutdown status=%d body=%q", response.Code, response.Body.String())
	}
	select {
	case <-shutdown:
	default:
		t.Fatal("valid shutdown did not request Guest stop")
	}

	foreign := localGuestShutdownIdentity{TenantID: "tenant_b", WorkspaceID: config.WorkspaceID,
		ResourcePlaneID: config.ResourcePlaneID, MicroVMID: config.MicroVMID}
	body, _ := json.Marshal(foreign)
	request = httptest.NewRequest(http.MethodPost, "/internal/shutdown", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("foreign shutdown status=%d", response.Code)
	}
	select {
	case <-shutdown:
		t.Fatal("foreign identity requested Guest stop")
	default:
	}
}

func TestLocalGuestShutdownControlRejectsMethodQueryAndUnknownFields(t *testing.T) {
	config := guestConfig{TenantID: "tenant_a", WorkspaceID: "workspace_a", ResourcePlaneID: "rp_a", MicroVMID: "fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}
	handler := localGuestShutdownHandler(config, make(chan struct{}, 1))
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/internal/shutdown", nil),
		httptest.NewRequest(http.MethodPost, "/internal/shutdown?force=true", bytes.NewReader([]byte(`{}`))),
		httptest.NewRequest(http.MethodPost, "/internal/shutdown", bytes.NewReader([]byte(`{"tenant_id":"tenant_a","workspace_id":"workspace_a","resource_plane_id":"rp_a","microvm_id":"fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa","force":true}`))),
	} {
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code < 400 {
			t.Fatalf("unsafe shutdown request accepted: method=%s path=%s status=%d", request.Method, request.URL, response.Code)
		}
	}
}
