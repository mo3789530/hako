package githubapp

import (
	"bytes"
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
	maxCheckResponseBytes = 256 << 10
	maxCheckPayloadBytes  = 64 << 10
	maxCheckAnnotations   = 50
	maxCheckLookupPages   = 10
)

var checkCommitPattern = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

type CheckRunStatus string
type CheckRunConclusion string

const (
	CheckQueued     CheckRunStatus = "queued"
	CheckInProgress CheckRunStatus = "in_progress"
	CheckCompleted  CheckRunStatus = "completed"
)

type CheckAnnotation struct {
	Path            string `json:"path"`
	StartLine       int    `json:"start_line"`
	EndLine         int    `json:"end_line"`
	StartColumn     int    `json:"start_column,omitempty"`
	EndColumn       int    `json:"end_column,omitempty"`
	AnnotationLevel string `json:"annotation_level"`
	Message         string `json:"message"`
	Title           string `json:"title,omitempty"`
	RawDetails      string `json:"raw_details,omitempty"`
}

type CheckOutput struct {
	Title       string            `json:"title"`
	Summary     string            `json:"summary"`
	Text        string            `json:"text,omitempty"`
	Annotations []CheckAnnotation `json:"annotations,omitempty"`
}

type CheckRunRequest struct {
	Name        string             `json:"name"`
	ExternalID  string             `json:"external_id"`
	HeadSHA     string             `json:"head_sha"`
	Status      CheckRunStatus     `json:"status"`
	Conclusion  CheckRunConclusion `json:"conclusion,omitempty"`
	StartedAt   time.Time          `json:"started_at"`
	CompletedAt *time.Time         `json:"completed_at,omitempty"`
	Output      CheckOutput        `json:"output"`
}

type CheckRun struct {
	ID         int64              `json:"id"`
	Name       string             `json:"name"`
	ExternalID string             `json:"external_id"`
	HeadSHA    string             `json:"head_sha"`
	Status     CheckRunStatus     `json:"status"`
	Conclusion CheckRunConclusion `json:"conclusion,omitempty"`
}

type ChecksClient struct {
	http   *http.Client
	apiURL string
	now    func() time.Time
}

// CheckToken exposes only the short-lived bearer value and expiry needed by
// the Checks client. Production callers should obtain it from TokenClient;
// it is not a general-purpose token source.
type CheckToken interface {
	Value() string
	ExpiresAt() time.Time
}

