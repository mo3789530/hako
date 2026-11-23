//go:build linux

// hako-local-workspace-reconciler accepts local Resource Controller operations
// and restores CloudHypervisor allocations from the durable journal.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspaceruntime"
)

const (
	defaultReconcileInterval = 30 * time.Second
	defaultReconcileLimit    = 100000
	maxLaunchSpecBytes       = 16 << 10
)

type reconcileReport struct {
	TenantID        string `json:"tenant_id,omitempty"`
	WorkspaceID     string `json:"workspace_id,omitempty"`
	ResourcePlaneID string `json:"resource_plane_id,omitempty"`
	Observed        string `json:"observed_state,omitempty"`
	Error           string `json:"error,omitempty"`
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Getenv, os.Stdout); err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "local Workspace reconciliation stopped:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, getenv func(string) string, output io.Writer) error {
	if ctx == nil || getenv == nil || output == nil {
		return errors.New("local Workspace reconciler dependencies are required")
	}
	stateDir := strings.TrimSpace(getenv("HAKO_LOCAL_CLOUD_HYPERVISOR_STATE_DIR"))
	journalDir := strings.TrimSpace(getenv("HAKO_LOCAL_WORKSPACE_JOURNAL_DIR"))
	binary := strings.TrimSpace(getenv("HAKO_LOCAL_CLOUD_HYPERVISOR_BINARY"))
	launchPath := strings.TrimSpace(getenv("HAKO_LOCAL_CLOUD_HYPERVISOR_LAUNCH_SPEC"))
	socketPath := strings.TrimSpace(getenv("HAKO_LOCAL_WORKSPACE_CONTROLLER_SOCKET"))
	resourcePlaneID := strings.TrimSpace(getenv("HAKO_LOCAL_RESOURCE_PLANE_ID"))
	gatewayOrigin := strings.TrimSpace(getenv("HAKO_LOCAL_WORKSPACE_GATEWAY_ORIGIN"))
	if !cleanAbsolute(stateDir) || !cleanAbsolute(journalDir) || !cleanAbsolute(binary) || !cleanAbsolute(launchPath) || !cleanAbsolute(socketPath) || resourcePlaneID == "" || gatewayOrigin == "" {
		return errors.New("local CloudHypervisor, journal, controller socket, launch-spec paths, Resource Plane ID, and Gateway origin must be configured")
	}
	interval := defaultReconcileInterval
	if raw := strings.TrimSpace(getenv("HAKO_LOCAL_RECONCILE_INTERVAL")); raw != "" {
		parsed, err := time.ParseDuration(raw)
		if err != nil || parsed < 100*time.Millisecond || parsed > 10*time.Minute {
			return errors.New("HAKO_LOCAL_RECONCILE_INTERVAL must be between 100ms and 10m")
		}
		interval = parsed
	}
	limit := defaultReconcileLimit
	if raw := strings.TrimSpace(getenv("HAKO_LOCAL_RECONCILE_LIMIT")); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > defaultReconcileLimit {
			return errors.New("HAKO_LOCAL_RECONCILE_LIMIT must be between 1 and 100000")
		}
		limit = parsed
	}
	payload, err := readPrivateLaunchSpec(launchPath)
	if err != nil {
		return err
	}
	store, err := workspacejournal.NewFileStore(journalDir)
	if err != nil {
		return errors.New("configure local Workspace journal")
	}
	resolver, err := workspaceruntime.NewCloudHypervisorLocalWorkspaceLaunchResolver(payload, store, gatewayOrigin)
	if err != nil {
		return errors.New("validate trusted local CloudHypervisor launch specification")
	}
	driver, err := workspaceruntime.NewCloudHypervisorLocalDriver(workspaceruntime.CloudHypervisorLocalDriverConfig{
		BinaryPath: binary, StateDirectory: stateDir,
	})
	if err != nil {
		return errors.New("configure local CloudHypervisor driver")
	}
	runtime, err := workspaceruntime.NewLocalMicroVM(driver, store, resolver, nil)
	if err != nil {
		return errors.New("configure local Workspace runtime")
	}
	operationAPI, err := workspaceruntime.NewLocalOperationAPI(domain.ResourcePlaneID(resourcePlaneID), runtime, nil)
	if err != nil {
		return errors.New("configure local Workspace operation API")
	}
	listener, err := listenPrivateSocket(socketPath)
	if err != nil {
		return errors.New("listen on local Workspace controller socket")
	}
	defer closePrivateSocket(listener, socketPath)
	server := &http.Server{Handler: operationAPI, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 2 * time.Minute,
		WriteTimeout: 16 * time.Minute, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 16 << 10}
	loopCtx, cancelLoop := context.WithCancel(ctx)
	defer cancelLoop()
	loopErr := make(chan error, 1)
	go func() {
		loopErr <- workspaceruntime.RunLocalReconcileLoop(loopCtx, runtime, interval, limit, func(results []workspaceruntime.LocalReconcileResult, reconcileErr error) {
			if reconcileErr != nil {
				_ = json.NewEncoder(output).Encode(reconcileReport{Error: "journal reconciliation failed"})
			}
			for _, result := range results {
				_ = json.NewEncoder(output).Encode(reconcileReportFor(result))
			}
		})
	}()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.Serve(listener) }()
	select {
	case <-ctx.Done():
	case err := <-loopErr:
		if err != nil && !errors.Is(err, context.Canceled) {
			cancelLoop()
			_ = server.Close()
			return errors.New("local Workspace reconciliation loop stopped")
		}
	case err := <-serveErr:
		cancelLoop()
		if !errors.Is(err, http.ErrServerClosed) {
			return errors.New("local Workspace operation API stopped")
		}
	}
	cancelLoop()
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		return errors.New("shutdown local Workspace operation API")
	}
	return nil
}

func reconcileReportFor(result workspaceruntime.LocalReconcileResult) reconcileReport {
	entry := reconcileReport{TenantID: string(result.TenantID), WorkspaceID: string(result.WorkspaceID),
		ResourcePlaneID: string(result.ResourcePlaneID), Observed: string(result.Observed)}
	if result.Err != nil {
		// Driver errors can contain host paths, configuration or provider detail.
		entry.Error = "allocation reconciliation failed"
	}
	return entry
}

func readPrivateLaunchSpec(path string) ([]byte, error) {
	file, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("open local CloudHypervisor launch specification")
	}
	defer file.Close()
	info, err := file.Stat()
	stat, ok := func() (*syscall.Stat_t, bool) {
		if info == nil {
			return nil, false
		}
		value, valid := info.Sys().(*syscall.Stat_t)
		return value, valid
	}()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() < 1 || info.Size() > maxLaunchSpecBytes || !ok || stat.Uid != uint32(os.Getuid()) {
		return nil, errors.New("local CloudHypervisor launch specification must be a private owned regular file")
	}
	payload, err := io.ReadAll(io.LimitReader(file, maxLaunchSpecBytes+1))
	if err != nil || len(payload) > maxLaunchSpecBytes {
		return nil, errors.New("local CloudHypervisor launch specification exceeds its size limit")
	}
	return payload, nil
}

func cleanAbsolute(path string) bool {
	return filepath.IsAbs(path) && filepath.Clean(path) == path && strings.TrimSpace(path) == path
}
