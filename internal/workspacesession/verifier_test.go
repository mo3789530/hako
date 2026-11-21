package workspacesession

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"
	"time"
)

type fakeActiveLookup struct {
	active bool
	err    error
	seen   Claims
	calls  int
}

func (l *fakeActiveLookup) IsActive(_ context.Context, claims Claims) (bool, error) {
	l.calls++
	l.seen = claims
	return l.active, l.err
}

func TestVerifierChecksActiveRecordAndChannelBeforeGatewayUpgrade(t *testing.T) {
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	issuer := &Ed25519Issuer{KeyID: "k1", PrivateKey: private, TTL: time.Minute, Now: func() time.Time { return now }}
	token, want, err := issuer.Issue(Claims{UserID: "usr_1", TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlane: "rp_1", Permissions: []Permission{PermissionShell}})
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeActiveLookup{active: true}
	verifier := &Verifier{Keys: map[string]ed25519.PublicKey{"k1": public}, Audience: AudienceGateway, Lookup: lookup, Now: func() time.Time { return now }}
	claims, err := verifier.Authorize(context.Background(), token, PermissionShell)
	if err != nil || claims.TicketID != want.TicketID {
		t.Fatalf("authorized claims=%+v error=%v", claims, err)
	}
	if lookup.calls != 1 || lookup.seen.UserID != want.UserID || lookup.seen.TenantID != want.TenantID ||
		lookup.seen.WorkspaceID != want.WorkspaceID || lookup.seen.ResourcePlane != want.ResourcePlane || lookup.seen.TicketID != want.TicketID {
		t.Fatalf("active-state lookup did not receive signed identity: %+v", lookup)
	}
	lookup.calls = 0
	if _, err := verifier.Authorize(context.Background(), token, PermissionVSCode); !errors.Is(err, ErrTicket) || lookup.calls != 0 {
		t.Fatalf("channel mismatch error=%v lookup calls=%d", err, lookup.calls)
	}
	lookup.active = false
	if _, err := verifier.Authorize(context.Background(), token, PermissionShell); !errors.Is(err, ErrTicket) {
		t.Fatalf("revoked session error = %v", err)
	}
	lookup.err = errors.New("private database detail")
	if _, err := verifier.Authorize(context.Background(), token, PermissionShell); !errors.Is(err, ErrSessionStateUnavailable) ||
		containsText(err.Error(), "private database detail") {
		t.Fatalf("active lookup failure leaked details: %v", err)
	}
}

func TestVerifierFailsClosedWithoutLookupOrOnCanceledRequest(t *testing.T) {
	if _, err := (*Verifier)(nil).Authorize(context.Background(), "token", PermissionShell); !errors.Is(err, ErrSessionStateUnavailable) {
		t.Fatalf("nil verifier error = %v", err)
	}
	verifier := &Verifier{Audience: AudienceGateway}
	if _, err := verifier.Authorize(context.Background(), "token", PermissionShell); !errors.Is(err, ErrSessionStateUnavailable) {
		t.Fatalf("missing lookup error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	issuer := &Ed25519Issuer{KeyID: "k1", PrivateKey: private, TTL: time.Minute}
	token, _, err := issuer.Issue(Claims{UserID: "u", TenantID: "t", WorkspaceID: "w", ResourcePlane: "rp", Permissions: []Permission{PermissionShell}})
	if err != nil {
		t.Fatal(err)
	}
	lookup := &fakeActiveLookup{active: true}
	verifier = &Verifier{Keys: map[string]ed25519.PublicKey{"k1": public}, Audience: AudienceGateway, Lookup: lookup}
	if _, err := verifier.Authorize(ctx, token, PermissionShell); !errors.Is(err, ErrSessionStateUnavailable) || lookup.calls != 0 {
		t.Fatalf("canceled validation error=%v lookup calls=%d", err, lookup.calls)
	}
}

func TestVerifierReloadsOverlappingSessionKeySetForRotation(t *testing.T) {
	oldPublic, oldPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	newPublic, newPrivate, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	claims := Claims{UserID: "usr_1", TenantID: "tenant_1", WorkspaceID: "ws_1", ResourcePlane: "rp_1", Permissions: []Permission{PermissionShell}}
	oldToken, _, err := (&Ed25519Issuer{KeyID: "session_old", PrivateKey: oldPrivate, TTL: time.Minute, Now: func() time.Time { return now }}).Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	newToken, _, err := (&Ed25519Issuer{KeyID: "session_new", PrivateKey: newPrivate, TTL: time.Minute, Now: func() time.Time { return now }}).Issue(claims)
	if err != nil {
		t.Fatal(err)
	}
	activeKeys := map[string]ed25519.PublicKey{"session_old": oldPublic}
	lookup := &fakeActiveLookup{active: true}
	verifier := &Verifier{Audience: AudienceGateway, Lookup: lookup, Now: func() time.Time { return now },
		KeyResolver: func() (map[string]ed25519.PublicKey, error) { return activeKeys, nil }}
	if _, err := verifier.Authorize(context.Background(), oldToken, PermissionShell); err != nil {
		t.Fatalf("old key before rotation failed: %v", err)
	}
	activeKeys = map[string]ed25519.PublicKey{"session_old": oldPublic, "session_new": newPublic}
	if _, err := verifier.Authorize(context.Background(), oldToken, PermissionShell); err != nil {
		t.Fatalf("old key during overlap failed: %v", err)
	}
	if _, err := verifier.Authorize(context.Background(), newToken, PermissionShell); err != nil {
		t.Fatalf("new key during overlap failed: %v", err)
	}
	activeKeys = map[string]ed25519.PublicKey{"session_new": newPublic}
	if _, err := verifier.Authorize(context.Background(), oldToken, PermissionShell); !errors.Is(err, ErrTicket) {
		t.Fatalf("retired key was still accepted: %v", err)
	}
	if _, err := verifier.Authorize(context.Background(), newToken, PermissionShell); err != nil {
		t.Fatalf("new key after rotation failed: %v", err)
	}
	lookup.calls = 0
	verifier.KeyResolver = func() (map[string]ed25519.PublicKey, error) { return nil, errors.New("private file error") }
	if _, err := verifier.Authorize(context.Background(), newToken, PermissionShell); !errors.Is(err, ErrSessionStateUnavailable) || lookup.calls != 0 || containsText(err.Error(), "private file error") {
		t.Fatalf("key reload failure did not fail closed safely: err=%v lookup calls=%d", err, lookup.calls)
	}
}

func containsText(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
