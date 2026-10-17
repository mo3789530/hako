//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type mountCommandCall struct {
	binary string
	args   []string
}

type mountCommandStub struct {
	infoPath string
	calls    []mountCommandCall
	err      error
}

func (s *mountCommandStub) Run(_ context.Context, binary string, args ...string) error {
	s.calls = append(s.calls, mountCommandCall{binary: binary, args: append([]string(nil), args...)})
	if s.err != nil {
		return s.err
	}
	if binary == workspaceUmountBinary {
		return os.WriteFile(s.infoPath, nil, 0600)
	}
	return os.WriteFile(s.infoPath, []byte("36 25 0:42 / /workspace rw,relatime - nfs4 127.0.0.1:/ rw,vers=4.1\n"), 0600)
}

func TestLinuxEFSMountUsesFixedTLSAccessPointAndIsIdempotent(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	markerPath := filepath.Join(t.TempDir(), "mount-marker.json")
	if err := os.WriteFile(infoPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &mountCommandStub{infoPath: infoPath}
	executor := &LinuxEFSMountExecutor{runner: runner, mountBinary: "/bin/mount", umountBinary: workspaceUmountBinary, mountInfoPath: infoPath,
		markerPath: markerPath, workspacePath: "/workspace", isRoot: func() bool { return true },
		makeDirectory: func(string, os.FileMode) error { return nil }}
	spec := testMountSpec()
	if err := executor.MountAndVerify(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	wantArgs := []string{"-t", "efs", "-o", "tls,iam,accesspoint=fsap-0123456789abcdef0,region=us-east-1", "fs-0123456789abcdef0:/", "/workspace"}
	if len(runner.calls) != 1 || runner.calls[0].binary != "/bin/mount" || !reflect.DeepEqual(runner.calls[0].args, wantArgs) {
		t.Fatalf("mount command = %+v, want %q %q", runner.calls, "/bin/mount", wantArgs)
	}
	marker, found, err := readWorkspaceMountMarker(markerPath)
	if err != nil || !found || marker != spec {
		t.Fatalf("mount intent marker = %+v found=%v err=%v", marker, found, err)
	}
	markerInfo, err := os.Stat(markerPath)
	if err != nil || markerInfo.Mode().Perm() != 0600 {
		t.Fatalf("mount intent marker permissions = %v, %v", markerInfo, err)
	}
	if err := executor.MountAndVerify(context.Background(), spec); err != nil || len(runner.calls) != 1 {
		t.Fatalf("same mount retry was not idempotent: calls=%d err=%v", len(runner.calls), err)
	}
	other := spec
	other.AccessPointID = "fsap-1123456789abcdef0"
	if err := executor.MountAndVerify(context.Background(), other); err == nil || len(runner.calls) != 1 {
		t.Fatal("different Access Point replaced the pinned Workspace mount")
	}
}

func TestLinuxEFSMountRecordsIntentBeforeCommandAndRetriesFailure(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	markerPath := filepath.Join(t.TempDir(), "mount-marker.json")
	if err := os.WriteFile(infoPath, nil, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &mountCommandStub{infoPath: infoPath, err: errors.New("mount failed")}
	executor := &LinuxEFSMountExecutor{runner: runner, mountBinary: "/bin/mount", umountBinary: workspaceUmountBinary, mountInfoPath: infoPath,
		markerPath: markerPath, workspacePath: "/workspace", isRoot: func() bool { return true },
		makeDirectory: func(string, os.FileMode) error { return nil }}
	spec := testMountSpec()
	if err := executor.MountAndVerify(context.Background(), spec); err == nil {
		t.Fatal("mount failure was reported as successful")
	}
	if marker, found, err := readWorkspaceMountMarker(markerPath); err != nil || !found || marker != spec {
		t.Fatalf("retry identity was not persisted before mount side effect: %+v found=%v err=%v", marker, found, err)
	}
	runner.err = nil
	if err := executor.MountAndVerify(context.Background(), spec); err != nil || len(runner.calls) != 2 {
		t.Fatalf("same-binding retry did not recover mount: calls=%d err=%v", len(runner.calls), err)
	}
}

func TestLinuxEFSMountRejectsUnownedMountAndNonRoot(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	markerPath := filepath.Join(t.TempDir(), "mount-marker.json")
	runner := &mountCommandStub{infoPath: infoPath}
	if err := os.WriteFile(infoPath, []byte("36 25 0:42 / /workspace rw - nfs4 127.0.0.1:/ rw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	executor := &LinuxEFSMountExecutor{runner: runner, mountBinary: "/bin/mount", umountBinary: workspaceUmountBinary, mountInfoPath: infoPath,
		markerPath: markerPath, workspacePath: "/workspace", isRoot: func() bool { return true },
		makeDirectory: func(string, os.FileMode) error { return nil }}
	if err := executor.MountAndVerify(context.Background(), testMountSpec()); err == nil || len(runner.calls) != 0 {
		t.Fatal("adopted a pre-existing mount without an Hako-owned marker")
	}
	executor.isRoot = func() bool { return false }
	if err := executor.MountAndVerify(context.Background(), testMountSpec()); err == nil || len(runner.calls) != 0 {
		t.Fatal("allowed an unprivileged mount request")
	}
}

func TestLinuxEFSMountUnmountsOnlyOwnedMountAndIsIdempotent(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	markerPath := filepath.Join(t.TempDir(), "mount-marker.json")
	if err := os.WriteFile(infoPath, []byte("36 25 0:42 / /workspace rw - nfs4 127.0.0.1:/ rw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceMountMarker(markerPath, testMountSpec()); err != nil {
		t.Fatal(err)
	}
	runner := &mountCommandStub{infoPath: infoPath}
	executor := &LinuxEFSMountExecutor{runner: runner, mountBinary: "/bin/mount", umountBinary: workspaceUmountBinary,
		mountInfoPath: infoPath, markerPath: markerPath, workspacePath: "/workspace", isRoot: func() bool { return true }}
	if err := executor.Unmount(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(runner.calls) != 1 || runner.calls[0].binary != workspaceUmountBinary || !reflect.DeepEqual(runner.calls[0].args, []string{"/workspace"}) {
		t.Fatalf("unmount command = %+v", runner.calls)
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mount ownership marker was not removed: %v", err)
	}
	if err := executor.Unmount(context.Background()); err != nil || len(runner.calls) != 1 {
		t.Fatalf("unmount retry was not idempotent: calls=%d err=%v", len(runner.calls), err)
	}
}

func TestLinuxEFSMountUnmountRetainsMarkerOnFailure(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	markerPath := filepath.Join(t.TempDir(), "mount-marker.json")
	if err := os.WriteFile(infoPath, []byte("36 25 0:42 / /workspace rw - nfs4 127.0.0.1:/ rw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := writeWorkspaceMountMarker(markerPath, testMountSpec()); err != nil {
		t.Fatal(err)
	}
	runner := &mountCommandStub{infoPath: infoPath, err: errors.New("busy")}
	executor := &LinuxEFSMountExecutor{runner: runner, umountBinary: workspaceUmountBinary, mountInfoPath: infoPath,
		markerPath: markerPath, workspacePath: "/workspace", isRoot: func() bool { return true }}
	if err := executor.Unmount(context.Background()); err == nil {
		t.Fatal("reported a busy mount as unmounted")
	}
	if _, found, err := readWorkspaceMountMarker(markerPath); err != nil || !found {
		t.Fatalf("failed unmount lost its ownership marker: found=%v err=%v", found, err)
	}
}

func TestWorkspaceMountInfoRejectsWrongFilesystemAtTarget(t *testing.T) {
	infoPath := filepath.Join(t.TempDir(), "mountinfo")
	if err := os.WriteFile(infoPath, []byte("36 25 0:42 / /workspace rw - ext4 /dev/vda rw\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := workspacePathMounted(infoPath, "/workspace"); err == nil {
		t.Fatal("accepted a non-NFS filesystem at /workspace")
	}
}

func testMountSpec() WorkspaceMountSpec {
	return WorkspaceMountSpec{TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlaneID: "rp-1",
		FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/0123456789abcdef/1123456789abcdef/2123456789abcdef", MountPath: "/workspace", UID: 100123, GID: 100123, TLS: true, IAM: true}
}
