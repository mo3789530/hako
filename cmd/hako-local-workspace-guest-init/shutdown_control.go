//go:build linux

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
)

type localGuestShutdownIdentity struct {
	TenantID        string `json:"tenant_id"`
	WorkspaceID     string `json:"workspace_id"`
	ResourcePlaneID string `json:"resource_plane_id"`
	MicroVMID       string `json:"microvm_id"`
}

func localGuestShutdownHandler(config guestConfig, shutdown chan<- struct{}) http.Handler {
	expected := localGuestShutdownIdentity{TenantID: config.TenantID, WorkspaceID: config.WorkspaceID,
		ResourcePlaneID: config.ResourcePlaneID, MicroVMID: config.MicroVMID}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/internal/shutdown" || r.URL.RawQuery != "" ||
			r.URL.ForceQuery || r.ContentLength < 1 || r.ContentLength > 4096 ||
			r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "invalid Guest shutdown request", http.StatusBadRequest)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4097))
		if err != nil || len(body) == 0 || len(body) > 4096 {
			http.Error(w, "invalid Guest shutdown request", http.StatusBadRequest)
			return
		}
		decoder := json.NewDecoder(bytes.NewReader(body))
		decoder.DisallowUnknownFields()
		var identity localGuestShutdownIdentity
		if decoder.Decode(&identity) != nil || decoder.Decode(new(any)) != io.EOF || identity != expected {
			http.Error(w, "invalid Guest shutdown identity", http.StatusForbidden)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case shutdown <- struct{}{}:
		default:
		}
	})
}
