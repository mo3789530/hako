package workspaceruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

type localOperationRuntimeFunc func(context.Context, resourcecontroller.Command) (domain.ObservedWorkspaceState, error)

func (f localOperationRuntimeFunc) Execute(ctx context.Context, command resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
	return f(ctx, command)
}

func TestLocalOperationAPIExecutesAndReturnsNormalResourceControllerResult(t *testing.T) {
	now := time.Now().UTC()
	store, err := workspacejournal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	runtime, err := NewLocalMicroVM(driver, store, localResolverFunc(func(_ context.Context, command resourcecontroller.Command) ([]byte, error) {
		if command.TenantID != "tenant_1" || command.WorkspaceID != "ws_1" || command.ResourcePlaneID != "rp_local" {
			t.Fatalf("launch resolver received unexpected operation: %+v", command)
		}
		return []byte(`{"fixture":"trusted local profile"}`), nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewLocalOperationAPI("rp_local", runtime, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: 1, OperationID: "op_1",
		TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_local",
		Type: domain.OperationEnsureRunning, CreatedAt: now}
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	var result resourcecontroller.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Status != domain.OperationSucceeded || result.ObservedState != domain.ObservedWorkspaceRunning ||
		result.OperationID != command.OperationID || result.TenantID != command.TenantID || result.WorkspaceID != command.WorkspaceID ||
		result.ResourceKind != "cloud_hypervisor_local" || result.ExternalResourceID == "" || result.AllocationGeneration != 1 {
		t.Fatalf("local operation response = status %d, result %+v, body %q", response.Code, result, response.Body.String())
	}
	if len(driver.states) != 1 {
		t.Fatalf("local Cloud Hypervisor allocations = %d, want 1", len(driver.states))
	}
	retry := postLocalOperation(t, api, command)
	if retry.ExternalResourceID != result.ExternalResourceID || retry.AllocationGeneration != result.AllocationGeneration || len(driver.states) != 1 {
		t.Fatalf("idempotent operation retry changed allocation: initial=%+v retry=%+v allocations=%d", result, retry, len(driver.states))
	}
}

func TestLocalOperationAPIExecutesFullWorkspaceOperationLifecycle(t *testing.T) {
	now := time.Now().UTC()
	store, err := workspacejournal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	runtime, err := NewLocalMicroVM(driver, store, localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"fixture":"trusted local profile"}`), nil
	}), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewLocalOperationAPI("rp_local", runtime, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	for revision, step := range []struct {
		operationID domain.OperationID
		typeName    domain.OperationType
		observed    domain.ObservedWorkspaceState
	}{
		{"op_ensure", domain.OperationEnsureRunning, domain.ObservedWorkspaceRunning},
		{"op_suspend", domain.OperationSuspend, domain.ObservedWorkspaceSuspended},
		{"op_resume", domain.OperationResume, domain.ObservedWorkspaceRunning},
		{"op_delete", domain.OperationDelete, domain.ObservedWorkspaceDeleted},
	} {
		command := resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: int64(revision + 1),
			OperationID: step.operationID, TenantID: "tenant_1",
			WorkspaceID: "ws_lifecycle", ResourcePlaneID: "rp_local", Type: step.typeName, CreatedAt: now}
		result := postLocalOperation(t, api, command)
		if result.Status != domain.OperationSucceeded || result.ObservedState != step.observed || result.OperationID != command.OperationID {
			t.Fatalf("operation %s returned %+v, want observed %s", step.typeName, result, step.observed)
		}
		if step.typeName != domain.OperationDelete && (result.ResourceKind != "cloud_hypervisor_local" || result.ExternalResourceID == "") {
			t.Fatalf("operation %s omitted active local binding: %+v", step.typeName, result)
		}
	}
	if len(driver.states) != 0 || len(driver.launches) != 0 {
		t.Fatalf("delete retained local runtime resources: states=%v launches=%v", driver.states, driver.launches)
	}
}

func postLocalOperation(t *testing.T, api http.Handler, command resourcecontroller.Command) resourcecontroller.Result {
	t.Helper()
	payload, err := json.Marshal(command)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	var result resourcecontroller.Result
	if response.Code != http.StatusOK {
		t.Fatalf("local operation HTTP status %d: %s", response.Code, response.Body.String())
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestLocalOperationAPIRejectsInvalidRouteAndOversizedBody(t *testing.T) {
	api, err := NewLocalOperationAPI("rp_local", localOperationRuntimeFunc(func(context.Context, resourcecontroller.Command) (domain.ObservedWorkspaceState, error) {
		return domain.ObservedWorkspaceRunning, nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name   string
		method string
		path   string
		body   string
		media  string
		status int
	}{
		{name: "wrong path", method: http.MethodPost, path: "/debug", status: http.StatusNotFound},
		{name: "wrong method", method: http.MethodGet, path: "/v1/operations", status: http.StatusMethodNotAllowed},
		{name: "oversized", method: http.MethodPost, path: "/v1/operations", body: strings.Repeat("x", maxLocalOperationRequestBytes+1), status: http.StatusRequestEntityTooLarge},
		{name: "unknown field", method: http.MethodPost, path: "/v1/operations", body: `{"operation_id":"op_1","unexpected":"secret"}`, status: http.StatusBadRequest},
		{name: "unsupported content type", method: http.MethodPost, path: "/v1/operations", body: `{}`, media: "text/plain", status: http.StatusUnsupportedMediaType},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := httptest.NewRequest(test.method, test.path, strings.NewReader(test.body))
			if test.method == http.MethodPost {
				media := test.media
				if media == "" {
					media = "application/json"
				}
				request.Header.Set("Content-Type", media)
			}
			response := httptest.NewRecorder()
			api.ServeHTTP(response, request)
			if response.Code != test.status || strings.Contains(response.Body.String(), "secret") || response.Header().Get("Cache-Control") != "no-store" {
				t.Fatalf("response status=%d headers=%v body=%q", response.Code, response.Header(), response.Body.String())
			}
		})
	}
}

func TestLocalOperationAPIReportsRuntimeFailureWithoutLeakingHostDetails(t *testing.T) {
	store, err := workspacejournal.NewFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	driver := newLocalDriverFake()
	driver.failAfter["ensure"] = errors.New("/private/state/token=secret")
	runtime, err := NewLocalMicroVM(driver, store, localResolverFunc(func(context.Context, resourcecontroller.Command) ([]byte, error) {
		return []byte(`{"fixture":"trusted local profile"}`), nil
	}), nil)
	if err != nil {
		t.Fatal(err)
	}
	api, err := NewLocalOperationAPI("rp_local", runtime, nil)
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{SchemaVersion: 2, WorkspaceRevision: 1, OperationID: "op_failure",
		TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlaneID: "rp_local",
		Type: domain.OperationEnsureRunning, CreatedAt: time.Now().UTC()}
	payload, _ := json.Marshal(command)
	request := httptest.NewRequest(http.MethodPost, "/v1/operations", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	api.ServeHTTP(response, request)
	var result resourcecontroller.Result
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Status != domain.OperationFailed || result.ErrorCode != "runtime_error" ||
		strings.Contains(response.Body.String(), "secret") || strings.Contains(response.Body.String(), "/private/state") {
		t.Fatalf("runtime error response leaked details: status=%d result=%+v body=%q", response.Code, result, response.Body.String())
	}
}
