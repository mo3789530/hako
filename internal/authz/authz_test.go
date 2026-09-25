package authz

import (
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
)

type guardTx struct{ pgx.Tx }

func TestMembershipRejectsMissingTransactionOrIdentity(t *testing.T) {
	if _, err := RequireTenantMembership(t.Context(), nil, "usr_1", "tenant_1"); err == nil {
		t.Fatal("nil transaction should fail")
	}
	for _, identity := range []struct {
		user   domain.UserID
		tenant domain.TenantID
	}{{"", "tenant_1"}, {"usr_1", ""}} {
		if _, err := RequireTenantMembership(t.Context(), guardTx{}, identity.user, identity.tenant); !errors.Is(err, ErrTenantAccessDenied) {
			t.Errorf("empty identity should be non-disclosing access denied, got %v", err)
		}
	}
}
