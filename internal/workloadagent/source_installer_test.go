package workloadagent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

func TestFilesystemSourceInstallerVerifiesBeforeInstallAndFencesAttempt(t *testing.T) {
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.go"), []byte("package main\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), sourceRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	installer, err := NewFilesystemSourceInstaller(filepath.Join(t.TempDir(), "checkout"), 0)
	if err != nil {
		t.Fatal(err)
	}
	commit := strings.Repeat("b", 40)
	claims := sourceTicketClaims(archive.SHA256(), commit)
	if installer.Ready(context.Background(), claims, archive.SHA256(), commit) {
		t.Fatal("source unexpectedly ready before install")
	}
	if err := installer.Install(context.Background(), claims, archive.SHA256(), commit, archive.Reader(), archive.Size()); err != nil {
		t.Fatal(err)
	}
	if !installer.Ready(context.Background(), claims, archive.SHA256(), commit) {
		t.Fatal("verified source was not marked ready")
	}
	if installer.Ready(context.Background(), claims, archive.SHA256(), strings.Repeat("c", 40)) {
		t.Fatal("source installation was reused for a different immutable commit")
	}
	otherToken := claims
	otherToken.Identity.IdempotencyToken = "idem_other"
	if installer.Ready(context.Background(), otherToken, archive.SHA256(), commit) {
		t.Fatal("source installation was reused for a different idempotency identity")
	}
	content, err := os.ReadFile(filepath.Join(installer.destination, "main.go"))
	if err != nil || string(content) != "package main\n" {
		t.Fatalf("installed source content = %q err=%v", content, err)
	}
	if err := installer.Install(context.Background(), claims, archive.SHA256(), commit, archive.Reader(), archive.Size()); err != nil {
		t.Fatalf("same attempt/digest retry was not idempotent: %v", err)
	}
	other := claims
	other.Identity.AttemptID = "attempt_other"
	if err := installer.Install(context.Background(), other, archive.SHA256(), commit, archive.Reader(), archive.Size()); !errors.Is(err, ErrAttemptConflict) {
		t.Fatalf("foreign attempt source install error = %v", err)
	}
	if !installer.Ready(context.Background(), claims, archive.SHA256(), commit) {
		t.Fatal("foreign attempt changed the installed source binding")
	}
}

func TestFilesystemSourceInstallerRejectsBadDigestAndWrongOperation(t *testing.T) {
	root := t.TempDir()
	sourceRoot := t.TempDir()
	if err := os.WriteFile(filepath.Join(sourceRoot, "main.go"), []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), sourceRoot, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	installer, err := NewFilesystemSourceInstaller(filepath.Join(root, "checkout"), 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	claims := sourceTicketClaims(archive.SHA256(), strings.Repeat("b", 40))
	if err := installer.Install(context.Background(), claims, strings.Repeat("0", 64), strings.Repeat("b", 40), archive.Reader(), archive.Size()); !errors.Is(err, ErrSourceInstall) {
		t.Fatalf("bad digest error = %v", err)
	}
	if _, err := os.Stat(installer.destination); !os.IsNotExist(err) {
		t.Fatal("bad digest created a repository directory")
	}
	wrongOperation := claims
	wrongOperation.Operation = OperationStart
	if err := installer.Install(context.Background(), wrongOperation, archive.SHA256(), strings.Repeat("b", 40), archive.Reader(), archive.Size()); !errors.Is(err, ErrSourceInstall) {
		t.Fatalf("Start ticket installed source: %v", err)
	}
}

func sourceTicketClaims(digest, commit string) TicketClaims {
	identity := IdentityFromCommand(workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1",
		AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"})
	return TicketClaims{Version: 1, MicroVMID: "vm_1", Identity: identity, Operation: OperationSource,
		Execution: &ExecutionBinding{SourceDigest: digest, CommitSHA: commit, Deadline: time.Now().UTC().Add(time.Hour)},
		IssuedAt:  time.Now().Add(-time.Minute).Unix(), ExpiresAt: time.Now().Add(time.Minute).Unix(), Nonce: strings.Repeat("a", 32)}
}
