package githubapp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	maxActionsRunResponse = 1 << 20
	maxActionsJobPages    = 10
)

var ErrActionsRunLookup = errors.New("GitHub Actions run metadata lookup denied")

var (
	actionsWorkflowPathPattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
	actionsRunSHAPattern       = regexp.MustCompile(`^(?:[0-9a-fA-F]{40}|[0-9a-fA-F]{64})$`)
)

// ActionsRunSnapshot contains only authoritative, non-secret metadata needed
// to compare a queued webhook against the workflow/job GitHub currently knows.
type ActionsRunSnapshot struct {
	RepositoryID   int64
	HeadRepository int64
	RunID          int64
	RunAttempt     int64
	WorkflowID     int64
	WorkflowPath   string
	WorkflowRef    string
	WorkflowName   string
	HeadSHA        string
	HeadBranch     string
	Event          string
	JobID          int64
	JobName        string
	JobHeadBranch  string
	Labels         []string
}

type ActionsRunClient struct {
	http   *http.Client
	apiURL string
	now    func() time.Time
}

// ActionsRunToken is expected to be a short-lived repository-scoped token
// with actions:read only.
type ActionsRunToken interface {
	Value() string
	ExpiresAt() time.Time
}

func NewActionsRunClient(client *http.Client) (*ActionsRunClient, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	if copyClient.Timeout <= 0 || copyClient.Timeout > 10*time.Second {
		copyClient.Timeout = 10 * time.Second
	}
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ActionsRunClient{http: &copyClient, apiURL: "https://api.github.com", now: time.Now}, nil
}

// Fetch validates exact Repository/Run/Attempt/Job identities while resolving
// the workflow file path from GitHub. It fails closed on missing/ambiguous
// jobs, foreign repositories, pagination overflow, redirects, oversized
// responses, or malformed metadata.
func (c *ActionsRunClient) Fetch(ctx context.Context, token ActionsRunToken, repository RepositoryAccess,
	repositoryID, runID, attempt, jobID int64) (ActionsRunSnapshot, error) {
	if c == nil || c.http == nil || ctx == nil || !validActionsRunToken(token, c.now()) || !validLogin(repository.Owner) ||
		!validCredentialRepositoryName(repository.Name) || repositoryID <= 0 || runID <= 0 || attempt <= 0 || jobID <= 0 {
		return ActionsRunSnapshot{}, ErrActionsRunLookup
	}
	base := "/repos/" + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Name) + "/actions/runs/" + strconv.FormatInt(runID, 10)
	var run struct {
		ID         int64  `json:"id"`
		RunAttempt int64  `json:"run_attempt"`
		WorkflowID int64  `json:"workflow_id"`
		Name       string `json:"name"`
		HeadBranch string `json:"head_branch"`
		HeadSHA    string `json:"head_sha"`
		Event      string `json:"event"`
		Repository struct {
			ID int64 `json:"id"`
		} `json:"repository"`
		HeadRepo *struct {
			ID int64 `json:"id"`
		} `json:"head_repository"`
	}
	if err := c.get(ctx, token, base, &run); err != nil || run.ID != runID || run.RunAttempt != attempt || run.WorkflowID <= 0 ||
		run.Repository.ID != repositoryID || run.HeadRepo == nil || run.HeadRepo.ID <= 0 || !validActionsRunRef(run.HeadBranch) ||
		!actionsRunSHAPattern.MatchString(run.HeadSHA) || !validActionsRunEvent(run.Event) {
		return ActionsRunSnapshot{}, ErrActionsRunLookup
	}
	workflowRef := run.HeadBranch
	if strings.HasPrefix(workflowRef, "refs/") {
		// GitHub's REST workflow-run object exposes head_branch rather than a
		// qualified ref. Preserve an already-qualified value, otherwise make
		// the branch namespace explicit for policy comparison.
	} else {
		workflowRef = "refs/heads/" + workflowRef
	}
	workflowPath := "/repos/" + url.PathEscape(repository.Owner) + "/" + url.PathEscape(repository.Name) +
		"/actions/workflows/" + strconv.FormatInt(run.WorkflowID, 10)
	var workflow struct {
		ID   int64  `json:"id"`
		Path string `json:"path"`
	}
	if err := c.get(ctx, token, workflowPath, &workflow); err != nil || workflow.ID != run.WorkflowID || !actionsWorkflowPathPattern.MatchString(workflow.Path) {
		return ActionsRunSnapshot{}, ErrActionsRunLookup
	}
	jobsPath := base + "/attempts/" + strconv.FormatInt(attempt, 10) + "/jobs?per_page=100"
	var match *struct {
		ID         int64    `json:"id"`
		RunID      int64    `json:"run_id"`
		RunAttempt int64    `json:"run_attempt"`
		Name       string   `json:"name"`
		HeadBranch string   `json:"head_branch"`
		Labels     []string `json:"labels"`
	}
	for page := 1; page <= maxActionsJobPages; page++ {
		path := jobsPath + "&page=" + strconv.Itoa(page)
		var response struct {
			TotalCount int `json:"total_count"`
			Jobs       []struct {
				ID         int64    `json:"id"`
				RunID      int64    `json:"run_id"`
				RunAttempt int64    `json:"run_attempt"`
				Name       string   `json:"name"`
				HeadBranch string   `json:"head_branch"`
				Labels     []string `json:"labels"`
			} `json:"jobs"`
		}
		if err := c.get(ctx, token, path, &response); err != nil || len(response.Jobs) > 100 {
			return ActionsRunSnapshot{}, ErrActionsRunLookup
		}
		for i := range response.Jobs {
			candidate := &response.Jobs[i]
			if candidate.ID != jobID {
				continue
			}
			if match != nil || candidate.RunID != runID || candidate.RunAttempt != attempt || candidate.Name == "" ||
				!validActionsRunRef(candidate.HeadBranch) || !validActionsRunLabels(candidate.Labels) {
				return ActionsRunSnapshot{}, ErrActionsRunLookup
			}
			match = &struct {
				ID         int64    `json:"id"`
				RunID      int64    `json:"run_id"`
				RunAttempt int64    `json:"run_attempt"`
				Name       string   `json:"name"`
				HeadBranch string   `json:"head_branch"`
				Labels     []string `json:"labels"`
			}{candidate.ID, candidate.RunID, candidate.RunAttempt, candidate.Name, candidate.HeadBranch, append([]string(nil), candidate.Labels...)}
		}
		if len(response.Jobs) < 100 {
			break
		}
		if page == maxActionsJobPages {
			return ActionsRunSnapshot{}, ErrActionsRunLookup
		}
	}
	if match == nil {
		return ActionsRunSnapshot{}, ErrActionsRunLookup
	}
	return ActionsRunSnapshot{RepositoryID: run.Repository.ID, HeadRepository: run.HeadRepo.ID, RunID: run.ID, RunAttempt: run.RunAttempt,
		WorkflowID: run.WorkflowID, WorkflowPath: workflow.Path, WorkflowRef: workflowRef, WorkflowName: run.Name,
		HeadSHA: strings.ToLower(run.HeadSHA), HeadBranch: run.HeadBranch, Event: run.Event, JobID: match.ID,
		JobName: match.Name, JobHeadBranch: match.HeadBranch, Labels: match.Labels}, nil
}

