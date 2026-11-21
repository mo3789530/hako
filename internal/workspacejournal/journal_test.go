package workspacejournal

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacestorage"
)

func TestJournalPinsLaunchAndRoundTripsStrictly(t *testing.T) {
	now := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	cmd := command(domain.OperationEnsureRunning, 1, "op_1")
	journal, err := New(cmd, "token_1", DigestLaunchSpec([]byte(`{"image":"sha256:abc"}`)), now)
	if err != nil {
		t.Fatal(err)
	}
	data, err := Encode(journal)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := Decode(data)
	if err != nil || decoded.WorkspaceID != cmd.WorkspaceID || decoded.AllocationGeneration != 1 {
		t.Fatalf("round-trip = %+v, %v", decoded, err)
	}
	if _, err := Decode(append(data, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing JSON value was accepted")
	}
	if _, err := Decode(append(data, []byte(` garbage`)...)); err == nil {
		t.Fatal("malformed trailing data was accepted")
	}
	unknown := strings.Replace(string(data), `"state":"provisioning"`, `"state":"provisioning","secret":"must-not-be-accepted"`, 1)
	if _, err := Decode([]byte(unknown)); err == nil {
		t.Fatal("unknown journal field was accepted")
	}
}

func TestApplyCommandFencesStaleDuplicateAndDeletionTombstone(t *testing.T) {
	now := time.Now().UTC()
	launchHash := DigestLaunchSpec([]byte(`{"image":"image-v1"}`))
	initial, err := New(command(domain.OperationEnsureRunning, 1, "op_1"), "token_1", launchHash, now)
	if err != nil {
		t.Fatal(err)
	}
	current := Snapshot{Journal: initial, ETag: `"v1"`}
	running := initial
	running.Revision++
	running.State, running.ProviderID = Running, "microvm-1"
	running.UpdatedAt = now.Add(time.Second)
	if err := ValidateUpdate(current, running); err != nil {
		t.Fatalf("provisioning -> running: %v", err)
	}
	current = Snapshot{Journal: running, ETag: `"v2"`}

	if _, err := ApplyCommand(current, command(domain.OperationEnsureRunning, 1, "op_1"), "token_1", launchHash, now.Add(2*time.Second)); !errors.Is(err, ErrDuplicate) {
		t.Fatalf("duplicate command error = %v", err)
	}
	if _, err := ApplyCommand(current, command(domain.OperationEnsureRunning, 1, "op_1"), "other_token", launchHash, now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate operation with changed launch token error = %v", err)
	}
	if _, err := ApplyCommand(current, command(domain.OperationSuspend, 1, "op_other"), "", "", now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("same revision with different operation error = %v", err)
	}
	if _, err := ApplyCommand(current, command(domain.OperationSuspend, 0, "op_old"), "", "", now.Add(2*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("zero revision should be stale: %v", err)
	}

	suspend, err := ApplyCommand(current, command(domain.OperationSuspend, 2, "op_2"), "", "", now.Add(2*time.Second))
	if err != nil || suspend.State != Suspending || suspend.ProviderID != "microvm-1" {
		t.Fatalf("suspend checkpoint = %+v, %v", suspend, err)
	}
	suspended := suspend
	suspended.Revision++
	suspended.State = Suspended
	suspended.UpdatedAt = now.Add(3 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: suspend, ETag: `"v3"`}, suspended); err != nil {
		t.Fatalf("suspending -> suspended: %v", err)
	}
	deleting, err := ApplyCommand(Snapshot{Journal: suspended, ETag: `"v4"`}, command(domain.OperationDelete, 3, "op_3"), "", "", now.Add(4*time.Second))
	if err != nil || !deleting.Tombstone || deleting.State != Deleting || deleting.CleanupState != CleanupPending {
		t.Fatalf("delete checkpoint = %+v, %v", deleting, err)
	}
	if _, err := ApplyCommand(Snapshot{Journal: deleting, ETag: `"v5"`}, command(domain.OperationEnsureRunning, 4, "op_4"), "token_2", launchHash, now.Add(5*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ensure after deletion tombstone error = %v", err)
	}
	if _, err := ApplyCommand(Snapshot{Journal: deleting, ETag: `"v5"`}, command(domain.OperationSuspend, 2, "op_2"), "", "", now.Add(5*time.Second)); !errors.Is(err, ErrStale) {
		t.Fatalf("stale revision error = %v", err)
	}
}

func TestDeleteWithoutAllocationLeavesIrrevocableTombstone(t *testing.T) {
	journal, err := New(command(domain.OperationDelete, 9, "op_delete"), "", "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if journal.State != Deleted || !journal.Tombstone || journal.CleanupState != CleanupConfirmed {
		t.Fatalf("initial delete journal = %+v", journal)
	}
	if _, err := ApplyCommand(Snapshot{Journal: journal, ETag: `"etag"`}, command(domain.OperationEnsureRunning, 10, "op_late"), "token", DigestLaunchSpec([]byte("{}")), time.Now()); !errors.Is(err, ErrConflict) {
		t.Fatalf("late ensure after tombstone error = %v", err)
	}
}

func TestDeletionCleanupFailureRemainsDurableAndRetryable(t *testing.T) {
	now := time.Now().UTC()
	journal, err := New(command(domain.OperationEnsureRunning, 1, "op_1"), "token_1", DigestLaunchSpec([]byte("{}")), now)
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID = Running, "microvm-1"
	current := Snapshot{Journal: journal, ETag: `"running"`}
	deleting, err := ApplyCommand(current, command(domain.OperationDelete, 2, "op_delete"), "", "", now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	failedCleanup := deleting
	failedCleanup.Revision++
	failedCleanup.State, failedCleanup.CleanupState = Failed, CleanupFailed
	failedCleanup.UpdatedAt = now.Add(2 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: deleting, ETag: `"deleting"`}, failedCleanup); err != nil {
		t.Fatalf("persist cleanup failure: %v", err)
	}
	retry := failedCleanup
	retry.Revision++
	retry.State, retry.CleanupState = Deleting, CleanupPending
	retry.UpdatedAt = now.Add(3 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: failedCleanup, ETag: `"cleanup-failed"`}, retry); err != nil {
		t.Fatalf("retry cleanup: %v", err)
	}
	deleted := retry
	deleted.Revision++
	deleted.State, deleted.CleanupState = Deleted, CleanupConfirmed
	deleted.UpdatedAt = now.Add(4 * time.Second)
	if err := ValidateUpdate(Snapshot{Journal: retry, ETag: `"retrying"`}, deleted); err != nil {
		t.Fatalf("confirm cleanup: %v", err)
	}
	if _, err := ApplyCommand(Snapshot{Journal: deleted, ETag: `"deleted"`}, command(domain.OperationEnsureRunning, 3, "op_late"), "token_2", DigestLaunchSpec([]byte("{}")), now.Add(5*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ensure after completed deletion error = %v", err)
	}
}

func TestDeleteFencesInFlightProvisioningBeforeTerminationRecovery(t *testing.T) {
	now := time.Now().UTC()
	launchHash := DigestLaunchSpec([]byte(`{"image":"image-v1"}`))
	provisioning, err := New(command(domain.OperationEnsureRunning, 1, "op_1"), "token_1", launchHash, now)
	if err != nil {
		t.Fatal(err)
	}
	deleting, err := ApplyCommand(Snapshot{Journal: provisioning, ETag: `"provisioning"`}, command(domain.OperationDelete, 2, "op_delete"), "", "", now.Add(time.Second))
	if err != nil || deleting.State != Deleting || !deleting.Tombstone || deleting.CleanupState != CleanupPending || deleting.LaunchToken != "token_1" {
		t.Fatalf("delete did not fence in-flight launch: %+v, %v", deleting, err)
	}
	if _, err := ApplyCommand(Snapshot{Journal: deleting, ETag: `"deleting"`}, command(domain.OperationEnsureRunning, 3, "op_late"), "token_2", launchHash, now.Add(2*time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("ensure after in-flight delete tombstone error = %v", err)
	}
}

func TestLaunchPayloadIsValidatedAndImmutableAcrossRestartCheckpoint(t *testing.T) {
	now := time.Now().UTC()
	payload := []byte(`{"image":"image-v1","connectors":["egress"]}`)
	created, err := NewWithLaunchSpec(command(domain.OperationEnsureRunning, 1, "op_payload"), "token_payload", payload, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(created)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Decode(encoded)
	if err != nil || !bytes.Equal(restarted.LaunchSpecPayload, payload) {
		t.Fatalf("launch payload did not survive journal restart: %s, %v", restarted.LaunchSpecPayload, err)
	}
	current := Snapshot{Journal: restarted, ETag: `"v1"`}
	duplicate := command(domain.OperationEnsureRunning, 1, "op_payload")
	if _, err := ApplyCommandWithLaunchSpec(current, duplicate, "token_payload", []byte(`{"image":"image-v2"}`), now.Add(time.Second)); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate command with changed launch payload error = %v", err)
	}
	corrupt := restarted
	corrupt.LaunchSpecPayload = []byte(`{"image":"tampered"}`)
	if err := corrupt.Validate(); err == nil {
		t.Fatal("journal accepted launch payload whose digest does not match")
	}
}

func TestWorkspaceStorageBindingIsDurableAndImmutable(t *testing.T) {
	now := time.Now().UTC()
	cmd := command(domain.OperationEnsureRunning, 1, "op_storage")
	binding := journalTestBinding(cmd)
	created, err := NewWithLaunchSpecAndStorage(cmd, "token_storage", []byte(`{"image":"image-v1"}`), binding, now)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := Encode(created)
	if err != nil {
		t.Fatal(err)
	}
	restarted, err := Decode(encoded)
	if err != nil || restarted.StorageBinding == nil || *restarted.StorageBinding != *binding {
		t.Fatalf("storage binding did not survive journal restart: %+v, %v", restarted.StorageBinding, err)
	}
	current := Snapshot{Journal: restarted, ETag: `"v1"`}
	next := restarted
	next.Revision++
	next.UpdatedAt = now.Add(time.Second)
	changed := *binding
	changed.AccessPointID = "fsap-1123456789abcdef0"
	next.StorageBinding = &changed
	if err := ValidateUpdate(current, next); !errors.Is(err, ErrConflict) {
		t.Fatalf("journal accepted a changed Workspace storage binding: %v", err)
	}
	foreignWorkspace := restarted
	foreignWorkspace.WorkspaceID = "ws_other"
	if err := foreignWorkspace.Validate(); err == nil {
		t.Fatal("journal accepted an EFS binding from a different Workspace")
	}
}

func journalTestBinding(command Command) *workspacestorage.Binding {
	identity := workspacestorage.Identity{TenantID: command.TenantID, WorkspaceID: command.WorkspaceID, ResourcePlaneID: command.ResourcePlaneID}
	hash := func(value string) string {
		sum := sha256.Sum256([]byte(value))
		return hex.EncodeToString(sum[:8])
	}
	uidHash := sha256.Sum256([]byte(string(identity.TenantID) + ":" + string(identity.WorkspaceID)))
	uid := (uint32(uidHash[0])<<16 | uint32(uidHash[1])<<8 | uint32(uidHash[2])) + 100000
	return &workspacestorage.Binding{FileSystemID: "fs-0123456789abcdef0", Region: "us-east-1", AccessPointID: "fsap-0123456789abcdef0",
		RootPath: "/hako/" + hash(string(identity.ResourcePlaneID)) + "/" + hash(string(identity.TenantID)) + "/" + hash(string(identity.WorkspaceID)), UID: uid, GID: uid}
}

func TestReplacementRequiresConfirmedPriorAllocationAndNewImmutableLaunch(t *testing.T) {
	now := time.Now().UTC()
	hash1, hash2 := DigestLaunchSpec([]byte(`{"image":"v1"}`)), DigestLaunchSpec([]byte(`{"image":"v2"}`))
	journal, err := New(command(domain.OperationEnsureRunning, 1, "op_1"), "token_1", hash1, now)
	if err != nil {
		t.Fatal(err)
	}
	journal.State, journal.ProviderID = Terminated, "microvm-1"
	journal.CleanupState = CleanupConfirmed
	current := Snapshot{Journal: journal, ETag: `"v1"`}
	next, err := ApplyCommand(current, command(domain.OperationEnsureRunning, 2, "op_2"), "token_2", hash2, now.Add(time.Second))
	if err != nil || next.State != Provisioning || next.AllocationGeneration != 2 || next.ProviderID != "" {
		t.Fatalf("replacement checkpoint = %+v, %v", next, err)
	}
	bad := next
	bad.Revision++
	bad.UpdatedAt = now.Add(2 * time.Second)
	bad.LaunchToken = "token_3"
	if err := ValidateUpdate(Snapshot{Journal: next, ETag: `"v2"`}, bad); !errors.Is(err, ErrConflict) {
		t.Fatalf("launch-token mutation within generation error = %v", err)
	}
}

func command(operation domain.OperationType, revision int64, operationID domain.OperationID) Command {
	return Command{TenantID: "tenant_1", WorkspaceID: "workspace_1", ResourcePlaneID: "rp_1",
		WorkspaceRevision: revision, OperationID: operationID, Type: operation}
}
