//go:build integration

package authz

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/testutil"
)

func TestRepositoryAuthorizationScopesGrants(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	exec := func(statement string) {
		t.Helper()
		if _, err := tx.Exec(ctx, statement); err != nil {
			t.Fatal(err)
		}
	}
	for _, statement := range []string{
		`INSERT INTO tenants (id, name, created_at) VALUES ('tenant_a','A',now()),('tenant_b','B',now())`,
		`INSERT INTO users (id,cognito_subject,email,created_at) VALUES ('usr_owner','owner','',now()),('usr_member','member','',now())`,
		`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_a','usr_owner','owner',now()),('tenant_a','usr_member','member',now()),('tenant_b','usr_member','member',now())`,
		`INSERT INTO tenant_github_installations (tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_a',10,'acme','active','usr_owner',now(),now())`,
		`INSERT INTO github_app_installation_bindings (tenant_id,installation_id,bound_at) VALUES ('tenant_a',10,now())`,
		`INSERT INTO tenant_github_repositories (tenant_id,installation_id,github_repository_id,owner_login,repository_name,synchronized_at) VALUES ('tenant_a',10,20,'acme','api',now()),('tenant_a',10,21,'acme','other',now())`,
		`INSERT INTO tenant_repository_members (tenant_id,installation_id,github_repository_id,user_id,role,membership_joined_at,installation_bound_at,granted_by,updated_at) VALUES ('tenant_a',10,20,'usr_member','viewer',now(),now(),'usr_owner',now())`,
	} {
		exec(statement)
	}
	repo := RepositoryKey{TenantID: "tenant_a", InstallationID: 10, RepositoryID: 20}
	check := func(user domain.UserID, key RepositoryKey, action RepositoryAction, want error) {
		t.Helper()
		if err := RequireRepositoryAction(ctx, tx, user, key, action); !errors.Is(err, want) {
			t.Errorf("%s %v %s: got %v, want %v", user, key, action, err, want)
		}
	}
	check("usr_member", repo, RepositoryRead, nil)
	check("usr_member", repo, RepositoryRun, ErrRepositoryActionDenied)
	check("usr_member", RepositoryKey{"tenant_a", 10, 21}, RepositoryRead, ErrRepositoryAccessDenied)
	check("usr_member", RepositoryKey{"tenant_b", 10, 20}, RepositoryRead, ErrRepositoryAccessDenied)
	check("usr_member", RepositoryKey{"tenant_a", 11, 20}, RepositoryRead, ErrRepositoryAccessDenied)
	check("usr_owner", repo, RepositoryManage, nil)
	exec(`UPDATE tenant_repository_members SET role='operator'`)
	check("usr_member", repo, RepositoryRun, nil)
	check("usr_member", repo, RepositoryManage, ErrRepositoryActionDenied)
	exec(`DELETE FROM tenant_members WHERE tenant_id='tenant_a' AND user_id='usr_member'`)
	check("usr_member", repo, RepositoryRead, ErrTenantAccessDenied)
	exec(`INSERT INTO tenant_members (tenant_id,user_id,role,joined_at) VALUES ('tenant_a','usr_member','member',now()+interval '1 second')`)
	check("usr_member", repo, RepositoryRead, ErrRepositoryAccessDenied)
}
