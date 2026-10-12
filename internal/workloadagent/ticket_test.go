package workloadagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/workloadjournal"
)

func TestEd25519TicketIsShortLivedAndBoundToVMIdentityAndOperation(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	issuer := &Ed25519TicketIssuer{KeyID: "rp-key-7", PrivateKey: privateKey, Now: func() time.Time { return now }}
	id := testIdentity()
	binding := ExecutionBinding{SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
	ticket, err := issuer.IssueExecution("microvm_1", id, OperationStart, binding)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"rp-key-7": publicKey}
	if err := VerifyTicket(ticket, keys, "microvm_1", id, OperationStart, now.Add(4*time.Minute)); err != nil {
		t.Fatalf("valid ticket rejected: %v", err)
	}
	claims, err := VerifyTicketClaims(ticket, keys, id, OperationStart, now)
	if err != nil || claims.Execution == nil || *claims.Execution != binding {
		t.Fatalf("signed execution binding = %+v, err=%v", claims.Execution, err)
	}
	for name, verify := range map[string]func() error{
		"expired": func() error {
			return VerifyTicket(ticket, keys, "microvm_1", id, OperationStart, now.Add(5*time.Minute))
		},
		"wrong VM":        func() error { return VerifyTicket(ticket, keys, "microvm_2", id, OperationStart, now) },
		"wrong operation": func() error { return VerifyTicket(ticket, keys, "microvm_1", id, OperationResult, now) },
		"wrong attempt": func() error {
			changed := id
			changed.AttemptID = "attempt_2"
			return VerifyTicket(ticket, keys, "microvm_1", changed, OperationStart, now)
		},
		"unknown signing key": func() error {
			return VerifyTicket(ticket, map[string]ed25519.PublicKey{}, "microvm_1", id, OperationStart, now)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := verify(); err != ErrAgentTicket {
				t.Fatalf("invalid ticket accepted: %v", err)
			}
		})
	}

	parts := strings.Split(ticket, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	if err := VerifyTicket(strings.Join(parts, "."), keys, "microvm_1", id, OperationStart, now); err != ErrAgentTicket {
		t.Fatalf("tampered ticket accepted: %v", err)
	}
}

func TestTicketIssuerFailsClosedOnInvalidInputsAndClampsLifetime(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)
	issuer := &Ed25519TicketIssuer{KeyID: "key_1", PrivateKey: privateKey, TTL: time.Second, Now: func() time.Time { return now }}
	id := testIdentity()
	binding := ExecutionBinding{SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: now.Add(time.Hour)}
	ticket, err := issuer.Issue("microvm_1", id, OperationStatus)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyTicket(ticket, map[string]ed25519.PublicKey{"key_1": publicKey}, "microvm_1", id, OperationStatus, now.Add(30*time.Second)); err != nil {
		t.Fatalf("minimum ticket lifetime was not applied: %v", err)
	}
	if _, err := issuer.Issue("microvm_1", id, Operation("shell")); err != ErrAgentTicket {
		t.Fatalf("unapproved operation issued: %v", err)
	}
	if _, err := issuer.IssueExecution("other.vm", id, OperationStart, binding); err != ErrAgentTicket {
		t.Fatalf("invalid VM ID accepted: %v", err)
	}
	if _, err := (&Ed25519TicketIssuer{KeyID: "bad kid", PrivateKey: privateKey}).IssueExecution("microvm_1", id, OperationStart, binding); err != ErrAgentTicket {
		t.Fatalf("invalid key ID accepted: %v", err)
	}
	if _, err := issuer.Issue("microvm_1", id, OperationStart); err != ErrAgentTicket {
		t.Fatal("unbound Start ticket was issued")
	}
	tooLong := binding
	tooLong.Deadline = now.Add(9 * time.Hour)
	if _, err := issuer.IssueExecution("microvm_1", id, OperationStart, tooLong); err != ErrAgentTicket {
		t.Fatalf("execution ticket exceeded the MicroVM lifetime: %v", err)
	}
	if err := VerifyTicket("", nil, "microvm_1", workloadjournal.Command{}, OperationStart, now); err != ErrAgentTicket {
		t.Fatalf("empty ticket accepted: %v", err)
	}
}
