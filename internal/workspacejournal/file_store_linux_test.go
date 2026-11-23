//go:build linux

package workspacejournal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestFileStorePersistsAndFencesWorkspaceLifecycleAcrossRestart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 11, 21, 4, 0, 0, 0, time.UTC)
	store, err := NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	command := command(domain.OperationEnsureRunning, 1, "op_file_1")
	payload := []byte(`{"kernel":"/opt/hako/kernel","initrd":"/opt/hako/guest.cpio"}`)
	initial, err := NewWithLaunchSpec(command, "launch_1", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	created, err := store.Create(ctx, initial)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	path := filepath.Join(store.root, string(command.ResourcePlaneID), string(command.TenantID), string(command.WorkspaceID)+".json")
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("journal file mode=%v error=%v; want 0600", info, err)
	}

	// Re-open the store as a restarted controller would and verify the durable
	// immutable launch request and revision checkpoint.
	restarted, err := NewFileStore(store.root)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := restarted.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID)
	if err != nil || loaded.ETag != created.ETag || string(loaded.Journal.LaunchSpecPayload) != string(payload) {
		t.Fatalf("restarted journal=%+v err=%v", loaded, err)
	}

	running, err := Advance(loaded, Running, "fc-123", CleanupNotRequired, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	runningSnapshot, err := restarted.CompareAndSwap(ctx, loaded, running)
	if err != nil {
		t.Fatalf("persist running checkpoint: %v", err)
	}
	if _, err := restarted.CompareAndSwap(ctx, loaded, running); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale ETag was accepted: %v", err)
	}

	deleteCommand := command
	deleteCommand.Type, deleteCommand.WorkspaceRevision, deleteCommand.OperationID = domain.OperationDelete, 2, "op_file_2"
	deleting, err := ApplyCommand(runningSnapshot, deleteCommand, "", "", now.Add(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	deletedSnapshot, err := restarted.CompareAndSwap(ctx, runningSnapshot, deleting)
	if err != nil {
		t.Fatalf("persist delete tombstone: %v", err)
	}
	listed, err := restarted.ListJournals(ctx, 10)
	if err != nil || len(listed) != 1 || !listed[0].Journal.Tombstone || listed[0].ETag != deletedSnapshot.ETag {
		t.Fatalf("reconciliation listing=%+v err=%v", listed, err)
	}
	if _, err := restarted.Get(ctx, command.TenantID, command.WorkspaceID, command.ResourcePlaneID); err != nil {
		t.Fatalf("confirmed in-process delete journal unexpectedly disappeared: %v", err)
	}
}

func TestFileStoreSerializesConcurrentCompareAndSwap(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	store, err := NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := command(domain.OperationEnsureRunning, 1, "op_race_1")
	initial, err := NewWithLaunchSpec(cmd, "launch_race", []byte(`{"image":"test"}`), now)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Create(ctx, initial)
	if err != nil {
		t.Fatal(err)
	}
	toRunning, err := Advance(snapshot, Running, "fc-race", CleanupNotRequired, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	toFailed, err := Advance(snapshot, Failed, "", CleanupNotRequired, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	results := make(chan error, 2)
	var workers sync.WaitGroup
	for _, next := range []Journal{toRunning, toFailed} {
		workers.Add(1)
		go func(candidate Journal) {
			defer workers.Done()
			<-start
			_, err := store.CompareAndSwap(ctx, snapshot, candidate)
			results <- err
		}(next)
	}
	close(start)
	workers.Wait()
	close(results)
	twins, conflicts := 0, 0
	for err := range results {
		if err == nil {
			twins++
		} else if errors.Is(err, ErrConflict) {
			conflicts++
		} else {
			t.Fatalf("unexpected CAS error: %v", err)
		}
	}
	if twins != 1 || conflicts != 1 {
		t.Fatalf("CAS winners=%d conflicts=%d; want one each", twins, conflicts)
	}
}

func TestFileStoreLockWaitHonorsContextCancellation(t *testing.T) {
	store, err := NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	path, err := store.journalPath("tenant_lock", "ws_lock", "rp_lock", true)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := store.lock(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
	defer cancel()
	if _, err := store.lock(ctx, path); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("lock wait error=%v, want context deadline", err)
	}
}

func TestFileStoreSerializesWorkspaceOperationsAcrossStoreInstances(t *testing.T) {
	root := filepath.Join(t.TempDir(), "journals")
	first, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	unlock, err := first.LockOperation(context.Background(), "tenant_oplock", "ws_oplock", "rp_oplock")
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := second.LockOperation(ctx, "tenant_oplock", "ws_oplock", "rp_oplock"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second operation lock error=%v, want context deadline", err)
	}
}

func TestNewFileStoreRejectsSymlinkRootAndListingBounds(t *testing.T) {
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "journal-link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFileStore(link); err == nil {
		t.Fatal("journal root symlink was accepted")
	}
	store, err := NewFileStore(filepath.Join(t.TempDir(), "journals"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ListJournals(context.Background(), 0); err == nil {
		t.Fatal("zero journal listing limit was accepted")
	}
}
