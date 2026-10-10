package githubbroker

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

type fakeVerifier struct {
	verify func() (githubapp.InstallationIdentity, error)
}

func (fakeVerifier) SetupURL(string) string                 { return "setup-url" }
func (fakeVerifier) AuthorizationURL(string, string) string { return "authorization-url" }
func (v fakeVerifier) VerifyInstallation(context.Context, string, int64, string) (githubapp.InstallationIdentity, error) {
	return v.verify()
}

func TestOAuthServiceAuthorizationAndLivePin(t *testing.T) {
	pin := githubregistry.SetupAppPin{TenantID: "tenant_a", IntegrationID: "integration_a", AppRegistrationID: "app_a", GitHubAppID: 123, ConfigRevision: 1, CredentialGeneration: 2}
	for _, mode := range []string{"success", "unauthenticated", "cross_tenant", "secret_error", "wrong_app", "rotation", "revoked", "github_error", "rejected"} {
		t.Run(mode, func(t *testing.T) {
			readCalls, resolveCalls, authCalls := 0, 0, 0
			service, err := NewOAuthService(func(context.Context, githubregistry.SetupAppPin) error {
				authCalls++
				if mode == "unauthenticated" || mode == "revoked" && authCalls > 1 {
					return errors.New("private identity detail")
				}
				return nil
			}, func(context.Context, githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
				resolveCalls++
				c := githubregistry.SetupCredential{Pin: pin, Slug: "test-app", ClientID: "test-client", VersionID: strings.Repeat("a", 32)}
				if mode == "cross_tenant" {
					c.Pin.TenantID = "tenant_b"
				}
				if mode == "rotation" && resolveCalls > 1 {
					c.VersionID = strings.Repeat("b", 32)
				}
				return c, nil
			}, func(context.Context, githubregistry.SetupCredential) (string, error) {
				readCalls++
				if mode == "secret_error" {
					return "", errors.New("sensitive-secret")
				}
				return "fake-secret", nil
			}, "https://hako.example/callback")
			if err != nil {
				t.Fatal(err)
			}
			service.verifier = func(c githubapp.SetupConfig) (githubapp.SetupVerifier, error) {
				if c.ClientSecret != "fake-secret" {
					t.Fatal("incorrect broker secret")
				}
				return fakeVerifier{func() (githubapp.InstallationIdentity, error) {
					identity := githubapp.InstallationIdentity{ID: 456, AppID: 123, AccountLogin: "owner"}
					if mode == "wrong_app" {
						identity.AppID = 124
					}
					if mode == "github_error" {
						return identity, errors.New("fake-secret")
					}
					if mode == "rejected" {
						return identity, githubapp.ErrAuthorizationRejected
					}
					return identity, nil
				}}, nil
			}
			identity, err := service.Verify(context.Background(), pin, "code", 456, strings.Repeat("v", 43))
			if mode == "success" {
				if err != nil || identity.ID != 456 || authCalls != 2 {
					t.Fatalf("verify failed: %v", err)
				}
			} else if err == nil || identity.ID != 0 || strings.Contains(err.Error(), "fake-secret") || strings.Contains(err.Error(), "sensitive") {
				t.Fatal("unsafe result")
			}
			if (mode == "unauthenticated" || mode == "cross_tenant") && readCalls != 0 {
				t.Fatal("secret read before authorization")
			}
		})
	}
}

func TestOAuthURLsDoNotReadSecret(t *testing.T) {
	pin := githubregistry.SetupAppPin{TenantID: "t", IntegrationID: "i", AppRegistrationID: "a", GitHubAppID: 1, ConfigRevision: 1, CredentialGeneration: 1}
	service, err := NewOAuthService(func(context.Context, githubregistry.SetupAppPin) error { return nil }, func(context.Context, githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
		return githubregistry.SetupCredential{Pin: pin, Slug: "test-app", ClientID: "client"}, nil
	}, func(context.Context, githubregistry.SetupCredential) (string, error) {
		t.Fatal("URL requested secret")
		return "", nil
	}, "https://hako.example/callback")
	if err != nil {
		t.Fatal(err)
	}
	setup, authorization, err := service.URLs(context.Background(), pin, "state", strings.Repeat("v", 43))
	if err != nil || !strings.HasPrefix(setup, "https://github.com/apps/test-app/") || !strings.Contains(authorization, "code_challenge_method=S256") {
		t.Fatal("invalid URLs")
	}
	if _, err := NewOAuthService(nil, nil, nil, "http://unsafe.example"); err == nil {
		t.Fatal("invalid service accepted")
	}
}
