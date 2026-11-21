// Package githubbroker owns credential-bearing GitHub operations. OAuth and
// Checks are Control Plane-only; checkout additionally admits exact
// allowlisted Resource Plane identities over mTLS. Public API callers are excluded.
package githubbroker

import (
	"context"
	"errors"

	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/store/githubregistry"
)

var ErrUnavailable = errors.New("GitHub OAuth broker unavailable")

// Authorize is a mandatory transport-authentication boundary. Implementations
// verify the caller identity and permission to use the specific Tenant pin.
type Authorize func(context.Context, githubregistry.SetupAppPin) error
type ResolveCredential func(context.Context, githubregistry.SetupAppPin) (githubregistry.SetupCredential, error)
type ReadSecret func(context.Context, githubregistry.SetupCredential) (string, error)
type NewVerifier func(githubapp.SetupConfig) (githubapp.SetupVerifier, error)

type OAuthService struct {
	authorize   Authorize
	resolve     ResolveCredential
	read        ReadSecret
	verifier    NewVerifier
	callbackURL string
}

type callerContextKey struct{}

// CallerFromContext exposes the TLS-authenticated workload identity to policy.
func CallerFromContext(ctx context.Context) (string, bool) {
	v, ok := ctx.Value(callerContextKey{}).(string)
	return v, ok
}

func NewOAuthService(authorize Authorize, resolve ResolveCredential, read ReadSecret, callbackURL string) (*OAuthService, error) {
	return NewOAuthServiceWithVerifierFactory(authorize, resolve, read, callbackURL, func(c githubapp.SetupConfig) (githubapp.SetupVerifier, error) {
		return githubapp.NewSetupClient(c)
	})
}

// NewOAuthServiceWithVerifierFactory allows a trusted broker composition to
// inject its GitHub transport while keeping all credential resolution and
// authorization checks in OAuthService.
func NewOAuthServiceWithVerifierFactory(authorize Authorize, resolve ResolveCredential, read ReadSecret, callbackURL string, verifier NewVerifier) (*OAuthService, error) {
	if authorize == nil || resolve == nil || read == nil || verifier == nil {
		return nil, ErrUnavailable
	}
	// Reuse the OAuth client's callback validation without reading credentials.
	if _, err := githubapp.NewSetupClient(githubapp.SetupConfig{Slug: "validation", AppID: 1, ClientID: "validation", ClientSecret: "validation", CallbackURL: callbackURL}); err != nil {
		return nil, ErrUnavailable
	}
	return &OAuthService{authorize, resolve, read, verifier, callbackURL}, nil
}

func (s *OAuthService) credential(ctx context.Context, pin githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
	if s == nil || ctx == nil || pin.TenantID == "" || pin.IntegrationID == "" || pin.AppRegistrationID == "" || pin.GitHubAppID <= 0 || pin.ConfigRevision <= 0 || pin.CredentialGeneration <= 0 {
		return githubregistry.SetupCredential{}, ErrUnavailable
	}
	if err := s.authorize(ctx, pin); err != nil {
		return githubregistry.SetupCredential{}, ErrUnavailable
	}
	c, err := s.resolve(ctx, pin)
	if err != nil || c.Pin != pin {
		return githubregistry.SetupCredential{}, ErrUnavailable
	}
	return c, nil
}

// URLs returns public setup metadata only. No Secret is read for URL generation.
func (s *OAuthService) URLs(ctx context.Context, pin githubregistry.SetupAppPin, state, codeVerifier string) (string, string, error) {
	c, err := s.credential(ctx, pin)
	if err != nil {
		return "", "", ErrUnavailable
	}
	verifier, err := s.verifier(githubapp.SetupConfig{Slug: c.Slug, AppID: pin.GitHubAppID, ClientID: c.ClientID, ClientSecret: "unused-for-public-URLs", CallbackURL: s.callbackURL})
	if err != nil || verifier == nil {
		return "", "", ErrUnavailable
	}
	return verifier.SetupURL(state), verifier.AuthorizationURL(state, codeVerifier), nil
}

// Verify returns only Installation identity, never an OAuth token or Secret.
// Authentication and live pin are rechecked for every use and after GitHub I/O.
func (s *OAuthService) Verify(ctx context.Context, pin githubregistry.SetupAppPin, code string, installationID int64, codeVerifier string) (githubapp.InstallationIdentity, error) {
	if code == "" || len(code) > 4096 || installationID <= 0 || len(codeVerifier) < 43 || len(codeVerifier) > 128 {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	c, err := s.credential(ctx, pin)
	if err != nil {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	secret, err := s.read(ctx, c)
	if err != nil || secret == "" {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	verifier, err := s.verifier(githubapp.SetupConfig{Slug: c.Slug, AppID: pin.GitHubAppID, ClientID: c.ClientID, ClientSecret: secret, CallbackURL: s.callbackURL})
	if err != nil || verifier == nil {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	identity, err := verifier.VerifyInstallation(ctx, code, installationID, codeVerifier)
	if errors.Is(err, githubapp.ErrAuthorizationRejected) {
		return githubapp.InstallationIdentity{}, githubapp.ErrAuthorizationRejected
	}
	if err != nil {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	if identity.ID != installationID || identity.AppID != pin.GitHubAppID {
		return githubapp.InstallationIdentity{}, githubapp.ErrAuthorizationRejected
	}
	current, err := s.credential(ctx, pin)
	if err != nil || current != c {
		return githubapp.InstallationIdentity{}, ErrUnavailable
	}
	return identity, nil
}
