//go:build linux

// hako-workspace-vsock-smoke-guest serves a minimal HTTP endpoint through the
// same net.Listener implementation used by the Workspace Agent. The local
// Cloud Hypervisor smoke packages this static binary into a disposable initrd.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/workspaceagent"
)

func main() {
	listener, err := workspaceagent.ListenWorkspaceAgentVsock(workspaceagent.WorkspaceAgentVsockPort)
	if err != nil {
		fmt.Println("HAKO_VSOCK_SMOKE_LISTENER_FAILED")
		return
	}
	defer listener.Close()
	mux := http.NewServeMux()
	identity := readGuestIdentity()
	mux.HandleFunc("GET /internal/heartbeat", func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal(map[string]any{"protocol_version": 1, "tenant_id": identity["tenant_id"],
			"workspace_id": identity["workspace_id"], "resource_plane_id": identity["resource_plane_id"], "microvm_id": identity["microvm_id"]})
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		w.WriteHeader(http.StatusOK)
		if _, err := w.Write(body); err != nil {
			return
		}
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "HAKO_WORKSPACE_AGENT_VSOCK_HTTP_OK")
	})
	go func() {
		if err := (&http.Server{Handler: mux}).Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Println("HAKO_VSOCK_SMOKE_SERVER_FAILED")
		}
	}()
	controlListener, err := workspaceagent.ListenWorkspaceAgentVsock(7001)
	if err != nil {
		fmt.Println("HAKO_VSOCK_SMOKE_CONTROL_LISTENER_FAILED")
		return
	}
	control := http.NewServeMux()
	control.HandleFunc("POST /internal/shutdown", func(w http.ResponseWriter, request *http.Request) {
		_, _ = io.Copy(io.Discard, io.LimitReader(request.Body, 4096))
		w.WriteHeader(http.StatusNoContent)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		go func() {
			time.Sleep(100 * time.Millisecond)
			syscall.Sync()
			if syscall.Unmount("/workspace", 0) == nil {
				fmt.Fprintln(os.Stdout, "HAKO_LOCAL_GUEST_QUIESCENT_V1")
				_ = syscall.Reboot(syscall.LINUX_REBOOT_CMD_POWER_OFF)
			}
		}()
	})
	if err := (&http.Server{Handler: control}).Serve(controlListener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Println("HAKO_VSOCK_SMOKE_CONTROL_SERVER_FAILED")
	}
}

func readGuestIdentity() map[string]string {
	identity := map[string]string{}
	cmdline, err := os.ReadFile("/proc/cmdline")
	if err != nil {
		return identity
	}
	for _, field := range strings.Fields(string(cmdline)) {
		key, value, ok := strings.Cut(field, "=")
		if !ok {
			continue
		}
		switch key {
		case "hako.tenant_id":
			identity["tenant_id"] = value
		case "hako.workspace_id":
			identity["workspace_id"] = value
		case "hako.resource_plane_id":
			identity["resource_plane_id"] = value
		case "hako.microvm_id":
			identity["microvm_id"] = value
		}
	}
	return identity
}
