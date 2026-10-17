// hako-workspace-init is the root-owned Lambda MicroVM lifecycle endpoint
// used by Workspace images. It mounts the trusted EFS Access Point before
// acknowledging the /run hook. Interactive Workspace processes are separate
// and must not run with this process's privileges.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/workspaceagent"
)

const workspaceInitListenAddress = "0.0.0.0:8080"

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Workspace init stopped: ", err)
	}
}

func run() error {
	handler, mountExecutor, err := workspaceagent.NewLinuxWorkspaceMountLifecycle(http.NotFoundHandler())
	if err != nil {
		return errors.New("configure Workspace init")
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := mountExecutor.Unmount(ctx); err != nil {
			log.Printf("Workspace EFS unmount did not finish cleanly")
		}
	}()
	listener, err := net.Listen("tcp", workspaceInitListenAddress)
	if err != nil {
		return errors.New("listen for Lambda MicroVM lifecycle hooks")
	}
	server := &http.Server{Handler: handler, ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(signals)
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("Workspace init listener stopped unexpectedly")
		}
		return nil
	case <-signals:
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(ctx); err != nil {
			_ = server.Close()
			return errors.New("shut down Workspace init listener")
		}
		return nil
	}
}