func NewChecksClient(httpClient *http.Client) (*ChecksClient, error) {
	client := &http.Client{Timeout: 10 * time.Second}
	if httpClient != nil {
		*client = *httpClient
	}
	if client.Timeout <= 0 || client.Timeout > 10*time.Second {
		client.Timeout = 10 * time.Second
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &ChecksClient{http: client, apiURL: "https://api.github.com", now: time.Now}, nil
}

// EnsureCheckRun updates the exact persisted Check ID or recovers a Check
// whose create succeeded before its ID was durably stored. external_id and
// commit SHA are verified on every path to prevent cross-Run result updates.
func (c *ChecksClient) EnsureCheckRun(ctx context.Context, token CheckToken, repository RepositoryAccess, checkRunID int64, request CheckRunRequest) (CheckRun, error) {
	if c == nil || ctx == nil || c.http == nil || !validCheckToken(token, c.now()) || !validLogin(repository.Owner) || !validCredentialRepositoryName(repository.Name) || validateCheckRequest(request) != nil || checkRunID < 0 {
		return CheckRun{}, errors.New("invalid GitHub Check request")
	}
	owner, name := url.PathEscape(repository.Owner), url.PathEscape(repository.Name)
	basePath := "/repos/" + owner + "/" + name + "/check-runs"
	if checkRunID > 0 {
		return c.updateCheckRun(ctx, token, basePath+"/"+strconv.FormatInt(checkRunID, 10), checkRunID, request)
	}
	query := url.Values{}
	query.Set("check_name", request.Name)
	query.Set("head_sha", request.HeadSHA)
	query.Set("per_page", "100")
	query.Set("filter", "all")
	var match *CheckRun
	for page := 1; page <= maxCheckLookupPages; page++ {
		query.Set("page", strconv.Itoa(page))
		var existing struct {
			CheckRuns []CheckRun `json:"check_runs"`
		}
		if err := c.do(ctx, http.MethodGet, basePath+"?"+query.Encode(), token.Value(), nil, &existing); err != nil {
			return CheckRun{}, err
		}
		for index := range existing.CheckRuns {
			candidate := existing.CheckRuns[index]
			if candidate.ExternalID != request.ExternalID {
				continue
			}
			if candidate.ID <= 0 || candidate.HeadSHA != request.HeadSHA || candidate.Name != request.Name || match != nil {
				return CheckRun{}, errors.New("ambiguous existing GitHub Check result")
			}
			match = &candidate
		}
		if len(existing.CheckRuns) < 100 {
			break
		}
		if page == maxCheckLookupPages {
			return CheckRun{}, errors.New("GitHub Check recovery search exceeded its page limit")
		}
	}
	if match != nil {
		return c.updateCheckRun(ctx, token, basePath+"/"+strconv.FormatInt(match.ID, 10), match.ID, request)
	}
	var created CheckRun
	body, err := json.Marshal(request)
	if err != nil {
		return CheckRun{}, errors.New("GitHub Check request encoding failed")
	}
	if err := c.do(ctx, http.MethodPost, basePath, token.Value(), body, &created); err != nil {
		return CheckRun{}, err
	}
	if err := verifyCheckIdentity(created, 0, request); err != nil {
		return CheckRun{}, err
	}
	return created, nil
}

func (c *ChecksClient) updateCheckRun(ctx context.Context, token CheckToken, path string, checkRunID int64, request CheckRunRequest) (CheckRun, error) {
	var updated CheckRun
	body, err := json.Marshal(request)
	if err != nil {
		return CheckRun{}, errors.New("GitHub Check request encoding failed")
	}
	if err := c.do(ctx, http.MethodPatch, path, token.Value(), body, &updated); err != nil {
		return CheckRun{}, err
	}
	if err := verifyCheckIdentity(updated, checkRunID, request); err != nil {
		return CheckRun{}, err
	}
	return updated, nil
}

func (c *ChecksClient) do(ctx context.Context, method, path, token string, body []byte, dst any) error {
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(c.apiURL, "/")+path, bytes.NewReader(body))
	if err != nil {
		return errors.New("create GitHub Check API request failed")
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &CredentialError{Code: "checks_transport_failed", Retryable: true}
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		retryable := response.StatusCode == http.StatusTooManyRequests || response.StatusCode >= 500 ||
			response.StatusCode == http.StatusForbidden && (response.Header.Get("Retry-After") != "" || response.Header.Get("X-RateLimit-Remaining") == "0")
		err := &CredentialError{Code: "checks_github_rejected", Status: response.StatusCode, Retryable: retryable}
		if retryable {
			retryAfter := response.Header.Get("Retry-After")
			seconds, parseErr := strconv.Atoi(retryAfter)
			if parseErr == nil && seconds > 0 && seconds <= 86400 {
				err.RetryAt = c.now().Add(time.Duration(seconds) * time.Second)
			} else if retryAt, parseErr := http.ParseTime(retryAfter); parseErr == nil && retryAt.After(c.now()) && !retryAt.After(c.now().Add(24*time.Hour)) {
				err.RetryAt = retryAt
			}
		}
		return err
	}
	limited := io.LimitReader(response.Body, maxCheckResponseBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil || len(data) > maxCheckResponseBytes || json.Unmarshal(data, dst) != nil {
		return errors.New("invalid GitHub Check API response")
	}
	return nil
}

func validateCheckRequest(request CheckRunRequest) error {
	if request.Name == "" || len(request.Name) > 100 || strings.ContainsAny(request.Name, "\r\n\x00") ||
		request.ExternalID == "" || len(request.ExternalID) > 255 || strings.ContainsAny(request.ExternalID, "\r\n\x00") ||
		!checkCommitPattern.MatchString(request.HeadSHA) || request.StartedAt.IsZero() ||
		(request.Status != CheckQueued && request.Status != CheckInProgress && request.Status != CheckCompleted) ||
		request.Output.Title == "" || len(request.Output.Title) > 255 || len(request.Output.Summary) == 0 || len(request.Output.Summary) > maxCheckPayloadBytes ||
		len(request.Output.Title)+len(request.Output.Summary)+len(request.Output.Text) > maxCheckPayloadBytes ||
		strings.ContainsAny(request.Output.Title, "\r\n\x00") {
		return errors.New("invalid Check identity, status, timestamp, or output")
	}
	if request.Status == CheckCompleted {
		if request.CompletedAt == nil || request.CompletedAt.Before(request.StartedAt) || !validCheckConclusion(request.Conclusion) {
			return errors.New("completed Check requires a valid conclusion and completion time")
		}
	} else if request.Conclusion != "" || request.CompletedAt != nil {
		return errors.New("nonterminal Check cannot include a conclusion or completion time")
	}
	if len(request.Output.Annotations) > maxCheckAnnotations {
		return errors.New("too many Check annotations")
	}
	for _, annotation := range request.Output.Annotations {
		if err := validateCheckAnnotation(annotation); err != nil {
			return err
		}
	}
	encoded, err := json.Marshal(request)
	if err != nil || len(encoded) > maxCheckPayloadBytes {
		return errors.New("GitHub Check payload exceeds size limit")
	}
	return nil
}

func validateCheckAnnotation(annotation CheckAnnotation) error {
	path := strings.ReplaceAll(annotation.Path, "\\", "/")
	if path == "" || strings.HasPrefix(path, "/") || len(path) > 255 || strings.ContainsAny(path, "\r\n\x00") ||
		annotation.StartLine < 1 || annotation.EndLine < annotation.StartLine || annotation.StartColumn < 0 || annotation.EndColumn < 0 ||
		annotation.StartColumn > 65535 || annotation.EndColumn > 65535 ||
		(annotation.StartColumn > 0 && annotation.EndColumn > 0 && annotation.EndLine == annotation.StartLine && annotation.EndColumn < annotation.StartColumn) ||
		(annotation.AnnotationLevel != "notice" && annotation.AnnotationLevel != "warning" && annotation.AnnotationLevel != "failure") ||
		annotation.Message == "" || len(annotation.Message) > 10000 || len(annotation.Title) > 255 || len(annotation.RawDetails) > 10000 ||
		strings.ContainsAny(annotation.Title, "\r\n\x00") {
		return errors.New("invalid Check annotation")
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == ".." || segment == "." || segment == "" {
			return errors.New("Check annotation path must be repository-relative")
		}
	}
	return nil
}

func validCheckConclusion(conclusion CheckRunConclusion) bool {
	switch conclusion {
	case "success", "failure", "neutral", "cancelled", "timed_out", "action_required", "stale", "skipped":
		return true
	default:
		return false
	}
}

func verifyCheckIdentity(check CheckRun, expectedID int64, request CheckRunRequest) error {
	if check.ID <= 0 || expectedID > 0 && check.ID != expectedID || check.Name != request.Name ||
		check.ExternalID != request.ExternalID || check.HeadSHA != request.HeadSHA || check.Status != request.Status ||
		request.Status == CheckCompleted && check.Conclusion != request.Conclusion {
		return errors.New("GitHub Check response identity did not match the requested Workload Run")
	}
	return nil
}

func validCheckToken(token CheckToken, now time.Time) bool {
	return token != nil && token.Value() != "" && len(token.Value()) <= 16384 && !strings.ContainsAny(token.Value(), "\r\n\x00 ") && token.ExpiresAt().After(now.Add(30*time.Second))
}

func (r CheckRunRequest) String() string {
	return fmt.Sprintf("GitHub Check update %s/%s", r.Name, r.Status)
}
