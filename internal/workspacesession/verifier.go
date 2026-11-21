package workspacesession

import (
	"context"
	"crypto/ed25519"
	"errors"
	"time"
)

var ErrSessionStateUnavailable = errors.New("Workspace session authorization is unavailable")

// ActiveSessionLookup must check the persisted session ID and every identity
// field against Control Plane state. Resource Plane Gateways inject a trusted
// internal client; they must not connect directly to Aurora DSQL.
type ActiveSessionLookup interface {
	IsActive(context.Context, Claims) (bool, error)
}

type Verifier struct {
	Keys        map[string]ed25519.PublicKey
	KeyResolver func() (map[string]ed25519.PublicKey, error)
	Audience    string
	Lookup      ActiveSessionLookup
	Now         func() time.Time
}

// Authorize verifies the bearer ticket and persisted revocation state before a
// Gateway upgrades a connection. Routing identity comes only from the claims.
func (v *Verifier) Authorize(ctx context.Context, token string, permission Permission) (Claims, error) {
	if v == nil || ctx == nil || v.Lookup == nil || v.Audience == "" {
		return Claims{}, ErrSessionStateUnavailable
	}
	if ctx.Err() != nil {
		return Claims{}, ErrSessionStateUnavailable
	}
	now := time.Now
	if v.Now != nil {
		now = v.Now
	}
	keys := v.Keys
	if v.KeyResolver != nil {
		resolved, err := v.KeyResolver()
		if err != nil || len(resolved) == 0 {
			return Claims{}, ErrSessionStateUnavailable
		}
		keys = resolved
	}
	claims, err := Verify(token, keys, v.Audience, now().UTC())
	if err != nil || AuthorizeChannel(claims, permission) != nil {
		return Claims{}, ErrTicket
	}
	active, err := v.Lookup.IsActive(ctx, claims)
	if err != nil || ctx.Err() != nil {
		return Claims{}, ErrSessionStateUnavailable
	}
	if !active {
		return Claims{}, ErrTicket
	}
	return claims, nil
}
