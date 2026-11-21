package modelcredentialbroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func TestNewRequiresDatabaseAndSecretReader(t *testing.T) {
	reader := secretReaderFunc(func(context.Context, modelcredentials.SecretAccess) ([]byte, error) {
		return []byte("synthetic"), nil
	})
	if _, err := New(nil, transaction.Policy{}, reader); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(nil, reader) error = %v", err)
	}
	pool := credentialNoopPool{}
	if _, err := New(pool, transaction.Policy{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("New(pool, nil) error = %v", err)
	}
	if resolver, err := New(pool, transaction.Policy{}, reader); err != nil || resolver.now == nil {
		t.Fatalf("New(pool, reader) = %+v, %v", resolver, err)
	}
}

func TestCredentialResolutionRejectsInvalidInvocationBeforeSecretRead(t *testing.T) {
	var reads int
	resolver := &Resolver{reader: secretReaderFunc(func(context.Context, modelcredentials.SecretAccess) ([]byte, error) {
		reads++
		return []byte("synthetic"), nil
	})}
	identity := modelcredentials.RunnerClaimIdentity{}
	consume := func(context.Context, []byte) error { return nil }
	if err := resolver.WithCredential(context.Background(), identity, nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil credential consumer error = %v", err)
	}
	if err := resolver.WithCredential(nil, identity, consume); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil context error = %v", err)
	}
	if err := (*Resolver)(nil).WithCredential(context.Background(), identity, consume); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil resolver error = %v", err)
	}
	if err := resolver.WithAgentRunCredential(context.Background(), modelcredentials.AgentRunIdentity{}, nil); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Agent credential consumer error = %v", err)
	}
	if err := resolver.WithAgentRunCredential(nil, modelcredentials.AgentRunIdentity{}, consume); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Agent context error = %v", err)
	}
	if err := (*Resolver)(nil).WithAgentRunCredential(context.Background(), modelcredentials.AgentRunIdentity{}, consume); !errors.Is(err, ErrUnavailable) {
		t.Errorf("nil Agent resolver error = %v", err)
	}
	if reads != 0 {
		t.Fatalf("invalid call read a Tenant Secret %d times", reads)
	}
}

func TestValidSecretEnforcesPrintableASCIIAndSizeBounds(t *testing.T) {
	for _, test := range []struct {
		name  string
		value []byte
		want  bool
	}{
		{name: "empty", value: nil, want: false},
		{name: "short printable", value: []byte("sk-ant-synthetic"), want: true},
		{name: "maximum size", value: []byte(strings.Repeat("x", 8192)), want: true},
		{name: "oversized", value: []byte(strings.Repeat("x", 8193)), want: false},
		{name: "control byte", value: []byte{'s', '\n', 'k'}, want: false},
		{name: "non ascii", value: []byte{'s', 0xff, 'k'}, want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := validSecret(test.value); got != test.want {
				t.Fatalf("validSecret() = %t, want %t", got, test.want)
			}
		})
	}
}

type secretReaderFunc func(context.Context, modelcredentials.SecretAccess) ([]byte, error)

func (f secretReaderFunc) ReadSecret(ctx context.Context, access modelcredentials.SecretAccess) ([]byte, error) {
	return f(ctx, access)
}

type credentialNoopPool struct{ pgx.Tx }

func (credentialNoopPool) Begin(context.Context) (pgx.Tx, error) {
	panic("unexpected database transaction")
}
