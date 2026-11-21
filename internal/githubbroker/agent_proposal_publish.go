package githubbroker

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var (
	ErrAgentProposalGitHubDenied = errors.New("Agent proposal GitHub publication denied")
	agentProposalCommitSHA       = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	agentProposalDigest          = regexp.MustCompile(`^[0-9a-f]{64}$`)
	agentProposalBranchPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
)

const (
	maxAgentProposalGitHubBody     = 1 << 20
	maxAgentProposalGitHubResponse = 2 << 20
)

type AgentProposalGitHubPublishRequest struct {
	Repository      githubapp.RepositoryAccess
	DefaultBranch   string
	BaseBranch      string
	BaseCommitSHA   string
	BranchName      string
	ProposalSHA256  string
	RunID           string
	AttemptID       string
	Archive         []byte
	Policy          agenttask.WritePolicy
	PassedChecks    map[string]bool
	OpenPullRequest bool
}

type AgentProposalGitHubPublishResult struct {
	BranchName        string
	BranchCommitSHA   string
	PullRequestNumber int64
	Recovered         bool
}

type AgentProposalWriteToken interface {
	Value() string
	ExpiresAt() time.Time
}

// AgentProposalGitHubWriter performs only Git data and same-Repository PR
// operations. It receives a broker-local exact-scope token and never logs or
// serializes it. A retry discovers and validates the deterministic branch and
// matching PR before creating anything else.
type AgentProposalGitHubWriter struct {
	baseURL *url.URL
	client  *http.Client
	now     func() time.Time
}

func NewAgentProposalGitHubWriter(apiURL string, client *http.Client) (*AgentProposalGitHubWriter, error) {
	base, err := url.Parse(strings.TrimSpace(apiURL))
	if err != nil || base.Scheme != "https" || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" {
		return nil, ErrUnavailable
	}
	if client == nil {
		client = &http.Client{Timeout: 55 * time.Second}
	} else {
		clientCopy := *client
		if clientCopy.Timeout <= 0 || clientCopy.Timeout > 55*time.Second {
			clientCopy.Timeout = 55 * time.Second
		}
		clientCopy.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
		client = &clientCopy
	}
	return &AgentProposalGitHubWriter{baseURL: base, client: client, now: time.Now}, nil
}

func (p *AgentProposalGitHubWriter) Publish(ctx context.Context, token AgentProposalWriteToken,
	request AgentProposalGitHubPublishRequest) (AgentProposalGitHubPublishResult, error) {
	return p.PublishFenced(ctx, token, request, nil)
}

