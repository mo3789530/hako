package githubbroker

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
)

func checkoutFixture() (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) {
	now := time.Now().UTC()
	run := domain.WorkloadRun{ID: "run_checkout", TenantID: "tenant_checkout", Kind: domain.WorkloadJob,
		GitHubInstallation: 123, GitHubRepository: 456, GitHubIntegrationID: "integration_checkout",
		GitHubConfigRevision: 3, GitHubCredentialGeneration: 7, CommitSHA: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		DesiredState: domain.WorkloadDesiredRunning, State: domain.WorkloadRunning, TimeoutAt: now.Add(5 * time.Minute)}
	access := githubapp.TenantPrivateKeyAccess{TenantID: "tenant_checkout", IntegrationID: "integration_checkout", GitHubAppID: 789,
		ConfigRevision: 3, CredentialGeneration: 7, ReaderRoleARN: "arn:aws:iam::123456789012:role/hako/private-key-reader",
		SecretARN: "arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:hako/private-key-AbCdEf", SecretVersionID: "0123456789abcdef0123456789abcdef"}
	return run, access
}

func TestCheckoutServiceResolvesAndRechecksCanonicalTenantRun(t *testing.T) {
	run, access := checkoutFixture()
	resolveCalls, authorizeCalls, mintCalls := 0, 0, 0
	service, err := NewCheckoutService(func(_ context.Context, caller string, tenant domain.TenantID, runID domain.WorkloadRunID) error {
		authorizeCalls++
		if caller != controlPlaneURI || tenant != run.TenantID || runID != run.ID {
			return errors.New("denied")
		}
		return nil
	}, func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
		resolveCalls++
		if tenant != run.TenantID || runID != run.ID {
			return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, errors.New("not found")
		}
		return run, access, nil
	}, func(_ context.Context, actual domain.WorkloadRun, actualAccess githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
		mintCalls++
		if !reflect.DeepEqual(actual, run) || actualAccess != access {
			t.Fatal("mint did not receive canonical Run and exact credential pin")
		}
		return "ghs_token_value_that_is_never_logged", time.Now().Add(time.Hour), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := service.Issue(context.Background(), controlPlaneURI, run.TenantID, run.ID)
	if err != nil || grant.Token() != "ghs_token_value_that_is_never_logged" || !grant.ExpiresAt().After(time.Now()) {
		t.Fatalf("Issue() grant=%s err=%v", grant, err)
	}
	if resolveCalls != 2 || authorizeCalls != 2 || mintCalls != 1 {
		t.Fatalf("resolve=%d authorize=%d mint=%d", resolveCalls, authorizeCalls, mintCalls)
	}
	if fmt.Sprint(grant) == grant.Token() {
		t.Fatal("grant formatting exposed the token")
	}
}

func TestCheckoutServiceRejectsWrongCallerTenantSharedRunAndStalePinBeforeMint(t *testing.T) {
	for _, mode := range []string{"wrong_caller", "wrong_tenant", "shared_run", "pin_mismatch", "rotation_during_mint", "cancel_during_mint", "invalid_token"} {
		t.Run(mode, func(t *testing.T) {
			run, access := checkoutFixture()
			mintCalls, resolveCalls := 0, 0
			service, err := NewCheckoutService(func(_ context.Context, _ string, tenant domain.TenantID, _ domain.WorkloadRunID) error {
				if mode == "wrong_tenant" && tenant != run.TenantID {
					return nil
				}
				return nil
			}, func(_ context.Context, tenant domain.TenantID, _ domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
				resolveCalls++
				currentRun, currentAccess := run, access
				if mode == "shared_run" {
					currentRun.GitHubIntegrationID = ""
				}
				if mode == "pin_mismatch" {
					currentAccess.CredentialGeneration++
				}
				if mode == "rotation_during_mint" && resolveCalls > 1 {
					currentAccess.CredentialGeneration++
				}
				if mode == "cancel_during_mint" && resolveCalls > 1 {
					currentRun.State = domain.WorkloadCancelled
				}
				if tenant != run.TenantID {
					return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, errors.New("not found")
				}
				return currentRun, currentAccess, nil
			}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
				mintCalls++
				if mode == "invalid_token" {
					return "", time.Now().Add(time.Hour), nil
				}
				return "ghs_token_value_that_is_never_logged", time.Now().Add(time.Hour), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			caller, tenant := controlPlaneURI, run.TenantID
			if mode == "wrong_caller" {
				caller = "spiffe://hako/untrusted"
			}
			if mode == "wrong_tenant" {
				tenant = "tenant_other"
			}
			_, err = service.Issue(context.Background(), caller, tenant, run.ID)
			expectedMintCalls := 1
			if mode == "wrong_caller" || mode == "wrong_tenant" || mode == "shared_run" || mode == "pin_mismatch" {
				expectedMintCalls = 0
			}
			if err == nil || mintCalls != expectedMintCalls {
				t.Fatalf("Issue() err=%v mintCalls=%d", err, mintCalls)
			}
		})
	}
}

func TestCheckoutServiceRejectsTokenAfterAuthorizationOrRunRevocation(t *testing.T) {
	for _, mode := range []string{"auth_revoked", "run_revoked"} {
		t.Run(mode, func(t *testing.T) {
			run, access := checkoutFixture()
			resolveCalls, authorizeCalls, mintCalls := 0, 0, 0
			service, err := NewCheckoutService(func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error {
				authorizeCalls++
				if mode == "auth_revoked" && authorizeCalls > 1 {
					return errors.New("revoked")
				}
				return nil
			}, func(context.Context, domain.TenantID, domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
				resolveCalls++
				current := run
				if mode == "run_revoked" && resolveCalls > 1 {
					current.DesiredState = domain.WorkloadDesiredCancelled
					current.State = domain.WorkloadCancelled
				}
				return current, access, nil
			}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
				mintCalls++
				return "ghs_token_value_that_is_never_logged", time.Now().Add(time.Hour), nil
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := service.Issue(context.Background(), controlPlaneURI, run.TenantID, run.ID); err == nil || mintCalls != 1 {
				t.Fatal("token was returned after revocation or cancellation")
			}
		})
	}
}

func TestCheckoutServiceCanRetryTransientMintFailureWithoutChangingRunPin(t *testing.T) {
	run, access := checkoutFixture()
	mintCalls := 0
	service, err := NewCheckoutService(func(context.Context, string, domain.TenantID, domain.WorkloadRunID) error { return nil },
		func(_ context.Context, tenant domain.TenantID, runID domain.WorkloadRunID) (domain.WorkloadRun, githubapp.TenantPrivateKeyAccess, error) {
			if tenant != run.TenantID || runID != run.ID {
				return domain.WorkloadRun{}, githubapp.TenantPrivateKeyAccess{}, ErrCheckoutDenied
			}
			return run, access, nil
		}, func(context.Context, domain.WorkloadRun, githubapp.TenantPrivateKeyAccess) (string, time.Time, error) {
			mintCalls++
			if mintCalls == 1 {
				return "", time.Time{}, errors.New("temporary upstream failure")
			}
			return "ghs_token_value_that_is_never_logged", time.Now().Add(time.Hour), nil
		})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Issue(context.Background(), controlPlaneURI, run.TenantID, run.ID); err == nil {
		t.Fatal("transient mint failure returned a grant")
	}
	grant, err := service.Issue(context.Background(), controlPlaneURI, run.TenantID, run.ID)
	if err != nil || grant.Token() == "" || mintCalls != 2 {
		t.Fatalf("retry failed: grant=%s calls=%d err=%v", grant, mintCalls, err)
	}
}
