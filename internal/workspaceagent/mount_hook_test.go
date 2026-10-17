package workspaceagent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type mountExecutorStub struct {
	spec  WorkspaceMountSpec
	calls int
	err   error
}

func (s *mountExecutorStub) MountAndVerify(_ context.Context, spec WorkspaceMountSpec) error {
	s.spec = spec
	s.calls++
	return s.err
}

func TestWorkspaceRunHookMountsExactAccessPointBeforeSuccess(t *testing.T) {
	executor := &mountExecutorStub{}
	handler, err := NewWorkspaceMountLifecycleHandler(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected regular agent route")
	}), executor)
	if err != nil {
		t.Fatal(err)
	}
	spec := WorkspaceMountSpec{FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/0123456789abcdef/1123456789abcdef/2123456789abcdef", MountPath: "/workspace", UID: 100123, GID: 100123, TLS: true, IAM: true}
	payload, err := json.Marshal(struct {
		Hako struct {
			WorkspaceMount WorkspaceMountSpec `json:"workspace_mount"`
		} `json:"hako"`
	}{Hako: struct {
		WorkspaceMount WorkspaceMountSpec `json:"workspace_mount"`
	}{WorkspaceMount: spec}})
	if err != nil {
		t.Fatal(err)
	}
	requestBody, _ := json.Marshal(workspaceRuntimeHookRequest{MicroVMID: "microvm-1", RunHookPayload: string(payload)})
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, workspaceLifecyclePrefix+"run", strings.NewReader(string(requestBody))))
	if recorder.Code != http.StatusOK || executor.calls != 1 || executor.spec != spec {
		t.Fatalf("run hook = %d, calls=%d spec=%+v", recorder.Code, executor.calls, executor.spec)
	}
}

func TestWorkspaceRunHookFailsClosedAndDoesNotAcknowledgeFailedMount(t *testing.T) {
	executor := &mountExecutorStub{err: errors.New("mount unavailable")}
	handler, err := NewWorkspaceMountLifecycleHandler(http.NotFoundHandler(), executor)
	if err != nil {
		t.Fatal(err)
	}
	valid := `{"microvmId":"microvm-1","runHookPayload":"{\"hako\":{\"workspace_mount\":{\"file_system_id\":\"fs-0123456789abcdef0\",\"region\":\"us-east-1\",\"access_point_id\":\"fsap-0123456789abcdef0\",\"root_path\":\"/hako/0123456789abcdef/1123456789abcdef/2123456789abcdef\",\"mount_path\":\"/workspace\",\"uid\":100123,\"gid\":100123,\"tls\":true,\"iam\":true}}}"}`
	for name, body := range map[string]string{
		"unknown wrapper field": strings.Replace(valid, `"microvmId"`, `"extra":true,"microvmId"`, 1),
		"TLS disabled":          strings.Replace(valid, `\"tls\":true`, `\"tls\":false`, 1),
		"IAM disabled":          strings.Replace(valid, `\"iam\":true`, `\"iam\":false`, 1),
		"wrong mount path":      strings.Replace(valid, `\"/workspace\"`, `\"/tmp/workspace\"`, 1),
		"trailing JSON":         valid + `{}`,
	} {
		t.Run(name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, workspaceLifecyclePrefix+"run", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest || executor.calls != 0 {
				t.Fatalf("invalid run hook status = %d calls=%d body=%s", recorder.Code, executor.calls, recorder.Body.String())
			}
		})
	}
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, workspaceLifecyclePrefix+"run", strings.NewReader(valid)))
	if recorder.Code != http.StatusInternalServerError || executor.calls != 1 {
		t.Fatalf("failed mount was acknowledged: status=%d calls=%d", recorder.Code, executor.calls)
	}
}

func TestWorkspaceLifecycleReadyAndBoundedRequests(t *testing.T) {
	handler, err := NewWorkspaceMountLifecycleHandler(http.NotFoundHandler(), &mountExecutorStub{})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"ready", "validate"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, workspaceLifecyclePrefix+path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("%s hook = %d", path, recorder.Code)
		}
	}
	tooLarge := httptest.NewRequest(http.MethodPost, workspaceLifecyclePrefix+"run", strings.NewReader(strings.Repeat("x", maxWorkspaceHookBytes+1)))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, tooLarge)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("oversized run hook status = %d", recorder.Code)
	}
}