// PublishFenced invokes the supplied authorization fence immediately before
// and after creating a branch ref or pull request. Unreachable Git objects may
// be prepared before a fence, but no branch/PR ref is created after a stale
// Tenant/App/Repository/policy pin is observed.
func (p *AgentProposalGitHubWriter) PublishFenced(ctx context.Context, token AgentProposalWriteToken,
	request AgentProposalGitHubPublishRequest, fence func(context.Context) error) (AgentProposalGitHubPublishResult, error) {
	if p == nil || ctx == nil || token.Value() == "" || !token.ExpiresAt().After(p.now().Add(time.Minute)) ||
		!validLogin(request.Repository.Owner) || !validCredentialRepository(request.Repository.Name) ||
		!validBrokerBranch(request.DefaultBranch) || request.BaseBranch != request.DefaultBranch ||
		!agentProposalCommitSHA.MatchString(request.BaseCommitSHA) ||
		!validBrokerBranch(request.BranchName) || !agentProposalDigest.MatchString(request.ProposalSHA256) ||
		!validCheckoutRunID(domain.WorkloadRunID(request.RunID)) || !validCheckoutRunID(domain.WorkloadRunID(request.AttemptID)) ||
		agenttask.AuthorizeBranchPush(request.Policy, request.BranchName, request.DefaultBranch, true, request.PassedChecks) != nil ||
		request.OpenPullRequest && agenttask.AuthorizePullRequest(request.Policy, request.BranchName, request.BaseBranch,
			request.DefaultBranch, true, request.PassedChecks) != nil || len(request.Archive) < 1 ||
		len(request.Archive) > workloadsource.MaxAgentProposalArchiveBytes {
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	digest := sha256.Sum256(request.Archive)
	if len(request.Archive) == 0 || hex.EncodeToString(digest[:]) != request.ProposalSHA256 {
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	files, err := workloadsource.ReadAgentProposalContents(request.Archive)
	if err != nil || len(files) == 0 {
		workloadsource.ClearAgentProposalContents(files)
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	defer workloadsource.ClearAgentProposalContents(files)

	owner, repository := request.Repository.Owner, request.Repository.Name
	basePath := "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(repository)
	baseRef, err := p.getRef(ctx, token.Value(), basePath, request.BaseBranch)
	if err != nil || baseRef != request.BaseCommitSHA {
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	treeSHA, err := p.createProposalTree(ctx, token.Value(), basePath, request, files)
	if err != nil {
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	branchCommit, recovered, err := p.findProposalBranch(ctx, token.Value(), basePath, request, treeSHA)
	if err != nil {
		return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
	}
	if branchCommit == "" {
		branchCommit, err = p.createProposalCommit(ctx, token.Value(), basePath, request, treeSHA)
		if err != nil {
			return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
		}
		if err := runAgentProposalFence(ctx, fence); err != nil {
			return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
		}
		if err := p.createRef(ctx, token.Value(), basePath, request.BranchName, branchCommit); err != nil {
			// A lost response or a concurrent retry may have created this exact
			// branch. Re-read and accept only our marker and immutable parent.
			confirmed, _, recoverErr := p.findProposalBranch(ctx, token.Value(), basePath, request, treeSHA)
			if recoverErr != nil || confirmed != branchCommit {
				return AgentProposalGitHubPublishResult{}, ErrAgentProposalGitHubDenied
			}
			recovered = true
		}
		if err := runAgentProposalFence(ctx, fence); err != nil {
			return AgentProposalGitHubPublishResult{BranchName: request.BranchName, BranchCommitSHA: branchCommit, Recovered: recovered}, ErrAgentProposalGitHubDenied
		}
	}
	result := AgentProposalGitHubPublishResult{BranchName: request.BranchName, BranchCommitSHA: branchCommit, Recovered: recovered}
	currentBase, baseErr := p.getRef(ctx, token.Value(), basePath, request.DefaultBranch)
	currentBranch, _, branchErr := p.findProposalBranch(ctx, token.Value(), basePath, request, treeSHA)
	if baseErr != nil || branchErr != nil || currentBase != request.BaseCommitSHA || currentBranch != branchCommit {
		return result, ErrAgentProposalGitHubDenied
	}
	if !request.OpenPullRequest {
		return result, nil
	}
	pr, exists, err := p.findProposalPullRequest(ctx, token.Value(), basePath, request)
	if err != nil {
		return result, ErrAgentProposalGitHubDenied
	}
	prWriteAttempted := false
	if !exists {
		if err := runAgentProposalFence(ctx, fence); err != nil {
			return result, ErrAgentProposalGitHubDenied
		}
		prWriteAttempted = true
		pr, err = p.createProposalPullRequest(ctx, token.Value(), basePath, request)
		if err != nil {
			// GitHub may have created the PR before the response was lost.
			pr, exists, err = p.findProposalPullRequest(ctx, token.Value(), basePath, request)
			if err != nil || !exists {
				return result, ErrAgentProposalGitHubDenied
			}
			result.Recovered = true
		}
	} else {
		result.Recovered = true
	}
	result.PullRequestNumber = pr.Number
	if prWriteAttempted {
		if err := runAgentProposalFence(ctx, fence); err != nil {
			return result, ErrAgentProposalGitHubDenied
		}
	}
	return result, nil
}

func runAgentProposalFence(ctx context.Context, fence func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if fence == nil {
		return nil
	}
	return fence(ctx)
}

func (p *AgentProposalGitHubWriter) getRef(ctx context.Context, token, basePath, branch string) (string, error) {
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	path := basePath + "/git/ref/heads/" + url.PathEscape(branch)
	if err := p.call(ctx, http.MethodGet, path, token, nil, &ref); err != nil {
		return "", err
	}
	if ref.Ref != "refs/heads/"+branch || !agentProposalCommitSHA.MatchString(ref.Object.SHA) {
		return "", ErrAgentProposalGitHubDenied
	}
	return ref.Object.SHA, nil
}

func (p *AgentProposalGitHubWriter) findProposalBranch(ctx context.Context, token, basePath string,
	request AgentProposalGitHubPublishRequest, expectedTreeSHA string) (string, bool, error) {
	sha, err := p.getRef(ctx, token, basePath, request.BranchName)
	if err != nil {
		if isGitHubNotFound(err) {
			return "", false, nil
		}
		return "", false, err
	}
	var commit struct {
		SHA     string `json:"sha"`
		Message string `json:"message"`
		Tree    struct {
			SHA string `json:"sha"`
		} `json:"tree"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
	}
	path := basePath + "/git/commits/" + url.PathEscape(sha)
	if err := p.call(ctx, http.MethodGet, path, token, nil, &commit); err != nil || commit.SHA != sha || commit.Tree.SHA != expectedTreeSHA ||
		len(commit.Parents) != 1 || commit.Parents[0].SHA != request.BaseCommitSHA || !strings.Contains(commit.Message, proposalCommitMarker(request)) {
		return "", false, ErrAgentProposalGitHubDenied
	}
	return sha, true, nil
}

func (p *AgentProposalGitHubWriter) createProposalTree(ctx context.Context, token, basePath string,
	request AgentProposalGitHubPublishRequest, files []workloadsource.AgentProposalContent) (string, error) {
	var baseCommit struct {
		SHA  string `json:"sha"`
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := p.call(ctx, http.MethodGet, basePath+"/git/commits/"+url.PathEscape(request.BaseCommitSHA), token, nil, &baseCommit); err != nil ||
		baseCommit.SHA != request.BaseCommitSHA || !agentProposalCommitSHA.MatchString(baseCommit.Tree.SHA) {
		return "", ErrAgentProposalGitHubDenied
	}
	tree := make([]map[string]string, 0, len(files))
	for _, file := range files {
		var blob struct {
			SHA string `json:"sha"`
		}
		body := map[string]string{"content": base64.StdEncoding.EncodeToString(file.Content), "encoding": "base64"}
		if err := p.call(ctx, http.MethodPost, basePath+"/git/blobs", token, body, &blob); err != nil || !agentProposalCommitSHA.MatchString(blob.SHA) {
			return "", ErrAgentProposalGitHubDenied
		}
		tree = append(tree, map[string]string{"path": file.Path, "mode": "100644", "type": "blob", "sha": blob.SHA})
	}
	var createdTree struct {
		SHA string `json:"sha"`
	}
	if err := p.call(ctx, http.MethodPost, basePath+"/git/trees", token,
		map[string]any{"base_tree": baseCommit.Tree.SHA, "tree": tree}, &createdTree); err != nil || !agentProposalCommitSHA.MatchString(createdTree.SHA) {
		return "", ErrAgentProposalGitHubDenied
	}
	return createdTree.SHA, nil
}

func (p *AgentProposalGitHubWriter) createProposalCommit(ctx context.Context, token, basePath string,
	request AgentProposalGitHubPublishRequest, treeSHA string) (string, error) {
	var createdCommit struct {
		SHA string `json:"sha"`
	}
	commitBody := map[string]any{"message": proposalCommitMessage(request), "tree": treeSHA, "parents": []string{request.BaseCommitSHA}}
	if err := p.call(ctx, http.MethodPost, basePath+"/git/commits", token, commitBody, &createdCommit); err != nil ||
		!agentProposalCommitSHA.MatchString(createdCommit.SHA) {
		return "", ErrAgentProposalGitHubDenied
	}
	return createdCommit.SHA, nil
}

func (p *AgentProposalGitHubWriter) createRef(ctx context.Context, token, basePath, branch, commitSHA string) error {
	var created struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := p.call(ctx, http.MethodPost, basePath+"/git/refs", token,
		map[string]string{"ref": "refs/heads/" + branch, "sha": commitSHA}, &created); err != nil {
		return err
	}
	if created.Ref != "refs/heads/"+branch || created.Object.SHA != commitSHA {
		return ErrAgentProposalGitHubDenied
	}
	return nil
}

type proposalPullRequest struct {
	Number int64  `json:"number"`
	State  string `json:"state"`
	Head   struct {
		Ref  string `json:"ref"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
	Base struct {
		Ref string `json:"ref"`
	} `json:"base"`
}

func (p *AgentProposalGitHubWriter) findProposalPullRequest(ctx context.Context, token, basePath string,
	request AgentProposalGitHubPublishRequest) (proposalPullRequest, bool, error) {
	query := url.Values{"head": {request.Repository.Owner + ":" + request.BranchName}, "state": {"all"}, "per_page": {"100"}}
	var prs []proposalPullRequest
	if err := p.call(ctx, http.MethodGet, basePath+"/pulls?"+query.Encode(), token, nil, &prs); err != nil {
		return proposalPullRequest{}, false, err
	}
	if len(prs) == 100 {
		return proposalPullRequest{}, false, ErrAgentProposalGitHubDenied
	}
	var found *proposalPullRequest
	for i := range prs {
		pr := prs[i]
		if pr.Head.Ref != request.BranchName {
			continue
		}
		if pr.Number < 1 || pr.Head.Repo == nil || pr.Head.Repo.FullName != request.Repository.Owner+"/"+request.Repository.Name ||
			pr.Base.Ref != request.BaseBranch || (pr.State != "open" && pr.State != "closed") || found != nil {
			return proposalPullRequest{}, false, ErrAgentProposalGitHubDenied
		}
		found = &pr
	}
	if found == nil {
		return proposalPullRequest{}, false, nil
	}
	return *found, true, nil
}

func (p *AgentProposalGitHubWriter) createProposalPullRequest(ctx context.Context, token, basePath string,
	request AgentProposalGitHubPublishRequest) (proposalPullRequest, error) {
	var pr proposalPullRequest
	body := map[string]string{"title": "Hako Agent proposal " + request.RunID,
		"head": request.BranchName, "base": request.BaseBranch,
		"body": "Automated Hako Agent proposal. Required Hako checks passed; review the changes before merging.\n\n" + proposalCommitMarker(request)}
	if err := p.call(ctx, http.MethodPost, basePath+"/pulls", token, body, &pr); err != nil || pr.Number < 1 ||
		pr.Head.Ref != request.BranchName || pr.Head.Repo == nil || pr.Head.Repo.FullName != request.Repository.Owner+"/"+request.Repository.Name ||
		pr.Base.Ref != request.BaseBranch || pr.State != "open" {
		return proposalPullRequest{}, ErrAgentProposalGitHubDenied
	}
	return pr, nil
}

func (p *AgentProposalGitHubWriter) call(ctx context.Context, method, path, token string, body, dst any) error {
	var requestBody []byte
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil || len(encoded) > maxAgentProposalGitHubBody {
			return ErrAgentProposalGitHubDenied
		}
		requestBody = encoded
	}
	relative, err := url.Parse(path)
	if err != nil {
		return ErrAgentProposalGitHubDenied
	}
	endpoint := *p.baseURL
	basePath := strings.TrimSuffix(endpoint.EscapedPath(), "/")
	endpoint.Path = strings.TrimSuffix(endpoint.Path, "/") + relative.Path
	endpoint.RawPath = basePath + relative.EscapedPath()
	endpoint.RawQuery = relative.RawQuery
	req, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(requestBody))
	if err != nil {
		return ErrAgentProposalGitHubDenied
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := p.client.Do(req)
	if err != nil {
		return ErrAgentProposalGitHubDenied
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotFound {
		return &agentProposalGitHubError{status: response.StatusCode}
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return ErrAgentProposalGitHubDenied
	}
	if dst == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxAgentProposalGitHubResponse+1))
		return nil
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxAgentProposalGitHubResponse+1))
	if err != nil || len(data) > maxAgentProposalGitHubResponse || json.Unmarshal(data, dst) != nil {
		return ErrAgentProposalGitHubDenied
	}
	return nil
}

type agentProposalGitHubError struct{ status int }

func (*agentProposalGitHubError) Error() string { return ErrAgentProposalGitHubDenied.Error() }
func (*agentProposalGitHubError) Unwrap() error { return ErrAgentProposalGitHubDenied }

func isGitHubNotFound(err error) bool {
	var githubErr *agentProposalGitHubError
	return errors.As(err, &githubErr) && githubErr.status == http.StatusNotFound
}

func proposalCommitMarker(request AgentProposalGitHubPublishRequest) string {
	return "Hako-Run: " + request.RunID + "\nHako-Attempt: " + request.AttemptID + "\nHako-Proposal-SHA256: " +
		request.ProposalSHA256 + "\nHako-Base-SHA: " + request.BaseCommitSHA
}

func proposalCommitMessage(request AgentProposalGitHubPublishRequest) string {
	return "Hako Agent proposal " + request.RunID + "\n\n" + proposalCommitMarker(request)
}

func validBrokerBranch(branch string) bool {
	if !agentProposalBranchPattern.MatchString(branch) || strings.Contains(branch, "..") || strings.Contains(branch, "//") ||
		strings.HasPrefix(branch, "/") || strings.HasSuffix(branch, "/") || strings.Contains(branch, "@{") {
		return false
	}
	for _, component := range strings.Split(branch, "/") {
		if component == "" || strings.HasPrefix(component, ".") || strings.HasSuffix(component, ".") || strings.HasSuffix(component, ".lock") {
			return false
		}
	}
	return true
}

func (p AgentProposalGitHubPublishResult) String() string {
	return fmt.Sprintf("Agent proposal branch %s commit %s PR %d recovered=%t", p.BranchName, p.BranchCommitSHA, p.PullRequestNumber, p.Recovered)
}
