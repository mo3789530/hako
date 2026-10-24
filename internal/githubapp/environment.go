package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"
)

var (
	ErrEnvironmentProtectionUnavailable = errors.New("GitHub Environment protection unavailable")
	ErrEnvironmentApprovalMissing       = errors.New("GitHub Environment approval protection missing")
)

// VerifyEnvironmentApproval verifies that an exact GitHub Environment has a
// required-reviewers protection rule. The caller must supply a short-lived,
// repository-scoped Installation token with Actions read permission.
func (c *TokenClient) VerifyEnvironmentApproval(ctx context.Context, token InstallationToken, repository RepositoryAccess, environment string) error {
	if c == nil || ctx == nil || c.now == nil || token.Value() == "" || token.ExpiresAt().IsZero() || !token.ExpiresAt().After(c.now()) ||
		!validLogin(repository.Owner) || !validCredentialRepositoryName(repository.Name) || !validEnvironmentName(environment) {
		return ErrEnvironmentProtectionUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	path := fmt.Sprintf("/repos/%s/%s/environments/%s", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), url.PathEscape(environment))
	var response struct {
		Name            string `json:"name"`
		ProtectionRules []struct {
			Type      string `json:"type"`
			Reviewers []struct {
				Type     string `json:"type"`
				Reviewer struct {
					ID    int64  `json:"id"`
					Login string `json:"login"`
				} `json:"reviewer"`
			} `json:"reviewers"`
		} `json:"protection_rules"`
	}
	if err := c.getInstallationJSON(ctx, token, path, &response); err != nil {
		return ErrEnvironmentProtectionUnavailable
	}
	if response.Name != environment {
		return ErrEnvironmentProtectionUnavailable
	}
	for _, rule := range response.ProtectionRules {
		if rule.Type != "required_reviewers" || len(rule.Reviewers) == 0 {
			continue
		}
		valid := true
		for _, reviewer := range rule.Reviewers {
			if reviewer.Type != "User" && reviewer.Type != "Team" {
				valid = false
				break
			}
			if reviewer.Reviewer.ID <= 0 && strings.TrimSpace(reviewer.Reviewer.Login) == "" {
				valid = false
				break
			}
		}
		if valid {
			return nil
		}
	}
	return ErrEnvironmentApprovalMissing
}

func validEnvironmentName(value string) bool {
	if value == "" || len(value) > 64 || strings.TrimSpace(value) != value || strings.Contains(value, "..") {
		return false
	}
	for _, r := range value {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-') {
			return false
		}
	}
	return true
}
