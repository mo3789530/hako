package workspaceruntime

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
)

const maxLocalOperationRequestBytes = 64 << 10

// LocalOperationAPI adapts the normal Resource Controller command/result
// contract to the single-host Firecracker runtime. The transport must provide
// local-user authentication (the production local command uses a private Unix
// socket); this handler does not trust caller-selected host paths or images.
type LocalOperationAPI struct {
	resourcePlaneID domain.ResourcePlaneID
	runtime         resourcecontroller.Runtime
	now             func() time.Time
}

func NewLocalOperationAPI(resourcePlaneID domain.ResourcePlaneID, runtime resourcecontroller.Runtime, now func() time.Time) (*LocalOperationAPI, error) {
	if resourcePlaneID == "" || runtime == nil {
		return nil, errors.New("local Resource Plane ID and runtime are required")
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &LocalOperationAPI{resourcePlaneID: resourcePlaneID, runtime: runtime, now: now}, nil
}

func (a *LocalOperationAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if a == nil || r == nil {
		writeLocalOperationError(w, http.StatusInternalServerError, "controller_unavailable")
		return
	}
	if r.URL.Path == "/healthz" {
		if r.Method != http.MethodGet {
			writeLocalOperationError(w, http.StatusMethodNotAllowed, "method_not_allowed")
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"status":"ok"}`)
		return
	}
	if r.URL.Path != "/v1/operations" {
		writeLocalOperationError(w, http.StatusNotFound, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		writeLocalOperationError(w, http.StatusMethodNotAllowed, "method_not_allowed")
		return
	}
	if r.Header.Get("Content-Type") != "application/json" {
		writeLocalOperationError(w, http.StatusUnsupportedMediaType, "unsupported_media_type")
		return
	}
	if r.ContentLength > maxLocalOperationRequestBytes {
		writeLocalOperationError(w, http.StatusRequestEntityTooLarge, "request_too_large")
		return
	}
	payload, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxLocalOperationRequestBytes))
	if err != nil || len(payload) == 0 {
		writeLocalOperationError(w, http.StatusBadRequest, "invalid_operation")
		return
	}
	sink := &localOperationResultSink{}
	controller, err := resourcecontroller.New(a.resourcePlaneID, a.runtime, sink, a.now)
	if err != nil {
		writeLocalOperationError(w, http.StatusInternalServerError, "controller_unavailable")
		return
	}
	if err := controller.Handle(r.Context(), payload); err != nil {
		writeLocalOperationError(w, http.StatusBadRequest, "invalid_operation")
		return
	}
	if sink.result == nil {
		writeLocalOperationError(w, http.StatusInternalServerError, "result_unavailable")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sink.result)
}

type localOperationResultSink struct {
	result *resourcecontroller.Result
}

func (s *localOperationResultSink) Report(_ context.Context, result resourcecontroller.Result) error {
	if s == nil || s.result != nil {
		return errors.New("local operation result sink cannot accept another result")
	}
	copy := result
	s.result = &copy
	return nil
}

func writeLocalOperationError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}
