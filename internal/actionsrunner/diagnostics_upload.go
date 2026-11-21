package actionsrunner

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const runnerDiagnosticsPath = "/v1/internal/actions-runner/integrations/"

var runnerDiagnosticsURLPattern = regexp.MustCompile(`^/v1/internal/actions-runner/integrations/[A-Za-z0-9_-]{3,128}/jobs/[1-9][0-9]{0,18}/attempts/[1-9][0-9]{0,8}/diagnostics$`)
var runnerDiagnosticsTokenPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type RunnerDiagnosticsUploader interface {
	Upload(context.Context, string, string, []byte, string) error
}

type HTTPRunnerDiagnosticsUploader struct {
	client *http.Client
}

func NewHTTPRunnerDiagnosticsUploader(client *http.Client) (*HTTPRunnerDiagnosticsUploader, error) {
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	copyClient := *client
	copyClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &HTTPRunnerDiagnosticsUploader{client: &copyClient}, nil
}

func (u *HTTPRunnerDiagnosticsUploader) Upload(ctx context.Context, endpoint, token string, logs []byte, digest string) error {
	if u == nil || u.client == nil || ctx == nil || !validRunnerDiagnosticsURL(endpoint) || !runnerDiagnosticsTokenPattern.MatchString(token) ||
		len(logs) == 0 || len(logs) > maxRunnerDiagnosticBytes || !runnerDiagnosticsTokenPattern.MatchString(digest) {
		return ErrRunnerBootstrap
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(logs))
	if err != nil {
		return ErrRunnerBootstrap
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/octet-stream")
	request.Header.Set("X-Hako-SHA256", digest)
	request.Header.Set("Cache-Control", "no-store")
	response, err := u.client.Do(request)
	if err != nil {
		return ErrRunnerBootstrap
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusNoContent || response.ContentLength > 0 || response.Header.Get("Cache-Control") != "no-store" {
		return ErrRunnerBootstrap
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 1))
	if err != nil || len(body) != 0 {
		return ErrRunnerBootstrap
	}
	return nil
}

func validRunnerDiagnosticsClaims(endpoint, token, integrationID string, workflowJobID, runAttempt int64) bool {
	if endpoint == "" && token == "" {
		return true
	}
	if !validRunnerDiagnosticsURL(endpoint) || !runnerDiagnosticsTokenPattern.MatchString(token) {
		return false
	}
	expectedSuffix := "/integrations/" + integrationID + "/jobs/" + strconv.FormatInt(workflowJobID, 10) + "/attempts/" +
		strconv.FormatInt(runAttempt, 10) + "/diagnostics"
	parsed, err := url.Parse(endpoint)
	return err == nil && strings.HasSuffix(parsed.Path, expectedSuffix)
}

func validRunnerDiagnosticsBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" &&
		!parsed.ForceQuery && parsed.Fragment == "" && (parsed.Path == "" || parsed.Path == "/") && parsed.RawPath == ""
}

func ValidRunnerDiagnosticsBaseURL(value string) bool {
	return validRunnerDiagnosticsBaseURL(value)
}

func runnerDiagnosticsUploadURL(base, integrationID string, workflowJobID, runAttempt int64) string {
	if !validRunnerDiagnosticsBaseURL(base) || !regexp.MustCompile(`^[A-Za-z0-9_-]{3,128}$`).MatchString(integrationID) || workflowJobID <= 0 || runAttempt <= 0 {
		return ""
	}
	return strings.TrimRight(base, "/") + runnerDiagnosticsPath + integrationID + "/jobs/" + strconv.FormatInt(workflowJobID, 10) +
		"/attempts/" + strconv.FormatInt(runAttempt, 10) + "/diagnostics"
}

func validRunnerDiagnosticsURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Hostname() != "" && parsed.User == nil && parsed.RawQuery == "" &&
		!parsed.ForceQuery && parsed.Fragment == "" && parsed.RawPath == "" && runnerDiagnosticsURLPattern.MatchString(parsed.Path)
}

var _ RunnerDiagnosticsUploader = (*HTTPRunnerDiagnosticsUploader)(nil)
