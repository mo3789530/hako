//go:build linux && firecracker_driver_smoke

// local_firecracker_driver_smoke exercises the concrete host driver against a
// disposable Firecracker Guest. It is invoked only by the explicit KVM smoke.
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/workspaceruntime"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "local Firecracker driver smoke failed:", err)
		os.Exit(1)
	}
}

func run() error {
	if len(os.Args) != 6 {
		return errors.New("usage: local_firecracker_driver_smoke <firecracker> <kernel> <initrd> <state-dir> <boot-args>")
	}
	binary, kernel, initrd, stateDir, bootArgs := os.Args[1], os.Args[2], os.Args[3], os.Args[4], os.Args[5]
	config := workspaceruntime.FirecrackerLocalDriverConfig{BinaryPath: binary, StateDirectory: stateDir,
		StartupTimeout: 15 * time.Second, StopTimeout: 20 * time.Second}
	kernelSHA, err := hashFile(kernel)
	if err != nil {
		return err
	}
	initrdSHA, err := hashFile(initrd)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(workspaceruntime.FirecrackerLocalLaunchSpec{KernelImagePath: kernel, InitrdPath: initrd,
		KernelImageSHA256: kernelSHA, InitrdSHA256: initrdSHA,
		KernelBootArgs: bootArgs + " hako_disk_mode=write hako_enable_vsock=1", VCPUCount: 1, MemoryMiB: 256, WorkspaceDiskSizeMiB: 64})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	const allocationID = "fc_driver_smoke"
	first, err := workspaceruntime.NewFirecrackerLocalDriver(config)
	if err != nil {
		return err
	}
	if err := first.EnsureRunning(ctx, allocationID, payload); err != nil {
		return err
	}
	if err := waitForMarker(ctx, filepath.Join(stateDir, allocationID, "firecracker.log"), "HAKO_LOCAL_GUEST_READY_V1"); err != nil {
		return err
	}
	if err := waitForMarker(ctx, filepath.Join(stateDir, allocationID, "firecracker.log"), "HAKO_LOCAL_GUEST_VSOCK_READY_V1"); err != nil {
		return err
	}
	if err := probeGuest(ctx, stateDir, allocationID); err != nil {
		return err
	}
	if err := first.Suspend(ctx, allocationID); err != nil {
		return err
	}
	// Recreate the driver as if the Resource Controller process restarted.
	restarted, err := workspaceruntime.NewFirecrackerLocalDriver(config)
	if err != nil {
		return err
	}
	if err := restarted.Resume(ctx, allocationID, payload); err != nil {
		return err
	}
	if err := probeGuest(ctx, stateDir, allocationID); err != nil {
		return err
	}
	if err := waitForMarker(ctx, filepath.Join(stateDir, allocationID, "firecracker.log"), "HAKO_LOCAL_DRIVER_DISK_RESTORED_OK"); err != nil {
		return err
	}
	if err := restarted.EnsureRunning(ctx, allocationID, payload); err != nil {
		return err
	}
	if err := restarted.Delete(ctx, allocationID); err != nil {
		return err
	}
	if _, err := os.Lstat(filepath.Join(stateDir, allocationID)); !errors.Is(err, os.ErrNotExist) {
		return errors.New("local Firecracker allocation files were not removed")
	}
	fmt.Println("HAKO_LOCAL_FIRECRACKER_DRIVER_LIFECYCLE_OK")
	return nil
}

func probeGuest(ctx context.Context, stateDir, allocationID string) error {
	deadline := time.NewTimer(15 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		dialCtx, cancel := context.WithTimeout(ctx, time.Second)
		conn, err := workspaceruntime.DialFirecrackerLocalGuest(dialCtx, filepath.Join(stateDir, allocationID))
		cancel()
		if err == nil {
			_ = conn.SetDeadline(time.Now().Add(time.Second))
			request := "GET /healthz HTTP/1.1\r\nHost: workspace-agent.vsock\r\nConnection: close\r\n\r\n"
			if _, err = io.WriteString(conn, request); err == nil {
				response, readErr := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodGet})
				if readErr == nil {
					body, bodyErr := io.ReadAll(io.LimitReader(response.Body, 256))
					_ = response.Body.Close()
					if bodyErr == nil && response.StatusCode == http.StatusOK && string(body) == "HAKO_WORKSPACE_AGENT_VSOCK_HTTP_OK" {
						_ = conn.Close()
						return nil
					}
					err = errors.New("Guest vsock returned an unexpected HTTP response")
				} else {
					err = readErr
				}
			}
			_ = conn.Close()
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("Guest host-private vsock ping failed: %w", lastErr)
		case <-ticker.C:
		}
	}
}

func waitForMarker(ctx context.Context, path, marker string) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		data, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(data), marker) {
			return nil
		}
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("guest did not emit %s before timeout", marker)
		case <-ticker.C:
		}
	}
}

func hashFile(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:]), nil
}
