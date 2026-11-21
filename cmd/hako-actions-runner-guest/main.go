// hako-actions-runner-guest is the non-root HTTP/runtime entrypoint inside one
// Lambda MicroVM. It accepts one provider /run identity and one signed runner
// bootstrap, then starts the official GitHub Actions runner once.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/actionsrunner"
)

const (
	defaultRunnerRoot = "/opt/actions-runner"
	defaultPublicKey  = "/etc/hako/actions-runner-bootstrap.pub"
	listenAddress     = "0.0.0.0:8080"
)

func main() {
	if err := run(); err != nil {
		log.Fatal("Hako Actions runner guest stopped (failure_class=process_exit)")
	}
}

func run() error {
	keyPath := strings.TrimSpace(os.Getenv("HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE"))
	if keyPath == "" {
		keyPath = defaultPublicKey
	}
	publicKey, err := loadBootstrapPublicKey(keyPath)
	if err != nil {
		return errors.New("load runner bootstrap verification key")
	}
	defer clear(publicKey)
	consumer, err := actionsrunner.NewRunnerProcessConsumer(actionsrunner.RunnerProcessConfig{
		RootDirectory: strings.TrimSpace(defaultString(os.Getenv("HAKO_ACTIONS_RUNNER_ROOT"), defaultRunnerRoot)),
		SetupTimeout:  10 * time.Second,
		MaximumRun:    8 * time.Hour,
		OnExit: func(result actionsrunner.RunnerProcessResult) {
			log.Printf("ephemeral runner finished job=%d attempt=%d exit_code=%d failure_phase=%s cancelled=%t timed_out=%t local_cleanup_failed=%t diagnostics_upload_failed=%t",
				result.WorkflowJobID, result.RunAttempt, result.ExitCode, result.FailurePhase, result.Cancelled, result.TimedOut, result.CleanupFailed, result.DiagnosticsUploadFailed)
		},
	})
	if err != nil {
		return errors.New("configure ephemeral runner process")
	}
	defer consumer.Stop()
	guest, err := actionsrunner.NewRunnerGuestRuntime(publicKey, consumer)
	if err != nil {
		return errors.New("configure MicroVM guest identity handler")
	}
	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		return errors.New("listen for Lambda MicroVM lifecycle and bootstrap requests")
	}
	server := &http.Server{Handler: guest, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	return serveGuest(ctx, listener, server)
}

func serveGuest(ctx context.Context, listener net.Listener, server *http.Server) error {
	if ctx == nil || listener == nil || server == nil || server.Handler == nil {
		return errors.New("runner guest server is not configured")
	}
	serveResult := make(chan error, 1)
	go func() { serveResult <- server.Serve(listener) }()
	select {
	case err := <-serveResult:
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("runner guest HTTP server stopped unexpectedly")
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			_ = server.Close()
			return errors.New("shut down runner guest HTTP server")
		}
		return nil
	}
}

func loadBootstrapPublicKey(path string) (ed25519.PublicKey, error) {
	if !strings.HasPrefix(path, "/") || strings.TrimSpace(path) != path {
		return nil, errors.New("invalid public key path")
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 || len(data) > 4096 {
		clear(data)
		return nil, errors.New("invalid public key file")
	}
	defer clear(data)
	block, rest := pem.Decode(data)
	if block == nil || block.Type != "PUBLIC KEY" || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid public key PEM")
	}
	parsed, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, errors.New("invalid public key encoding")
	}
	key, ok := parsed.(ed25519.PublicKey)
	if !ok || len(key) != ed25519.PublicKeySize {
		return nil, errors.New("bootstrap key is not Ed25519")
	}
	return append(ed25519.PublicKey(nil), key...), nil
}

func defaultString(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
