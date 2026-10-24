// hako-workspace-agent is the unprivileged interactive child process for one
// Workspace Lambda MicroVM. It inherits only the already-bound Unix listener
// and strict mount-hook identity from hako-workspace-init.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workspaceagent"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Workspace Agent stopped: ", err)
	}
}

func run() error {
	payload, err := workspaceagent.ReadWorkspaceAgentChildPayload(os.Stdin)
	if err != nil || os.Geteuid() != int(payload.Mount.UID) || os.Getegid() != int(payload.Mount.GID) {
		return errors.New("validate unprivileged Workspace Agent identity")
	}
	keyPath := strings.TrimSpace(os.Getenv(workspaceagent.WorkspaceAgentTrustedKeysFileEnvironment))
	if keyPath == "" || !strings.HasPrefix(keyPath, "/") {
		return errors.New("configure image-owned Workspace Agent public keys")
	}
	keyFile, err := os.Open(keyPath)
	if err != nil {
		return errors.New("open image-provisioned Workspace Agent public keys")
	}
	keys, keyErr := workloadagent.LoadTrustedKeys(keyFile)
	closeErr := keyFile.Close()
	if keyErr != nil || closeErr != nil {
		return errors.New("load image-provisioned Workspace Agent public keys")
	}
	agent, err := workspaceagent.NewWorkspaceShellAgent(keys, payload.GatewayOrigin)
	if err != nil {
		return errors.New("configure Workspace shell Agent")
	}
	if err := agent.BindWorkspaceRuntime(payload.MicroVMID, payload.Mount); err != nil {
		return errors.New("bind Workspace shell Agent identity")
	}
	workspaceFiles, err := workspaceagent.NewWorkspaceFiles(payload.Mount.MountPath, payload.Mount.UID, payload.Mount.GID)
	if err != nil {
		return errors.New("configure confined Workspace file transfer")
	}
	defer workspaceFiles.Close()
	if err := agent.BindWorkspaceFileRuntime(workspaceFiles); err != nil {
		return errors.New("bind Workspace file transfer")
	}
	servicesEnabled := os.Getenv(workspaceagent.WorkspaceServicesEnabledEnvironment)
	containerdSocket := strings.TrimSpace(os.Getenv(workspaceagent.ContainerdSocketEnvironment))
	switch servicesEnabled {
	case "":
		if containerdSocket != "" {
			return errors.New("unexpected Workspace containerd socket while Services are disabled")
		}
	case "true":
		if containerdSocket == "" {
			return errors.New("Workspace containerd socket is required when Services are enabled")
		}
	default:
		return errors.New("Workspace Service runtime setting is invalid")
	}
	if servicesEnabled == "true" {
		engine, err := workspaceagent.NewNerdctlServiceEngine(workspaceagent.NerdctlServiceEngineConfig{
			BinaryPath: "/usr/local/bin/nerdctl", ContainerdSocket: containerdSocket,
			WorkspaceRoot: payload.Mount.MountPath, WorkspaceID: domain.WorkspaceID(payload.Mount.WorkspaceID), RunAsUID: payload.Mount.UID,
		})
		if err != nil {
			return errors.New("configure rootless containerd Service engine")
		}
		if err := agent.BindWorkspaceServiceRuntime(engine); err != nil {
			return errors.New("bind Workspace Service runtime")
		}
	}
	codeServer, err := workspaceagent.StartWorkspaceCodeServer(payload.Mount)
	if err != nil {
		return errors.New("start Workspace code-server")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := codeServer.Stop(ctx); err != nil {
			log.Printf("Workspace code-server shutdown did not finish cleanly")
		}
	}()
	defer func() {
		if err := agent.Close(); err != nil {
			log.Printf("Workspace PTY shutdown did not finish cleanly")
		}
	}()
	listenerFile := os.NewFile(workspaceagent.ChildListenerFileDescriptor, "workspace-agent-listener")
	if listenerFile == nil {
		return errors.New("receive inherited Workspace Agent listener")
	}
	listener, err := net.FileListener(listenerFile)
	_ = listenerFile.Close()
	if err != nil {
		return errors.New("open inherited Workspace Agent listener")
	}
	server := &http.Server{Handler: agent, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("Workspace Agent Unix listener stopped unexpectedly")
		}
		return nil
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return errors.New("shut down Workspace Agent Unix listener")
		}
		return nil
	}
}