func (c *ActionsRunClient) get(ctx context.Context, token ActionsRunToken, path string, destination any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(c.apiURL, "/")+path, nil)
	if err != nil {
		return ErrActionsRunLookup
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token.Value())
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrActionsRunLookup
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK || resp.ContentLength > maxActionsRunResponse {
		return ErrActionsRunLookup
	}
	decoder := json.NewDecoder(io.LimitReader(resp.Body, maxActionsRunResponse+1))
	if err := decoder.Decode(destination); err != nil {
		return ErrActionsRunLookup
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return ErrActionsRunLookup
	}
	return nil
}

func validActionsRunToken(token ActionsRunToken, now time.Time) bool {
	return token != nil && token.Value() != "" && len(token.Value()) <= 16384 && !strings.ContainsAny(token.Value(), "\r\n\x00 ") && token.ExpiresAt().After(now.Add(30*time.Second))
}

func validActionsRunEvent(event string) bool {
	if event == "" || len(event) > 100 || strings.ContainsAny(event, "\r\n\x00") {
		return false
	}
	return true
}

func validActionsRunRef(ref string) bool {
	return ref != "" && len(ref) <= 255 && !strings.HasPrefix(ref, "-") && !strings.ContainsAny(ref, "\r\n\x00")
}

func validActionsRunLabels(labels []string) bool {
	if len(labels) == 0 || len(labels) > 100 {
		return false
	}
	for _, label := range labels {
		if label == "" || len(label) > 100 || strings.ContainsAny(label, "\r\n\x00") {
			return false
		}
	}
	return true
}

func (s ActionsRunSnapshot) String() string {
	return fmt.Sprintf("GitHub Actions run metadata (repository %d, run %d/%d, job %d)", s.RepositoryID, s.RunID, s.RunAttempt, s.JobID)
}
