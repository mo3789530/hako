//go:build linux

// hako-local-idp runs a single-subject OAuth/JWKS provider for development.
// It deliberately binds only to a loopback IP and must not be deployed.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/localidp"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "local Hako identity provider stopped (failure_class=local_idp_runtime)")
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	if ctx == nil || getenv == nil {
		return errors.New("local identity provider dependencies are required")
	}
	address := strings.TrimSpace(getenv("HAKO_LOCAL_IDP_ADDR"))
	if address == "" {
		address = "127.0.0.1:8787"
	}
	host, port, err := net.SplitHostPort(address)
	ip := net.ParseIP(host)
	if err != nil || ip == nil || !ip.IsLoopback() || port == "" {
		return errors.New("HAKO_LOCAL_IDP_ADDR must use a loopback IP and port")
	}
	clientID := strings.TrimSpace(getenv("HAKO_LOCAL_IDP_CLIENT_ID"))
	if clientID == "" {
		clientID = "hako-local-dev"
	}
	subject := strings.TrimSpace(getenv("HAKO_LOCAL_IDP_SUBJECT"))
	if subject == "" {
		subject = "hako-local-user"
	}
	provider, err := localidp.New(localidp.Config{Origin: "http://" + net.JoinHostPort(host, port), ClientID: clientID, Subject: subject})
	if err != nil {
		return errors.New("configure local Hako identity provider")
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return errors.New("listen on local Hako identity provider address")
	}
	server := &http.Server{Handler: provider, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second,
		WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-serveErr:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("serve local Hako identity provider")
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return server.Shutdown(shutdown)
}
