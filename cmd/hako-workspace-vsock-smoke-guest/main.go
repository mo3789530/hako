//go:build linux

// hako-workspace-vsock-smoke-guest serves a minimal HTTP endpoint through the
// same net.Listener implementation used by the Workspace Agent. The local
// Firecracker smoke packages this static binary into a disposable initrd.
package main

import (
	"errors"
	"fmt"
	"net/http"

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
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = fmt.Fprint(w, "HAKO_WORKSPACE_AGENT_VSOCK_HTTP_OK")
	})
	if err := (&http.Server{Handler: mux}).Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		fmt.Println("HAKO_VSOCK_SMOKE_SERVER_FAILED")
	}
}
