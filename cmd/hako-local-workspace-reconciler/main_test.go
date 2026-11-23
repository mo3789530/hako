//go:build linux

package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceruntime"
)

func TestReadPrivateLaunchSpecRequiresOwnedPrivateRegularFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "launch.json")
	if err := os.WriteFile(path, []byte(`{"kernel_image_path":"/kernel"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateLaunchSpec(path); err != nil {
		t.Fatalf("read private launch specification: %v", err)
	}
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateLaunchSpec(path); err == nil {
		t.Fatal("group/world-readable launch specification was accepted")
	}
	link := filepath.Join(dir, "launch-link.json")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateLaunchSpec(link); err == nil {
		t.Fatal("symlink launch specification was accepted")
	}
}

func TestReconcileReportRedactsRawDriverError(t *testing.T) {
	entry := reconcileReportFor(workspaceruntime.LocalReconcileResult{TenantID: "tenant_a", WorkspaceID: "ws_a",
		ResourcePlaneID: "rp_a", Observed: domain.ObservedWorkspaceFailed, Err: errors.New("/private/cloud-hypervisor/token=secret")})
	if entry.Error != "allocation reconciliation failed" || entry.TenantID != "tenant_a" {
		t.Fatalf("reconciliation report leaked raw failure details: %+v", entry)
	}
}

func TestCleanAbsoluteRequiresCanonicalPath(t *testing.T) {
	for _, path := range []string{"/opt/hako/state", "/tmp/x"} {
		if !cleanAbsolute(path) {
			t.Fatalf("clean absolute path rejected: %q", path)
		}
	}
	for _, path := range []string{"", "relative", "/tmp/../state", " /tmp/state"} {
		if cleanAbsolute(path) {
			t.Fatalf("invalid path accepted: %q", path)
		}
	}
}
