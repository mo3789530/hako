package actionsrunner

import (
	"errors"
	"regexp"
	"slices"
	"strings"

	"github.com/mo3789530/hako/internal/domain"
)

// RunnerWorkflowPolicy is the Hako-owned allowlist for one repository. It is
// deliberately data-only: it cannot supply commands, secrets, images, roles,
// or network profiles to a runner.
type RunnerWorkflowPolicy struct {
	TenantID       domain.TenantID
	IntegrationID  string
	InstallationID int64
	RepositoryID   int64
	Owner          string
	Repository     string
	WorkflowPath   string
	AllowedRefs    []string
	AllowedEvents  []string
	RequiredLabels []string
}

// AuthoritativeWorkflowRun must be populated from GitHub's Actions Run and
// Job APIs using a short-lived Installation token, not inferred from comments
// or accepted solely from webhook fields.
type AuthoritativeWorkflowRun struct {
	TenantID       domain.TenantID
	IntegrationID  string
	InstallationID int64
	RepositoryID   int64
	Owner          string
	Repository     string
	WorkflowPath   string
	WorkflowRef    string
	HeadSHA        string
	RunID          int64
	RunAttempt     int64
	JobID          int64
	Event          string
	HeadRepository int64
	Fork           bool
	Labels         []string
}

// QueuedWorkflowJob is the signed webhook identity Hako persisted before the
// authoritative API lookup. Every job/run coordinate must match exactly.
type QueuedWorkflowJob struct {
	RunID       int64
	RunAttempt  int64
	JobID       int64
	WorkflowRef string
	HeadSHA     string
}

var (
	workflowPathPattern = regexp.MustCompile(`^\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml$`)
	workflowSHAPattern  = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
)

// VerifyRunnerWorkflowProvenance rejects a queued job unless every mutable
// identity dimension matches both the Tenant policy and GitHub's authoritative
// run/job metadata. Forks are denied unconditionally.
func VerifyRunnerWorkflowProvenance(policy RunnerWorkflowPolicy, queued QueuedWorkflowJob, run AuthoritativeWorkflowRun) error {
	if policy.TenantID == "" || policy.IntegrationID == "" || policy.InstallationID <= 0 || policy.RepositoryID <= 0 ||
		!validRepositoryPart(policy.Owner) || !validRepositoryPart(policy.Repository) || !workflowPathPattern.MatchString(policy.WorkflowPath) ||
		len(policy.AllowedRefs) == 0 || len(policy.AllowedEvents) == 0 || len(policy.RequiredLabels) == 0 ||
		run.TenantID != policy.TenantID || run.IntegrationID != policy.IntegrationID || run.InstallationID != policy.InstallationID ||
		run.RepositoryID != policy.RepositoryID || run.Owner != policy.Owner || run.Repository != policy.Repository ||
		run.WorkflowPath != policy.WorkflowPath || !validWorkflowRef(run.WorkflowRef) || !containsExact(policy.AllowedRefs, run.WorkflowRef) ||
		!workflowSHAPattern.MatchString(run.HeadSHA) || queued.RunID <= 0 || queued.RunAttempt <= 0 || queued.JobID <= 0 ||
		queued.RunID != run.RunID || queued.RunAttempt != run.RunAttempt || queued.JobID != run.JobID ||
		queued.WorkflowRef != run.WorkflowRef || queued.HeadSHA != run.HeadSHA || run.Fork ||
		run.HeadRepository != policy.RepositoryID || !containsExact(policy.AllowedEvents, run.Event) ||
		!containsAll(run.Labels, policy.RequiredLabels) {
		return errors.New("GitHub Actions workflow provenance denied")
	}
	return nil
}

func validWorkflowRef(ref string) bool {
	return ref != "" && len(ref) <= 255 && !strings.ContainsAny(ref, "\x00\r\n") && !strings.HasPrefix(ref, "-")
}

func containsExact(values []string, value string) bool {
	return value != "" && slices.Contains(values, value)
}

func containsAll(values, required []string) bool {
	for _, label := range required {
		if !slices.Contains(values, label) {
			return false
		}
	}
	return true
}
