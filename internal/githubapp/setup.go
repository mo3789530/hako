// Package githubapp contains the small OAuth/user-installation verification
// boundary used by the tenant Installation setup callback.
package githubapp

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxGitHubResponseBytes = 1 << 20

var ErrAuthorizationRejected = errors.New("GitHub rejected the OAuth code or Installation authorization")

type httpStatusError struct{ status int }

func (e httpStatusError) Error() string { return fmt.Sprintf("GitHub returned HTTP %d", e.status) }

type InstallationIdentity struct {
	ID           int64
	AppID        int64
	AccountLogin string
}

type SetupVerifier interface {
	SetupURL(state string) string
	AuthorizationURL(state, codeVerifier string) string
	VerifyInstallation(context.Context, string, int64, string) (InstallationIdentity, error)
}

type SetupConfig struct {
	Slug         string
	AppID        int64
	ClientID     string
	ClientSecret string
	CallbackURL  string
	HTTPClient   *http.Client
}

type SetupClient struct {
	slug, clientID, clientSecret, callbackURL string
	appID                                     int64
	httpClient                                *http.Client
	oauthURL, apiURL                          string
}

func NewSetupClient(config SetupConfig) (*SetupClient, error) {
	config.Slug = strings.TrimSpace(config.Slug)
	config.ClientID = strings.TrimSpace(config.ClientID)
	callbackURL, err := url.Parse(strings.TrimSpace(config.CallbackURL))
	if !validSlug(config.Slug) || config.AppID <= 0 || config.ClientID == "" || strings.TrimSpace(config.ClientSecret) == "" || err != nil || callbackURL.Scheme != "https" || callbackURL.Host == "" || callbackURL.RawQuery != "" || callbackURL.Fragment != "" {
		return nil, errors.New("GitHub App slug, positive App ID, client ID, client secret, and HTTPS callback URL are required")
	}
	if config.HTTPClient == nil {
		config.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &SetupClient{
		slug: config.Slug, appID: config.AppID, clientID: config.ClientID, clientSecret: config.ClientSecret, callbackURL: callbackURL.String(),
		httpClient: config.HTTPClient, oauthURL: "https://github.com/login/oauth/access_token", apiURL: "https://api.github.com",
	}, nil
}

func (c *SetupClient) SetupURL(state string) string {
	return "https://github.com/apps/" + url.PathEscape(c.slug) + "/installations/new?state=" + url.QueryEscape(state)
}

func (c *SetupClient) AuthorizationURL(state, codeVerifier string) string {
	challengeDigest := sha256.Sum256([]byte(codeVerifier))
	challenge := base64.RawURLEncoding.EncodeToString(challengeDigest[:])
	query := url.Values{"client_id": {c.clientID}, "redirect_uri": {c.callbackURL}, "state": {state}, "code_challenge": {challenge}, "code_challenge_method": {"S256"}}
	return "https://github.com/login/oauth/authorize?" + query.Encode()
}

// VerifyInstallation exchanges the short-lived OAuth code and asks GitHub for
// the installations visible to that user. The user token is never returned or
// persisted. Merely receiving an installation ID from the setup redirect is
// not considered proof of ownership.
func (c *SetupClient) VerifyInstallation(ctx context.Context, code string, installationID int64, codeVerifier string) (InstallationIdentity, error) {
	if c == nil || c.httpClient == nil || strings.TrimSpace(code) == "" || installationID <= 0 || len(codeVerifier) < 43 || len(codeVerifier) > 128 {
		return InstallationIdentity{}, errors.New("valid GitHub OAuth code, PKCE verifier, and Installation ID are required")
	}
	form := url.Values{"client_id": {c.clientID}, "client_secret": {c.clientSecret}, "code": {code}, "redirect_uri": {c.callbackURL}, "code_verifier": {codeVerifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.oauthURL, strings.NewReader(form.Encode()))
	if err != nil {
		return InstallationIdentity{}, fmt.Errorf("create GitHub OAuth token request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.httpClient.Do(req)
	if err != nil {
		return InstallationIdentity{}, fmt.Errorf("exchange GitHub OAuth code: %w", err)
	}
	var token struct {
		AccessToken string `json:"access_token"`
		TokenType   string `json:"token_type"`
		Error       string `json:"error"`
	}
	if err := decodeGitHubJSON(response, &token); err != nil {
		var statusErr httpStatusError
		if errors.As(err, &statusErr) && statusErr.status >= 400 && statusErr.status < 500 {
			return InstallationIdentity{}, fmt.Errorf("%w: OAuth code exchange failed", ErrAuthorizationRejected)
		}
		return InstallationIdentity{}, fmt.Errorf("decode GitHub OAuth token response: %w", err)
	}
	if token.AccessToken == "" || token.Error != "" || !strings.EqualFold(token.TokenType, "bearer") {
		return InstallationIdentity{}, ErrAuthorizationRejected
	}
	for page := 1; page <= 100; page++ {
		endpoint := c.apiURL + "/user/installations?per_page=100&page=" + strconv.Itoa(page)
		req, err = http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return InstallationIdentity{}, fmt.Errorf("create GitHub user Installations request: %w", err)
		}
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+token.AccessToken)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		response, err = c.httpClient.Do(req)
		if err != nil {
			return InstallationIdentity{}, fmt.Errorf("verify GitHub user Installation: %w", err)
		}
		var installations struct {
			TotalCount    int `json:"total_count"`
			Installations []struct {
				ID      int64 `json:"id"`
				AppID   int64 `json:"app_id"`
				Account struct {
					Login string `json:"login"`
				} `json:"account"`
			} `json:"installations"`
		}
		if err := decodeGitHubJSON(response, &installations); err != nil {
			var statusErr httpStatusError
			if errors.As(err, &statusErr) && statusErr.status >= 400 && statusErr.status < 500 {
				return InstallationIdentity{}, fmt.Errorf("%w: user cannot access the requested Installation", ErrAuthorizationRejected)
			}
			return InstallationIdentity{}, fmt.Errorf("decode GitHub user Installations response: %w", err)
		}
		for _, installation := range installations.Installations {
			if installation.ID != installationID {
				continue
			}
			if installation.AppID != c.appID || !validLogin(installation.Account.Login) {
				return InstallationIdentity{}, ErrAuthorizationRejected
			}
			return InstallationIdentity{ID: installation.ID, AppID: installation.AppID, AccountLogin: installation.Account.Login}, nil
		}
		if page*100 >= installations.TotalCount || len(installations.Installations) == 0 {
			break
		}
	}
	return InstallationIdentity{}, ErrAuthorizationRejected
}

func decodeGitHubJSON(response *http.Response, dst any) error {
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxGitHubResponseBytes))
		return httpStatusError{status: response.StatusCode}
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return errors.New("GitHub response was not JSON")
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, maxGitHubResponseBytes+1))
	if err != nil {
		return err
	}
	if len(body) > maxGitHubResponseBytes {
		return errors.New("GitHub response exceeded the configured limit")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if err := decoder.Decode(dst); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("GitHub response contained trailing JSON")
	}
	return nil
}

func validSlug(value string) bool {
	if value == "" || len(value) > 100 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
			return false
		}
	}
	return true
}

func validLogin(value string) bool {
	if value == "" || len(value) > 39 {
		return false
	}
	for _, char := range value {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-') {
			return false
		}
	}
	return value[0] != '-' && value[len(value)-1] != '-'
}
