// Package modelcredentialbroker resolves Tenant-owned model credentials only
// for a provenance-verified, live Actions runner claim.
package modelcredentialbroker

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
)

var (
	ErrUnavailable   = errors.New("trusted model credential unavailable")
	ErrNotConfigured = errors.New("Tenant model credential is not configured")
)

type SecretReader interface {
	ReadSecret(context.Context, modelcredentials.SecretAccess) ([]byte, error)
}

type CredentialConsumer func(context.Context, []byte) error

type Resolver struct {
	pool   transaction.Beginner
	policy transaction.Policy
	reader SecretReader
	now    func() time.Time
}

func New(pool transaction.Beginner, policy transaction.Policy, reader SecretReader) (*Resolver, error) {
	if pool == nil || reader == nil {
		return nil, ErrUnavailable
	}
	return &Resolver{pool: pool, policy: policy, reader: reader, now: time.Now}, nil
}

// WithCredential verifies the exact launch proof and MicroVM binding before
// reading a Secret, then rechecks the active version/generation after I/O and
// before delivering bytes to the trusted in-process consumer. The byte buffer
// is wiped on return and is never serialized by this package.
func (r *Resolver) WithCredential(ctx context.Context, identity modelcredentials.RunnerClaimIdentity, consume CredentialConsumer) error {
	if r == nil || ctx == nil || consume == nil {
		return ErrUnavailable
	}
	access, err := r.resolve(ctx, identity)
	if err != nil {
		if errors.Is(err, modelcredentials.ErrCredentialNotConfigured) {
			return ErrNotConfigured
		}
		return ErrUnavailable
	}
	secret, err := r.reader.ReadSecret(ctx, access)
	if err != nil || !validSecret(secret) {
		clear(secret)
		return ErrUnavailable
	}
	defer clear(secret)
	current, err := r.resolve(ctx, identity)
	if err != nil || current != access {
		return ErrUnavailable
	}
	if err := consume(ctx, secret); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (r *Resolver) resolve(ctx context.Context, identity modelcredentials.RunnerClaimIdentity) (modelcredentials.SecretAccess, error) {
	return transaction.Within(ctx, r.pool, r.policy, func(ctx context.Context, tx pgx.Tx) (modelcredentials.SecretAccess, error) {
		return modelcredentials.ResolveForRunnerClaim(ctx, tx, identity, r.now().UTC())
	})
}

// WithAgentRunCredential verifies an exact, latest Agent attempt and the
// per-Repository Native Agent model-access policy before reading a Tenant
// credential. It rechecks the authorization after the read, then wipes the
// credential bytes after the trusted in-process consumer returns.
func (r *Resolver) WithAgentRunCredential(ctx context.Context, identity modelcredentials.AgentRunIdentity, consume CredentialConsumer) error {
	if r == nil || ctx == nil || consume == nil {
		return ErrUnavailable
	}
	access, err := r.resolveAgentRun(ctx, identity)
	if err != nil {
		return ErrUnavailable
	}
	secret, err := r.reader.ReadSecret(ctx, access)
	if err != nil || !validSecret(secret) {
		clear(secret)
		return ErrUnavailable
	}
	defer clear(secret)
	current, err := r.resolveAgentRun(ctx, identity)
	if err != nil || current != access {
		return ErrUnavailable
	}
	if err := consume(ctx, secret); err != nil {
		return ErrUnavailable
	}
	return nil
}

func (r *Resolver) resolveAgentRun(ctx context.Context, identity modelcredentials.AgentRunIdentity) (modelcredentials.SecretAccess, error) {
	return transaction.Within(ctx, r.pool, r.policy, func(ctx context.Context, tx pgx.Tx) (modelcredentials.SecretAccess, error) {
		return modelcredentials.ResolveForAgentRun(ctx, tx, identity, r.now().UTC())
	})
}

func validSecret(value []byte) bool {
	if len(value) == 0 || len(value) > 8192 {
		return false
	}
	for _, char := range value {
		if char < 0x20 || char > 0x7e {
			return false
		}
	}
	return true
}
