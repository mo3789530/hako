package workloadagent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestFilesystemExecutionResolverUsesOnlySignedBindingAndConfiguredPath(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	root := filepath.Join(t.TempDir(), "workspace", "source")
	resolver, err := NewFilesystemExecutionResolver(root, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 2,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	deadline := now.Add(7 * time.Hour).UTC()
	claims := TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationStart,
		Execution: &ExecutionBinding{SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: deadline}}
	plan, err := resolver.ResolveExecution(context.Background(), claims)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Identity != id || plan.RepositoryPath != root || plan.SourceDigest != claims.Execution.SourceDigest ||
		plan.CommitSHA != claims.Execution.CommitSHA || !plan.Deadline.Equal(deadline) {
		t.Fatalf("resolved execution plan = %+v", plan)
	}
	changed := claims
	changed.Identity = IdentityFromCommand(id)
	changed.Identity.WorkloadRunID = "../other-path"
	if _, err := resolver.ResolveExecution(context.Background(), changed); !errors.Is(err, ErrExecutionPlan) {
		t.Fatalf("invalid identity accepted: %v", err)
	}
}

func TestFilesystemExecutionResolverRejectsUnboundAndExpiredPlans(t *testing.T) {
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	resolver, err := NewFilesystemExecutionResolver(filepath.Join(t.TempDir(), "checkout"), func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	base := TicketClaims{MicroVMID: "microvm_1", Identity: IdentityFromCommand(id), Operation: OperationStart,
		Execution: &ExecutionBinding{SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}}
	tests := []struct {
		name   string
		claims TicketClaims
	}{
		{name: "missing binding", claims: TicketClaims{MicroVMID: base.MicroVMID, Identity: base.Identity, Operation: OperationStart}},
		{name: "read operation", claims: TicketClaims{MicroVMID: base.MicroVMID, Identity: base.Identity, Operation: OperationStatus, Execution: base.Execution}},
		{name: "expired", claims: TicketClaims{MicroVMID: base.MicroVMID, Identity: base.Identity, Operation: OperationStart,
			Execution: &ExecutionBinding{SourceDigest: base.Execution.SourceDigest, CommitSHA: base.Execution.CommitSHA, Deadline: now}}},
		{name: "beyond microvm lifetime", claims: TicketClaims{MicroVMID: base.MicroVMID, Identity: base.Identity, Operation: OperationStart,
			Execution: &ExecutionBinding{SourceDigest: base.Execution.SourceDigest, CommitSHA: base.Execution.CommitSHA, Deadline: now.Add(8*time.Hour + time.Second)}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := resolver.ResolveExecution(context.Background(), test.claims); !errors.Is(err, ErrExecutionPlan) {
				t.Fatalf("invalid claims accepted: %v", err)
			}
		})
	}
	if _, err := resolver.ResolveExecution(context.Background(), base); err != nil {
		t.Fatalf("valid signed execution rejected: %v", err)
	}
}

func TestNewFilesystemExecutionResolverRejectsUnsafePaths(t *testing.T) {
	for _, path := range []string{"", string(filepath.Separator), "relative", "/tmp/source\\escape", "/tmp/source\nagent"} {
		if _, err := NewFilesystemExecutionResolver(path, nil); err == nil {
			t.Fatalf("unsafe source directory %q accepted", path)
		}
	}
}
