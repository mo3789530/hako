//go:build linux

package workspaceruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/resourcecontroller"
	"github.com/mo3789530/hako/internal/workspacejournal"
)

func TestDecodeAndValidateFirecrackerLaunchSpec(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	initrd := filepath.Join(dir, "initrd")
	for _, path := range []string{kernel, initrd} {
		if err := os.WriteFile(path, []byte("test-image"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	kernelSHA, err := fileSHA256(kernel)
	if err != nil {
		t.Fatal(err)
	}
	initrdSHA, err := fileSHA256(initrd)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(FirecrackerLocalLaunchSpec{KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
		InitrdPath: initrd, InitrdSHA256: initrdSHA})
	if err != nil {
		t.Fatal(err)
	}
	spec, err := decodeFirecrackerLaunchSpec(payload)
	if err != nil {
		t.Fatal(err)
	}
	if err := prepareFirecrackerLaunchSpec(&spec); err != nil {
		t.Fatal(err)
	}
	if spec.VCPUCount != 1 || spec.MemoryMiB != 512 || spec.WorkspaceDiskSizeMiB != 1024 || spec.KernelBootArgs == "" {
		t.Fatalf("launch defaults were not applied: %+v", spec)
	}

	for _, invalid := range []string{
		`{"kernel_image_path":"/kernel","initrd_path":"/initrd","extra":true}`,
		`{"kernel_image_path":"/kernel","initrd_path":"/initrd"} {}`,
		`{"kernel_image_path":"relative","initrd_path":"/initrd"}`,
	} {
		decoded, err := decodeFirecrackerLaunchSpec([]byte(invalid))
		if err == nil {
			if err := prepareFirecrackerLaunchSpec(&decoded); err == nil {
				t.Fatalf("invalid launch specification was accepted: %s", invalid)
			}
		}
	}

	tooLarge := spec
	tooLarge.WorkspaceDiskSizeMiB = (1 << 20) + 1
	if err := prepareFirecrackerLaunchSpec(&tooLarge); err == nil {
		t.Fatal("oversized Workspace disk was accepted")
	}
	badArgs := spec
	badArgs.KernelBootArgs = "console=ttyS0\nrdinit=/init"
	if err := prepareFirecrackerLaunchSpec(&badArgs); err == nil {
		t.Fatal("multiline kernel boot arguments were accepted")
	}
	if err := os.WriteFile(kernel, []byte("replaced-kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	replacedImage := spec
	if err := prepareFirecrackerLaunchSpec(&replacedImage); err == nil {
		t.Fatal("image content changed after SHA-256 pin but launch was accepted")
	}
}

func TestFirecrackerUnixAPIClient(t *testing.T) {
	dir := t.TempDir()
	socket := filepath.Join(dir, "api.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/" {
			http.Error(w, "unexpected request", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"state":"Running"}`))
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()

	driver := &FirecrackerLocalDriver{}
	state, err := driver.vmState(context.Background(), dir)
	if err != nil || state != "Running" {
		t.Fatalf("vmState = %q, %v", state, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := driver.api(ctx, dir, http.MethodPost, "/unsupported", nil, nil); err == nil || !strings.Contains(err.Error(), "HTTP 400") {
		t.Fatalf("unexpected API route response error=%v", err)
	}
}

func TestPinFirecrackerLaunchSpecIsImmutableAndPrivate(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"kernel_image_path":"/opt/kernel","initrd_path":"/opt/initrd"}`)
	if err := pinFirecrackerLaunchSpec(dir, payload); err != nil {
		t.Fatal(err)
	}
	if err := pinFirecrackerLaunchSpec(dir, payload); err != nil {
		t.Fatalf("identical pin retry: %v", err)
	}
	if err := pinFirecrackerLaunchSpec(dir, []byte(`{"kernel_image_path":"/changed"}`)); !errors.Is(err, workspacejournal.ErrConflict) {
		t.Fatalf("changed launch payload error=%v, want conflict", err)
	}
	info, err := os.Stat(filepath.Join(dir, "launch-spec.json"))
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("launch spec mode=%v, err=%v; want 0600", info, err)
	}
}

func TestFirecrackerGuestQuiescenceRecordPinsAllocationAndPayload(t *testing.T) {
	dir := t.TempDir()
	payload := []byte(`{"kernel_image_path":"/kernel","initrd_path":"/initrd"}`)
	if err := pinFirecrackerLaunchSpec(dir, payload); err != nil {
		t.Fatal(err)
	}
	driver := &FirecrackerLocalDriver{}
	if err := driver.writeGuestQuiescedRecord(dir, "fc_test"); err != nil {
		t.Fatal(err)
	}
	if !driver.hasGuestQuiesced(dir, "fc_test") {
		t.Fatal("valid guest quiescence record was rejected")
	}
	if driver.hasGuestQuiesced(dir, "fc_other") {
		t.Fatal("quiescence record accepted a foreign allocation")
	}
	if err := pinFirecrackerLaunchSpec(dir, []byte(`{"kernel_image_path":"/changed"}`)); !errors.Is(err, workspacejournal.ErrConflict) {
		t.Fatalf("changed payload pin error=%v, want conflict", err)
	}
	path := filepath.Join(dir, strings.TrimPrefix(localGuestQuiescedPath, "/"))
	if err := os.Chmod(path, 0644); err != nil {
		t.Fatal(err)
	}
	if driver.hasGuestQuiesced(dir, "fc_test") {
		t.Fatal("quiescence record with group/world-readable permissions was accepted")
	}
}

func TestFirecrackerExitedCleanlyRequiresSuccessfulExitRecord(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "firecracker.log")
	if err := os.WriteFile(path, []byte("Firecracker exiting successfully. exit_code=0\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if !firecrackerExitedCleanly(path) {
		t.Fatal("successful Firecracker shutdown was not recognized")
	}
	if err := os.WriteFile(path, []byte("Firecracker exiting with error. exit_code=1\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if firecrackerExitedCleanly(path) {
		t.Fatal("failed Firecracker exit was accepted as guest quiescence")
	}
}

func TestStaticFirecrackerLocalLaunchResolverPinsAndCopiesPayload(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	initrd := filepath.Join(dir, "initrd")
	if err := os.WriteFile(kernel, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0600); err != nil {
		t.Fatal(err)
	}
	kernelSHA, err := fileSHA256(kernel)
	if err != nil {
		t.Fatal(err)
	}
	initrdSHA, err := fileSHA256(initrd)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(FirecrackerLocalLaunchSpec{KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
		InitrdPath: initrd, InitrdSHA256: initrdSHA})
	if err != nil {
		t.Fatal(err)
	}
	resolver, err := NewStaticFirecrackerLocalLaunchResolver(payload)
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{Type: domain.OperationEnsureRunning, TenantID: domain.TenantID("tenant"),
		WorkspaceID: domain.WorkspaceID("workspace"), ResourcePlaneID: domain.ResourcePlaneID("plane")}
	resolved, err := resolver.ResolveLocal(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	resolved[0] = 'x'
	if payload[0] != '{' {
		t.Fatal("resolver returned its internal launch payload buffer")
	}
	command.Type = domain.OperationResume
	if _, err := resolver.ResolveLocal(context.Background(), command); err == nil {
		t.Fatal("resolver supplied launch data for a non-ensure operation")
	}
}

func TestFirecrackerLocalWorkspaceLaunchResolverBindsIdentityAndReplacementGeneration(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	initrd := filepath.Join(dir, "initrd")
	if err := os.WriteFile(kernel, []byte("kernel"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(initrd, []byte("initrd"), 0600); err != nil {
		t.Fatal(err)
	}
	kernelSHA, _ := fileSHA256(kernel)
	initrdSHA, _ := fileSHA256(initrd)
	base, err := json.Marshal(FirecrackerLocalLaunchSpec{KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
		InitrdPath: initrd, InitrdSHA256: initrdSHA, KernelBootArgs: "console=ttyS0 rdinit=/init"})
	if err != nil {
		t.Fatal(err)
	}
	store := newMemoryStore()
	resolver, err := NewFirecrackerLocalWorkspaceLaunchResolver(base, store, "https://gateway.local.test")
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{SchemaVersion: 2, Type: domain.OperationEnsureRunning,
		TenantID: "tenant_local", WorkspaceID: "workspace_local", ResourcePlaneID: "rp_local",
		WorkspaceRevision: 1, OperationID: "op_create"}
	resolved, err := resolver.ResolveLocal(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	var spec FirecrackerLocalLaunchSpec
	if err := json.Unmarshal(resolved, &spec); err != nil {
		t.Fatal(err)
	}
	providerID := localProviderID(workspacejournal.Journal{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, AllocationGeneration: 1})
	for _, expected := range []string{"console=ttyS0", "rdinit=/init", "hako.tenant_id=tenant_local", "hako.workspace_id=workspace_local",
		"hako.resource_plane_id=rp_local", "hako.microvm_id=" + providerID, "hako.gateway_origin=https://gateway.local.test"} {
		if !strings.Contains(spec.KernelBootArgs, expected) {
			t.Fatalf("resolved kernel args lack %q: %q", expected, spec.KernelBootArgs)
		}
	}

	now := time.Now().UTC()
	initial, err := workspacejournal.NewWithLaunchSpec(workspacejournal.Command{TenantID: command.TenantID,
		WorkspaceID: command.WorkspaceID, ResourcePlaneID: command.ResourcePlaneID, WorkspaceRevision: 1,
		OperationID: "op_create", Type: domain.OperationEnsureRunning}, "launch_0123456789abcdef0123456789abcdef", base, now)
	if err != nil {
		t.Fatal(err)
	}
	terminated := initial
	terminated.State, terminated.ProviderID = workspacejournal.Terminated, providerID
	terminated.CleanupState = workspacejournal.CleanupConfirmed
	terminated.Revision++
	terminated.UpdatedAt = now.Add(time.Second)
	if err := terminated.Validate(); err != nil {
		t.Fatalf("test terminated journal invalid: %v", err)
	}
	store.data[storeKey(command.TenantID, command.WorkspaceID, command.ResourcePlaneID)] = workspacejournal.Snapshot{Journal: terminated, ETag: "terminated"}
	command.WorkspaceRevision = 2
	command.OperationID = "op_recreate"
	replacement, err := resolver.ResolveLocal(context.Background(), command)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(replacement, &spec); err != nil {
		t.Fatal(err)
	}
	replacementID := localProviderID(workspacejournal.Journal{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID,
		ResourcePlaneID: command.ResourcePlaneID, AllocationGeneration: 2})
	if replacementID == providerID || !strings.Contains(spec.KernelBootArgs, "hako.microvm_id="+replacementID) {
		t.Fatalf("replacement Guest identity did not advance allocation generation: id=%q args=%q", replacementID, spec.KernelBootArgs)
	}
}

func TestLocalGuestIdentityFromKernelArgsRequiresOneCompletePinnedIdentity(t *testing.T) {
	valid := "console=ttyS0 hako.tenant_id=tenant_local hako.workspace_id=workspace_local " +
		"hako.resource_plane_id=rp_local hako.microvm_id=fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa " +
		"hako.gateway_origin=https://gateway.local.test"
	identity, err := localGuestIdentityFromKernelArgs(valid)
	if err != nil || identity.tenantID != "tenant_local" || identity.workspaceID != "workspace_local" ||
		identity.resourcePlaneID != "rp_local" || identity.microVMID != "fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa" {
		t.Fatalf("valid Workspace Guest identity rejected: %+v err=%v", identity, err)
	}
	for name, args := range map[string]string{
		"missing identity":         "console=ttyS0",
		"duplicate identity":       valid + " hako.tenant_id=tenant_foreign",
		"unknown Hako argument":    valid + " hako.extra=value",
		"invalid MicroVM ID":       strings.Replace(valid, "fc_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "foreign_vm", 1),
		"invalid Gateway origin":   strings.Replace(valid, "https://gateway.local.test", "http://gateway.local.test", 1),
		"empty identity component": strings.Replace(valid, "hako.workspace_id=workspace_local", "hako.workspace_id=", 1),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := localGuestIdentityFromKernelArgs(args); err == nil {
				t.Fatal("invalid Guest identity was accepted")
			}
		})
	}
}

func TestFirecrackerLocalWorkspaceLaunchResolverRejectsUntrustedBaseArgsAndIdentity(t *testing.T) {
	dir := t.TempDir()
	kernel := filepath.Join(dir, "kernel")
	initrd := filepath.Join(dir, "initrd")
	_ = os.WriteFile(kernel, []byte("kernel"), 0600)
	_ = os.WriteFile(initrd, []byte("initrd"), 0600)
	kernelSHA, _ := fileSHA256(kernel)
	initrdSHA, _ := fileSHA256(initrd)
	for name, mutate := range map[string]func(*FirecrackerLocalLaunchSpec){
		"reserved identity argument": func(spec *FirecrackerLocalLaunchSpec) { spec.KernelBootArgs += " hako.tenant_id=foreign" },
	} {
		t.Run(name, func(t *testing.T) {
			spec := FirecrackerLocalLaunchSpec{KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
				InitrdPath: initrd, InitrdSHA256: initrdSHA, KernelBootArgs: "console=ttyS0"}
			mutate(&spec)
			payload, _ := json.Marshal(spec)
			if _, err := NewFirecrackerLocalWorkspaceLaunchResolver(payload, newMemoryStore(), "https://gateway.local.test"); err == nil {
				t.Fatal("reserved identity argument was accepted")
			}
		})
	}
	specPayload, _ := json.Marshal(FirecrackerLocalLaunchSpec{KernelImagePath: kernel, KernelImageSHA256: kernelSHA,
		InitrdPath: initrd, InitrdSHA256: initrdSHA})
	resolver, err := NewFirecrackerLocalWorkspaceLaunchResolver(specPayload, newMemoryStore(), "https://gateway.local.test")
	if err != nil {
		t.Fatal(err)
	}
	command := resourcecontroller.Command{SchemaVersion: 2, Type: domain.OperationEnsureRunning, TenantID: "../tenant",
		WorkspaceID: "workspace_local", ResourcePlaneID: "rp_local", WorkspaceRevision: 1, OperationID: "op_create"}
	if _, err := resolver.ResolveLocal(context.Background(), command); err == nil {
		t.Fatal("unsafe Tenant identity was embedded in Guest kernel args")
	}
}

func TestDialFirecrackerLocalGuestUsesOnlyTheFixedVsockPort(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	socket := filepath.Join(dir, strings.TrimPrefix(localFirecrackerVsockPath, "/"))
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.Chmod(socket, 0600); err != nil {
		t.Fatal(err)
	}
	serverErr := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			serverErr <- err
			return
		}
		defer conn.Close()
		request, err := bufio.NewReader(conn).ReadString('\n')
		if err != nil || request != "CONNECT 7000\n" {
			serverErr <- errors.New("host did not request the fixed Guest Agent port")
			return
		}
		// Firecracker's ACK and an eager Guest write may arrive in one packet.
		if _, err := io.WriteString(conn, "OK 1073741824\nready"); err != nil {
			serverErr <- err
			return
		}
		payload := make([]byte, 4)
		if _, err := io.ReadFull(conn, payload); err != nil {
			serverErr <- err
			return
		}
		if string(payload) != "ping" {
			serverErr <- errors.New("unexpected Guest request")
			return
		}
		_, err = io.WriteString(conn, "pong")
		serverErr <- err
	}()

	conn, err := DialFirecrackerLocalGuest(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	ready := make([]byte, len("ready"))
	if _, err := io.ReadFull(conn, ready); err != nil || string(ready) != "ready" {
		t.Fatalf("Guest bytes after handshake=%q error=%v", ready, err)
	}
	if _, err := io.WriteString(conn, "ping"); err != nil {
		t.Fatal(err)
	}
	response := make([]byte, 4)
	if _, err := io.ReadFull(conn, response); err != nil || string(response) != "pong" {
		t.Fatalf("Guest response=%q error=%v", response, err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestLocalGuestFailureStageReadsOnlySafeStageMarker(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, localFirecrackerLogPath)
	if err := os.WriteFile(logPath, []byte("Hako local Workspace Guest stopped (failure_class=guest_init stage=\"agent_start_ssh\")\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if stage := localGuestFailureStage(dir); stage != "agent_start_ssh" {
		t.Fatalf("failure stage=%q", stage)
	}
	if err := os.WriteFile(logPath, []byte("password=never-log\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if stage := localGuestFailureStage(dir); stage != "" {
		t.Fatalf("unrecognized Guest output became a failure stage: %q", stage)
	}
}
