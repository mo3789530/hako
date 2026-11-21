package workspaceagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workspacesession"
)

func TestAgentTicketBindsGatewaySessionToMicroVMAndPermission(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 17, 9, 0, 0, 0, time.UTC)
	issuer := &AgentTicketIssuer{KeyID: "rp-key-1", PrivateKey: private, TTL: time.Minute, Now: func() time.Time { return now }}
	input := AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-tokyo-1",
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell}
	token, issued, err := issuer.Issue(input)
	if err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyAgentTicket(token, map[string]ed25519.PublicKey{"rp-key-1": public}, input, now)
	if err != nil || verified != issued {
		t.Fatalf("verify ticket = %+v err=%v", verified, err)
	}
	for name, mismatch := range map[string]AgentTicket{
		"foreign Workspace": {UserID: input.UserID, TenantID: input.TenantID, WorkspaceID: "workspace-2", ResourcePlane: input.ResourcePlane, MicroVMID: input.MicroVMID, Permission: input.Permission},
		"foreign MicroVM":   {UserID: input.UserID, TenantID: input.TenantID, WorkspaceID: input.WorkspaceID, ResourcePlane: input.ResourcePlane, MicroVMID: "microvm-2", Permission: input.Permission},
		"different user":    {UserID: "user-2", TenantID: input.TenantID, WorkspaceID: input.WorkspaceID, ResourcePlane: input.ResourcePlane, MicroVMID: input.MicroVMID, Permission: input.Permission},
		"different scope":   {UserID: input.UserID, TenantID: input.TenantID, WorkspaceID: input.WorkspaceID, ResourcePlane: input.ResourcePlane, MicroVMID: input.MicroVMID, Permission: workspacesession.PermissionVSCode},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := VerifyAgentTicket(token, map[string]ed25519.PublicKey{"rp-key-1": public}, mismatch, now); !errors.Is(err, ErrAgentTicket) {
				t.Fatalf("mismatch accepted: %v", err)
			}
		})
	}
	if _, err := VerifyAgentTicket(token, map[string]ed25519.PublicKey{"rp-key-1": public}, input, now.Add(time.Minute)); !errors.Is(err, ErrAgentTicket) {
		t.Fatalf("expired ticket accepted: %v", err)
	}
}

func TestAgentTicketIssuerRejectsExcessiveTTLAndMissingIdentity(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	base := AgentTicket{UserID: "user-1", TenantID: "tenant-1", WorkspaceID: "workspace-1", ResourcePlane: "rp-1",
		MicroVMID: "microvm-1", Permission: workspacesession.PermissionShell}
	for name, issuer := range map[string]*AgentTicketIssuer{
		"too long":    {KeyID: "k1", PrivateKey: private, TTL: workspaceAgentTicketTTL + time.Second},
		"missing key": {KeyID: "k1", TTL: time.Minute},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := issuer.Issue(base); !errors.Is(err, ErrAgentTicket) {
				t.Fatalf("invalid issuer accepted: %v", err)
			}
		})
	}
	missing := base
	missing.MicroVMID = ""
	issuer := &AgentTicketIssuer{KeyID: "k1", PrivateKey: private, TTL: time.Minute}
	if _, _, err := issuer.Issue(missing); !errors.Is(err, ErrAgentTicket) {
		t.Fatalf("ticket without MicroVM identity accepted: %v", err)
	}
}

func TestAgentTicketReplayGuardIsAtomicAndExpiresEntries(t *testing.T) {
	now := time.Now().UTC()
	ticket := AgentTicket{TicketID: "ticket-1", ExpiresAt: now.Add(time.Minute).Unix()}
	guard := &AgentTicketReplayGuard{}
	var wg sync.WaitGroup
	var mu sync.Mutex
	accepted := 0
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if guard.Consume(ticket, now) == nil {
				mu.Lock()
				accepted++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if accepted != 1 {
		t.Fatalf("simultaneous replay acceptances = %d, want 1", accepted)
	}
	guard.seen["expired-ticket"] = now.Add(-time.Second)
	next := AgentTicket{TicketID: "ticket-2", ExpiresAt: now.Add(time.Minute).Unix()}
	if err := guard.Consume(next, now); err != nil {
		t.Fatalf("expired replay entry was not pruned: %v", err)
	}
	if _, exists := guard.seen["expired-ticket"]; exists {
		t.Fatal("expired replay entry remained in the guard")
	}
}
