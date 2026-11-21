package workspacesession

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
)

func TestTicketRoundTripBindsIdentityAudienceAndChannels(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 12, 4, 5, 6, 0, time.UTC)
	issuer := &Ed25519Issuer{KeyID: "gateway-2026-10", PrivateKey: private, TTL: 2 * time.Minute, Now: func() time.Time { return now }}
	input := Claims{UserID: "usr_1", TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlane: "rp_tokyo_01",
		Permissions: []Permission{PermissionPortForward, PermissionShell}}
	token, issued, err := issuer.Issue(input)
	if err != nil {
		t.Fatal(err)
	}
	claims, err := Verify(token, map[string]ed25519.PublicKey{issuer.KeyID: public}, AudienceGateway, now.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if claims.TicketID != issued.TicketID || claims.UserID != input.UserID || claims.TenantID != input.TenantID ||
		claims.WorkspaceID != input.WorkspaceID || claims.ResourcePlane != input.ResourcePlane || len(claims.Permissions) != 2 {
		t.Fatalf("verified claims lost identity binding: %+v", claims)
	}
	if err := AuthorizeChannel(claims, PermissionShell); err != nil {
		t.Fatalf("shell permission denied: %v", err)
	}
	if !errors.Is(AuthorizeChannel(claims, PermissionVSCode), ErrTicket) {
		t.Fatal("ungranted channel was authorized")
	}
	if _, err := Verify(token, map[string]ed25519.PublicKey{issuer.KeyID: public}, "other-gateway", now); !errors.Is(err, ErrTicket) {
		t.Fatalf("wrong audience error = %v", err)
	}
	if _, err := Verify(token, map[string]ed25519.PublicKey{"retired-key": public}, AudienceGateway, now); !errors.Is(err, ErrTicket) {
		t.Fatalf("unknown signing key error = %v", err)
	}
}

func TestTicketRejectsExpiryForgeryAndIdentityOmissions(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	issuer := &Ed25519Issuer{KeyID: "k1", PrivateKey: private, TTL: time.Minute, Now: func() time.Time { return now }}
	claims := Claims{UserID: "u1", TenantID: "t1", WorkspaceID: "w1", ResourcePlane: "rp1", Permissions: []Permission{PermissionShell}}
	token, _, err := issuer.Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	keys := map[string]ed25519.PublicKey{"k1": public}
	if _, err := Verify(token, keys, AudienceGateway, now.Add(time.Minute)); !errors.Is(err, ErrTicket) {
		t.Fatalf("expired token error = %v", err)
	}
	parts := strings.Split(token, ".")
	if parts[2][0] == 'A' {
		parts[2] = "B" + parts[2][1:]
	} else {
		parts[2] = "A" + parts[2][1:]
	}
	if _, err := Verify(strings.Join(parts, "."), keys, AudienceGateway, now); !errors.Is(err, ErrTicket) {
		t.Fatalf("forged token error = %v", err)
	}
	for _, invalid := range []Claims{
		{TenantID: "t1", WorkspaceID: "w1", ResourcePlane: "rp1", Permissions: []Permission{PermissionShell}},
		{UserID: "u1", TenantID: "t1", WorkspaceID: "w1", ResourcePlane: "rp1", Permissions: []Permission{"admin"}},
		{UserID: "u1", TenantID: "t1", WorkspaceID: "w1", ResourcePlane: "rp1", Permissions: []Permission{PermissionShell, PermissionShell}},
		{UserID: "u1", TenantID: "t1", WorkspaceID: "w1", Permissions: []Permission{PermissionShell}},
	} {
		if _, _, err := issuer.Issue(invalid); !errors.Is(err, ErrTicket) {
			t.Errorf("invalid ticket input was accepted: %+v", invalid)
		}
	}
	if err := ValidatePermissions([]Permission{"root"}); !errors.Is(err, ErrTicket) {
		t.Fatalf("unknown permission error = %v", err)
	}
}

func TestTicketIssuerRejectsExcessiveTTLAndUsesRotationKeyID(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	claims := Claims{UserID: domain.UserID("u1"), TenantID: "t1", WorkspaceID: "w1", ResourcePlane: "rp1", Permissions: []Permission{PermissionVSCode}}
	issuer := &Ed25519Issuer{KeyID: "rotation_key-2", PrivateKey: private, TTL: TicketMaxTTL + time.Second}
	if _, _, err := issuer.Issue(claims); !errors.Is(err, ErrTicket) {
		t.Fatalf("excessive TTL error = %v", err)
	}
	issuer.TTL = time.Minute
	token, _, err := issuer.Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(token, map[string]ed25519.PublicKey{issuer.KeyID: public}, AudienceGateway, time.Now()); err != nil {
		t.Fatalf("ticket signed by rotation key failed verification: %v", err)
	}
}

func TestTicketIssuerCapsExpiryAtPreviewDeadline(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC)
	issuer := &Ed25519Issuer{KeyID: "preview-bound-key", PrivateKey: private, TTL: TicketMaxTTL, Now: func() time.Time { return now }}
	input := Claims{UserID: "usr_preview", TenantID: "tenant_preview", WorkspaceID: "prvws_0123456789abcdef0123456789abcdef",
		ResourcePlane: "rp_preview", Permissions: []Permission{PermissionVSCode}}
	deadline := now.Add(30 * time.Second)
	token, claims, err := issuer.IssueBefore(input, deadline)
	if err != nil {
		t.Fatal(err)
	}
	if time.Unix(claims.ExpiresAt, 0).After(deadline) {
		t.Fatalf("ticket expiry %s exceeds Preview deadline %s", time.Unix(claims.ExpiresAt, 0), deadline)
	}
	if _, err := Verify(token, map[string]ed25519.PublicKey{issuer.KeyID: public}, AudienceGateway, now.Add(time.Second)); err != nil {
		t.Fatalf("deadline-bound ticket did not verify: %v", err)
	}
	if _, _, err := issuer.IssueBefore(input, now.Add(500*time.Millisecond)); !errors.Is(err, ErrTicket) {
		t.Fatalf("sub-second remaining Preview lifetime error = %v", err)
	}
}
