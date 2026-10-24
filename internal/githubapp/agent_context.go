package githubapp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

const maxAgentIssueResponse = 1 << 20

var agentCommitSHAPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// AgentIssueContext is bounded repository data fetched by the trusted broker.
// It must be passed only to the authorized Agent Run and never stored in
// webhook inbox payloads or credential logs.
type AgentIssueContext struct {
	IssueNumber int64  `json:"issue_number"`
	Title       string `json:"title"`
	Body        string `json:"body"`
	CommitSHA   string `json:"commit_sha"`
	DefaultRef  string `json:"default_ref"`
}

// ResolveAgentIssueContext fetches the exact Issue and the current immutable
// default-branch commit using an already repository-scoped Tenant App token.
// Pull requests, closed Issues, malformed metadata, redirects, and oversized
// responses fail closed. The caller must revalidate the source delivery and
// persist the returned SHA as the Run's immutable commit pin.
func (c *TokenClient) ResolveAgentIssueContext(ctx context.Context, token InstallationToken, repository RepositoryAccess, issueNumber int64) (AgentIssueContext, error) {
	if c == nil || c.now == nil || ctx == nil || !validLogin(repository.Owner) || !validCredentialRepositoryName(repository.Name) ||
		token.Value() == "" || token.ExpiresAt().IsZero() || !token.ExpiresAt().After(c.now()) || issueNumber <= 0 {
		return AgentIssueContext{}, credentialError("invalid_request")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	issuePath := fmt.Sprintf("/repos/%s/%s/issues/%d", url.PathEscape(repository.Owner), url.PathEscape(repository.Name), issueNumber)
	var issue struct {
		Number      int64   `json:"number"`
		Title       string  `json:"title"`
		Body        *string `json:"body"`
		State       string  `json:"state"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	if err := c.getInstallationJSON(ctx, token, issuePath, &issue); err != nil || issue.Number != issueNumber || issue.State != "open" || issue.PullRequest != nil ||
		strings.TrimSpace(issue.Title) == "" || len(issue.Title) > 1024 || issue.Body != nil && len(*issue.Body) > 64<<10 {
		return AgentIssueContext{}, credentialError("issue_context_unavailable")
	}
	var repositoryMetadata struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.getInstallationJSON(ctx, token, "/repos/"+url.PathEscape(repository.Owner)+"/"+url.PathEscape(repository.Name), &repositoryMetadata); err != nil ||
		repositoryMetadata.DefaultBranch == "" || len(repositoryMetadata.DefaultBranch) > 255 || strings.ContainsAny(repositoryMetadata.DefaultBranch, "\r\n\x00") {
		return AgentIssueContext{}, credentialError("issue_context_unavailable")
	}
	query := url.Values{"sha": []string{repositoryMetadata.DefaultBranch}, "per_page": []string{"1"}}
	var commits []struct {
		SHA string `json:"sha"`
	}
	commitPath := "/repos/" + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Name) + "/commits?" + query.Encode()
	if err := c.getInstallationJSON(ctx, token, commitPath, &commits); err != nil || len(commits) != 1 || !agentCommitSHAPattern.MatchString(commits[0].SHA) {
		return AgentIssueContext{}, credentialError("issue_context_unavailable")
	}
	body := ""
	if issue.Body != nil {
		body = *issue.Body
	}
	return AgentIssueContext{IssueNumber: issue.Number, Title: issue.Title, Body: body, CommitSHA: commits[0].SHA, DefaultRef: repositoryMetadata.DefaultBranch}, nil
}

func (c *TokenClient) getInstallationJSON(ctx context.Context, token InstallationToken, path string, destination any) error {
	if !strings.HasPrefix(path, "/repos/") || strings.Contains(path, "#") || strings.Contains(path, "\r") || strings.Contains(path, "\n") {
		return errors.New("invalid GitHub API path")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL+path, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token.Value())
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	response, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	mediaType, _, mediaErr := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if response.StatusCode < 200 || response.StatusCode >= 300 || mediaErr != nil || mediaType != "application/json" {
		return errors.New("GitHub API rejected Agent context request")
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAgentIssueResponse+1))
	if err != nil || len(data) == 0 || len(data) > maxAgentIssueResponse {
		return errors.New("invalid bounded GitHub API response")
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(destination); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("GitHub API response has trailing data")
	}
	return nil
}
