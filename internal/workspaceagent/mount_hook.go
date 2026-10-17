package workspaceagent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strings"
)

const (
	workspaceLifecyclePrefix = "/aws/lambda-microvms/runtime/v1/"
	maxWorkspaceHookBytes    = 16 << 10
)

var (
	workspaceFileSystemIDPattern = regexp.MustCompile(`^fs-[0-9a-f]{8,40}$`)
	workspaceAccessPointPattern  = regexp.MustCompile(`^fsap-[0-9a-f]{8,40}$`)
	workspaceRootPathPattern     = regexp.MustCompile(`^/hako/[a-f0-9]{16}/[a-f0-9]{16}/[a-f0-9]{16}$`)
	workspaceMicroVMIDPattern    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
	workspaceRegionPattern       = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)+$`)
)

// WorkspaceMountSpec is non-secret initialization metadata from the trusted
// Resource Plane. The workspace image must mount this exact EFS Access Point
// before acknowledging the Lambda MicroVM /run hook.
type WorkspaceMountSpec struct {
	TenantID        string `json:"tenant_id"`
	WorkspaceID     string `json:"workspace_id"`
	ResourcePlaneID string `json:"resource_plane_id"`
	FileSystemID    string `json:"file_system_id"`
	Region          string `json:"region"`
	AccessPointID   string `json:"access_point_id"`
	RootPath        string `json:"root_path"`
	MountPath       string `json:"mount_path"`
	UID             uint32 `json:"uid"`
	GID             uint32 `json:"gid"`
	TLS             bool   `json:"tls"`
	IAM             bool   `json:"iam"`
}

type WorkspaceMountExecutor interface {
	MountAndVerify(context.Context, WorkspaceMountSpec) error
}

// WorkspaceRuntimeBinder is called after storage is mounted but before the
// Lambda MicroVM run hook is acknowledged. Guest Agents use it to pin their
// immutable Workspace/MicroVM identity before inbound traffic is enabled.
type WorkspaceRuntimeBinder interface {
	BindWorkspaceRuntime(string, WorkspaceMountSpec) error
}

type workspaceMountHookPayload struct {
	Hako struct {
		WorkspaceMount WorkspaceMountSpec `json:"workspace_mount"`
	} `json:"hako"`
}

type workspaceRuntimeHookRequest struct {
	MicroVMID      string `json:"microvmId"`
	RunHookPayload string `json:"runHookPayload"`
}

// NewWorkspaceMountLifecycleHandler exposes the three Lambda MicroVM image
// hooks. /run returns success only after the injected mount executor confirms
// the TLS-enabled Access Point is mounted at /workspace.
func NewWorkspaceMountLifecycleHandler(next http.Handler, executor WorkspaceMountExecutor) (http.Handler, error) {
	if next == nil || executor == nil {
		return nil, errors.New("Workspace Agent handler and mount executor are required")
	}
	mux := http.NewServeMux()
	mux.Handle("/v1/", next)
	mux.Handle("/", next)
	mux.HandleFunc(workspaceLifecyclePrefix+"ready", workspaceHookReady)
	mux.HandleFunc(workspaceLifecyclePrefix+"validate", workspaceHookReady)
	mux.HandleFunc(workspaceLifecyclePrefix+"run", func(w http.ResponseWriter, r *http.Request) {
		binder, _ := next.(WorkspaceRuntimeBinder)
		workspaceHookRun(w, r, executor, binder)
	})
	return mux, nil
}

func workspaceHookReady(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.ContentLength > 0 || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid Workspace lifecycle request", http.StatusBadRequest)
		return
	}
	w.WriteHeader(http.StatusOK)
}

func workspaceHookRun(w http.ResponseWriter, r *http.Request, executor WorkspaceMountExecutor, binder WorkspaceRuntimeBinder) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.URL.RawQuery != "" || r.URL.Fragment != "" || r.ContentLength > maxWorkspaceHookBytes || len(r.TransferEncoding) > 0 {
		http.Error(w, "invalid Workspace lifecycle request", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, maxWorkspaceHookBytes+1))
	if err != nil || len(body) == 0 || len(body) > maxWorkspaceHookBytes {
		http.Error(w, "invalid Workspace lifecycle request", http.StatusBadRequest)
		return
	}
	var request workspaceRuntimeHookRequest
	if decodeWorkspaceHookJSON(body, &request) != nil || !validWorkspaceHookVMID(request.MicroVMID) || strings.TrimSpace(request.RunHookPayload) == "" || len(request.RunHookPayload) > maxWorkspaceHookBytes {
		http.Error(w, "invalid Workspace lifecycle request", http.StatusBadRequest)
		return
	}
	var payload workspaceMountHookPayload
	if decodeWorkspaceHookJSON([]byte(request.RunHookPayload), &payload) != nil || !validWorkspaceMountSpec(payload.Hako.WorkspaceMount) {
		http.Error(w, "invalid Workspace lifecycle request", http.StatusBadRequest)
		return
	}
	if err := executor.MountAndVerify(r.Context(), payload.Hako.WorkspaceMount); err != nil {
		http.Error(w, "Workspace storage mount failed", http.StatusInternalServerError)
		return
	}
	if binder != nil {
		if err := binder.BindWorkspaceRuntime(request.MicroVMID, payload.Hako.WorkspaceMount); err != nil {
			http.Error(w, "Workspace Agent initialization failed", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusOK)
}

func decodeWorkspaceHookJSON(data []byte, destination any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if decoder.Decode(destination) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("invalid Workspace hook JSON")
	}
	return nil
}

func validWorkspaceHookVMID(value string) bool {
	return workspaceMicroVMIDPattern.MatchString(value)
}

func validWorkspaceMountSpec(spec WorkspaceMountSpec) bool {
	return workspaceMicroVMIDPattern.MatchString(spec.TenantID) && workspaceMicroVMIDPattern.MatchString(spec.WorkspaceID) &&
		workspaceMicroVMIDPattern.MatchString(spec.ResourcePlaneID) && workspaceFileSystemIDPattern.MatchString(spec.FileSystemID) && workspaceRegionPattern.MatchString(spec.Region) &&
		workspaceAccessPointPattern.MatchString(spec.AccessPointID) &&
		workspaceRootPathPattern.MatchString(spec.RootPath) && spec.MountPath == "/workspace" &&
		spec.UID >= 100000 && spec.GID == spec.UID && spec.TLS && spec.IAM
}
