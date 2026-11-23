//go:build linux

// hako-local-workspace-worker drains Workspace Operation events from the
// Control Plane Outbox and applies them through the local Cloud Hypervisor
// Resource Controller over its private Unix socket.
package main

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/localoperationworker"
	"github.com/mo3789530/hako/internal/operationresults"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv); err != nil && !errors.Is(err, context.Canceled) {
		log.Fatal("local Workspace Outbox worker stopped (failure_class=worker_runtime)")
	}
}

func run(ctx context.Context, getenv func(string) string) error {
	if ctx == nil || getenv == nil {
		return errors.New("local Workspace worker dependencies are required")
	}
	socketPath := strings.TrimSpace(getenv("HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET"))
	if !filepath.IsAbs(socketPath) || filepath.Clean(socketPath) != socketPath {
		return errors.New("HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET must be a clean absolute path")
	}
	p, err := openDatabase(ctx, getenv)
	if err != nil {
		return errors.New("connect local Workspace worker database")
	}
	defer p.Close()
	client, err := unixSocketClient(socketPath)
	if err != nil {
		return errors.New("validate local Workspace Controller socket")
	}
	worker, err := localoperationworker.New(dispatcher.StoreAdapter{Pool: p, Policy: transaction.DefaultPolicy()}, operationresults.StoreAdapter{Pool: p, Policy: transaction.DefaultPolicy()}, client, localoperationworker.Config{
		BatchSize: 10, LeaseDuration: 20 * time.Minute, BaseBackoff: time.Second, MaxBackoff: time.Minute,
	})
	if err != nil {
		return errors.New("configure local Workspace Outbox worker")
	}
	interval := time.Second
	if raw := strings.TrimSpace(getenv("HAKO_LOCAL_WORKSPACE_WORKER_INTERVAL")); raw != "" {
		interval, err = time.ParseDuration(raw)
		if err != nil || interval < 100*time.Millisecond || interval > time.Minute {
			return errors.New("HAKO_LOCAL_WORKSPACE_WORKER_INTERVAL must be between 100ms and 1m")
		}
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	log.Printf("local Workspace Outbox worker started (poll interval %s)", interval)
	for {
		stats, err := worker.RunOnce(ctx)
		if err != nil {
			log.Printf("local Workspace Outbox batch failed (failure_class=dispatch_batch claimed=%d applied=%d failed=%d)", stats.Claimed, stats.Applied, stats.Failed)
		} else if stats.Claimed > 0 {
			log.Printf("local Workspace Outbox batch complete (claimed=%d applied=%d)", stats.Claimed, stats.Applied)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func unixSocketClient(path string) (*http.Client, error) {
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSocket == 0 || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("local Workspace Controller socket must be a private Unix socket")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("local Workspace Controller socket must be owned by this user")
	}
	transport := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 5 * time.Second}).DialContext(ctx, "unix", path)
	}}
	return &http.Client{Transport: transport, Timeout: 16 * time.Minute}, nil
}

func openDatabase(ctx context.Context, getenv func(string) string) (*pgxpool.Pool, error) {
	postgresURL := strings.TrimSpace(getenv("HAKO_DATABASE_URL"))
	dsqlHost := strings.TrimSpace(getenv("HAKO_DSQL_HOST"))
	if postgresURL == "" || dsqlHost != "" {
		return nil, errors.New("local Workspace worker requires HAKO_DATABASE_URL and does not support DSQL")
	}
	return dsql.OpenPostgres(ctx, postgresURL)
}
