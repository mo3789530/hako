package githubapp

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"
)

var (
	ErrEnvironmentProtectionUnavailable = errors.New("GitHub Environment protection unavailable")
	ErrEnvironmentApprovalMissing       = errors.New("GitHub Environment approval protection missing")
	ErrEnvironmentRunMismatch           = errors.New("GitHub Actions run identity mismatch")
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
			Type              string `json:"type"`
			PreventSelfReview bool   `json:"prevent_self_review"`
			Reviewers         []struct {
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
		if rule.Type != "required_reviewers" || len(rule.Reviewers) == 0 || !rule.PreventSelfReview {
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

// EnvironmentRunIdentity binds the verified OIDC claims to a concrete active
// GitHub Actions run attempt and its Environment review history.
type EnvironmentRunIdentity struct {
	RepositoryID      int64
	RepositoryOwnerID int64
	RunID             string
	RunAttempt        string
	Ref               string
	SHA               string
	WorkflowRef       string
	EventName         string
	Environment       string
}

// VerifyEnvironmentApprovalForRun requires the exact run attempt to be live,
// match the signed repository/ref/commit/workflow claims, have no pending
// deployment for the Environment, and have an approved reviewer record.
func (c *TokenClient) VerifyEnvironmentApprovalForRun(ctx context.Context, token InstallationToken, repository RepositoryAccess, identity EnvironmentRunIdentity) error {
	runID, runAttempt, ownerID, err := parseEnvironmentRunIdentity(identity)
	if c == nil || ctx == nil || c.now == nil || token.Value() == "" || token.ExpiresAt().IsZero() || !token.ExpiresAt().After(c.now()) ||
		!validLogin(repository.Owner) || !validCredentialRepositoryName(repository.Name) || err != nil || identity.RepositoryID <= 0 {
		return ErrEnvironmentRunMismatch
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	if err := c.VerifyEnvironmentApproval(ctx, token, repository, identity.Environment); err != nil {
		return err
	}
	path := "/repos/" + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Name)
	var run struct {
		ID         int64  `json:"id"`
		RunAttempt int64  `json:"run_attempt"`
		Status     string `json:"status"`
		Event      string `json:"event"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		Path       string `json:"path"`
		Repository struct {
			ID    int64 `json:"id"`
			Owner struct {
				ID int64 `json:"id"`
			} `json:"owner"`
		} `json:"repository"`
	}
	runPath := path + "/actions/runs/" + identity.RunID + "/attempts/" + identity.RunAttempt
	if err := c.getInstallationJSON(ctx, token, runPath, &run); err != nil {
		return ErrEnvironmentProtectionUnavailable
	}
	if run.ID != runID || run.RunAttempt != runAttempt || run.Repository.ID != identity.RepositoryID ||
		run.Repository.Owner.ID != ownerID || run.Status != "in_progress" || run.Event != identity.EventName || run.HeadBranch != refName(identity.Ref) ||
		run.HeadSHA != identity.SHA || workflowFilePath(run.Path) == "" || workflowFilePath(run.Path) != workflowFilePath(identity.WorkflowRef) {
		return ErrEnvironmentRunMismatch
	}

	var pending []struct {
		Environment struct {
			Name string `json:"name"`
		} `json:"environment"`
	}
	if err := c.getInstallationJSON(ctx, token, path+"/actions/runs/"+identity.RunID+"/pending_deployments", &pending); err != nil {
		return ErrEnvironmentProtectionUnavailable
	}
	for _, deployment := range pending {
		if deployment.Environment.Name == identity.Environment {
			return ErrEnvironmentApprovalMissing
		}
		if deployment.Environment.Name == "" {
			return ErrEnvironmentProtectionUnavailable
		}
	}

	var approvals []struct {
		State        string `json:"state"`
		Environments []struct {
			Name string `json:"name"`
		} `json:"environments"`
		User struct {
			ID int64 `json:"id"`
		} `json:"user"`
	}
	if err := c.getInstallationJSON(ctx, token, path+"/actions/runs/"+identity.RunID+"/approvals", &approvals); err != nil {
		return ErrEnvironmentProtectionUnavailable
	}
	for _, approval := range approvals {
		if approval.State != "approved" || approval.User.ID <= 0 {
			continue
		}
		for _, environment := range approval.Environments {
			if environment.Name == identity.Environment {
				return nil
			}
		}
	}
	return ErrEnvironmentApprovalMissing
}

func parseEnvironmentRunIdentity(identity EnvironmentRunIdentity) (int64, int64, int64, error) {
	if !validEnvironmentName(identity.Environment) || !validGitHubNumericID(identity.RunID) || !validGitHubNumericID(identity.RunAttempt) ||
		!validGitHubNumericID(strconv.FormatInt(identity.RepositoryOwnerID, 10)) || (len(identity.SHA) != 40 && len(identity.SHA) != 64) ||
		!strings.HasPrefix(identity.Ref, "refs/heads/") && !strings.HasPrefix(identity.Ref, "refs/tags/") ||
		identity.EventName != "push" && identity.EventName != "workflow_dispatch" || workflowFilePath(identity.WorkflowRef) == "" {
		return 0, 0, 0, ErrEnvironmentRunMismatch
	}
	for _, char := range identity.SHA {
		if !(char >= '0' && char <= '9' || char >= 'a' && char <= 'f') {
			return 0, 0, 0, ErrEnvironmentRunMismatch
		}
	}
	runID, err := strconv.ParseInt(identity.RunID, 10, 64)
	if err != nil {
		return 0, 0, 0, ErrEnvironmentRunMismatch
	}
	runAttempt, err := strconv.ParseInt(identity.RunAttempt, 10, 64)
	if err != nil {
		return 0, 0, 0, ErrEnvironmentRunMismatch
	}
	return runID, runAttempt, identity.RepositoryOwnerID, nil
}

func validGitHubNumericID(value string) bool {
	if value == "" || len(value) > 19 || (len(value) > 1 && value[0] == '0') {
		return false
	}
	for _, char := range value {
		if char < '0' || char > '9' {
			return false
		}
	}
	return true
}

func refName(ref string) string {
	if strings.HasPrefix(ref, "refs/heads/") {
		return strings.TrimPrefix(ref, "refs/heads/")
	}
	return strings.TrimPrefix(ref, "refs/tags/")
}

func workflowFilePath(value string) string {
	if at := strings.IndexByte(value, '@'); at >= 0 {
		value = value[:at]
	}
	if strings.HasPrefix(value, ".github/workflows/") {
		return value
	}
	if slash := strings.Index(value, "/.github/workflows/"); slash >= 0 {
		return value[slash+1:]
	}
	return ""
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
