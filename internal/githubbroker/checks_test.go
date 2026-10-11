package githubbroker

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

type fakeBrokerChecksToken struct {
	value  string
	expiry time.Time
}

func (t fakeBrokerChecksToken) Value() string        { return t.value }
func (t fakeBrokerChecksToken) ExpiresAt() time.Time { return t.expiry }

func checksFixture() ChecksRunIdentity {
	run, access := checkoutFixture()
	run.State = domain.WorkloadSucceeded
	run.CompletedAt = timePointer(time.Now().UTC())
	return ChecksRunIdentity{Run: run, Access: access, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}, DesiredCheckRevision: 4}
}

func timePointer(value time.Time) *time.Time { return &value }

func TestChecksServiceIssuesPinnedTokenForTerminalRunAndRechecksState(t *testing.T) {
	identity := checksFixture()
	authorizeCalls, resolveCalls, mintCalls := 0, 0, 0
	service, err := NewChecksService(func(_ context.Context, caller string, tenant domain.TenantID, runID domain.WorkloadRunID) error {
		authorizeCalls++
		if caller != controlPlaneURI || tenant != identity.Run.TenantID || runID != identity.Run.ID {
			return errors.New("denied")
		}
		return nil
	}, func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (ChecksRunIdentity, error) {
		resolveCalls++
		if tenant != identity.Run.TenantID || runID != identity.Run.ID {
			return ChecksRunIdentity{}, ErrChecksDenied
		}
		return identity, nil
	}, func(_ context.Context, got ChecksRunIdentity) (githubapp.CheckToken, error) {
		mintCalls++
		if got != identity {
			t.Fatal("token minter did not receive the durable terminal Run and Check revision")
		}
		return fakeBrokerChecksToken{value: "ghs_checks_write_secret_value", expiry: time.Now().Add(time.Hour)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := service.Issue(context.Background(), controlPlaneURI, identity.Run.TenantID, identity.Run.ID)
	if err != nil || grant.Token() != "ghs_checks_write_secret_value" || grant.Repository() != identity.Repository || resolveCalls != 2 || authorizeCalls != 2 || mintCalls != 1 {
		t.Fatalf("terminal Check token issue failed: grant=%s repo=%+v resolves=%d auth=%d mint=%d err=%v", grant, grant.Repository(), resolveCalls, authorizeCalls, mintCalls, err)
	}
	if fmt.Sprint(grant) == grant.Token() {
		t.Fatal("Checks grant formatting exposed token")
	}
}

func TestChecksIdentityAcceptsExplicitLegacySharedModeOnly(t *testing.T) {
	identity := checksFixture()
	identity.Run.GitHubIntegrationID = ""
	identity.Run.GitHubConfigRevision = 0
	identity.Run.GitHubCredentialGeneration = 0
	identity.Access = githubapp.TenantPrivateKeyAccess{}
	identity.Shared = true
	if !eligibleChecksIdentity(identity, identity.Run.TenantID, identity.Run.ID) {
		t.Fatal("valid legacy shared Check identity was rejected")
	}
	owned := identity
	owned.Shared = false
	if eligibleChecksIdentity(owned, owned.Run.TenantID, owned.Run.ID) {
		t.Fatal("legacy shared Run was accepted as a Tenant-owned Run")
	}
	partialPin := identity
	partialPin.Run.GitHubCredentialGeneration = 1
	if eligibleChecksIdentity(partialPin, partialPin.Run.TenantID, partialPin.Run.ID) {
		t.Fatal("partial Tenant credential pin was accepted as shared mode")
	}
	withOwnedAccess := identity
	withOwnedAccess.Access = checksFixture().Access
	if eligibleChecksIdentity(withOwnedAccess, withOwnedAccess.Run.TenantID, withOwnedAccess.Run.ID) {
		t.Fatal("shared Run with Tenant-owned credential material was accepted")
	}
}

func TestChecksTokenMinterSelectsPinnedCredentialModeWithoutFallback(t *testing.T) {
	identity := checksFixture()
	identity.Shared = true
	identity.Run.GitHubIntegrationID = ""
	identity.Run.GitHubConfigRevision = 0
	identity.Run.GitHubCredentialGeneration = 0
	identity.Access = githubapp.TenantPrivateKeyAccess{}
	sharedCalls, tenantCalls := 0, 0
	minter := NewChecksTokenMinter(func(context.Context, ChecksRunIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		tenantCalls++
		return fakeBrokerChecksToken{value: "ghs_tenant_secret_value", expiry: time.Now().Add(time.Hour)}, identity.Repository, nil
	}, func(_ context.Context, run domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		sharedCalls++
		if run.GitHubIntegrationID != "" {
			t.Fatal("shared provider received a Tenant-owned Run")
		}
		return fakeBrokerChecksToken{value: "ghs_shared_secret_value", expiry: time.Now().Add(time.Hour)}, identity.Repository, nil
	})
	token, err := minter(context.Background(), identity)
	if err != nil || token.Value() != "ghs_shared_secret_value" || sharedCalls != 1 || tenantCalls != 0 {
		t.Fatalf("shared credential mode selection = token %v, calls shared=%d tenant=%d, err=%v", token, sharedCalls, tenantCalls, err)
	}
	owned := checksFixture()
	token, err = minter(context.Background(), owned)
	if err != nil || token.Value() != "ghs_tenant_secret_value" || sharedCalls != 1 || tenantCalls != 1 {
		t.Fatalf("Tenant credential mode selection = token %v, calls shared=%d tenant=%d, err=%v", token, sharedCalls, tenantCalls, err)
	}
	if _, err := NewChecksTokenMinter(nil, nil)(context.Background(), identity); err == nil {
		t.Fatal("missing shared provider silently fell back to Tenant-owned credentials")
	}
	if _, err := NewChecksTokenMinter(nil, func(context.Context, domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
		sharedCalls++
		return fakeBrokerChecksToken{value: "ghs_shared_secret_value", expiry: time.Now().Add(time.Hour)}, identity.Repository, nil
	})(context.Background(), owned); err == nil {
		t.Fatal("Tenant-owned Run silently fell back to shared credentials")
	}
}

func TestChecksServiceRejectsUntrustedTenantPinAndRevisionChanges(t *testing.T) {
	for _, mode := range []string{"wrong_caller", "wrong_tenant", "shared_run", "bad_pin", "rotation", "check_revision_changed", "revoked_after_mint", "invalid_token"} {
		t.Run(mode, func(t *testing.T) {
			identity := checksFixture()
			authorizeCalls, resolveCalls, mintCalls := 0, 0, 0
			service, err := NewChecksService(func(_ context.Context, _ string, _ domain.TenantID, _ domain.WorkloadRunID) error {
				authorizeCalls++
				if mode == "revoked_after_mint" && authorizeCalls > 1 {
					return ErrChecksDenied
				}
				return nil
			}, func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (ChecksRunIdentity, error) {
				resolveCalls++
				current := identity
				if mode == "shared_run" {
					current.Run.GitHubIntegrationID = ""
				}
				if mode == "bad_pin" {
					current.Access.CredentialGeneration++
				}
				if (mode == "rotation" || mode == "check_revision_changed") && resolveCalls > 1 {
					if mode == "rotation" {
						current.Access.CredentialGeneration++
					} else {
						current.DesiredCheckRevision++
					}
				}
				if tenant != identity.Run.TenantID || runID != identity.Run.ID {
					return ChecksRunIdentity{}, ErrChecksDenied
				}
				return current, nil
			}, func(context.Context, ChecksRunIdentity) (githubapp.CheckToken, error) {
				mintCalls++
				value := "ghs_checks_write_secret_value"
				if mode == "invalid_token" {
					value = "bad"
				}
				return fakeBrokerChecksToken{value: value, expiry: time.Now().Add(time.Hour)}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			caller, tenant := controlPlaneURI, identity.Run.TenantID
			if mode == "wrong_caller" {
				caller = "spiffe://hako/resource-plane"
			}
			if mode == "wrong_tenant" {
				tenant = "tenant_other"
			}
			if _, err := service.Issue(context.Background(), caller, tenant, identity.Run.ID); err == nil {
				t.Fatal("untrusted or stale Check identity was granted a token")
			}
			if (mode == "wrong_caller" || mode == "wrong_tenant" || mode == "shared_run" || mode == "bad_pin") && mintCalls != 0 {
				t.Fatalf("invalid request reached token minter %d times", mintCalls)
			}
		})
	}
}
