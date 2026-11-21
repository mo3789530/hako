package actionsrunner

import (
	"strings"
	"testing"

	"github.com/mo3789530/hako/internal/domain"
)

func TestVerifyRunnerWorkflowProvenance(t *testing.T) {
	policy := RunnerWorkflowPolicy{TenantID: domain.TenantID("tenant_acme"), IntegrationID: "integration_acme", InstallationID: 101,
		RepositoryID: 202, Owner: "acme", Repository: "api", WorkflowPath: ".github/workflows/claude.yml",
		AllowedRefs: []string{"refs/heads/main"}, AllowedEvents: []string{"push"}, RequiredLabels: []string{"self-hosted", "hako"},
		AllowedLabels: []string{"self-hosted", "hako", "arm64"}}
	valid := AuthoritativeWorkflowRun{TenantID: policy.TenantID, IntegrationID: policy.IntegrationID, InstallationID: policy.InstallationID,
		RepositoryID: policy.RepositoryID, Owner: policy.Owner, Repository: policy.Repository, WorkflowPath: policy.WorkflowPath,
		WorkflowRef: "refs/heads/main", HeadSHA: strings.Repeat("a", 40), RunID: 303, RunAttempt: 1, JobID: 404,
		Event: "push", HeadRepository: policy.RepositoryID, Labels: []string{"self-hosted", "hako", "arm64"}}
	queued := QueuedWorkflowJob{RunID: valid.RunID, RunAttempt: valid.RunAttempt, JobID: valid.JobID, WorkflowRef: valid.WorkflowRef, HeadSHA: valid.HeadSHA}
	if err := VerifyRunnerWorkflowProvenance(policy, queued, valid); err != nil {
		t.Fatalf("valid provenance rejected: %v", err)
	}
	tests := map[string]func(*AuthoritativeWorkflowRun){
		"wrong tenant":            func(run *AuthoritativeWorkflowRun) { run.TenantID = "tenant_other" },
		"wrong integration":       func(run *AuthoritativeWorkflowRun) { run.IntegrationID = "integration_other" },
		"wrong installation":      func(run *AuthoritativeWorkflowRun) { run.InstallationID++ },
		"wrong repository":        func(run *AuthoritativeWorkflowRun) { run.RepositoryID++ },
		"wrong owner":             func(run *AuthoritativeWorkflowRun) { run.Owner = "other" },
		"wrong workflow":          func(run *AuthoritativeWorkflowRun) { run.WorkflowPath = ".github/workflows/ci.yml" },
		"unapproved ref":          func(run *AuthoritativeWorkflowRun) { run.WorkflowRef = "refs/heads/attacker" },
		"wrong sha":               func(run *AuthoritativeWorkflowRun) { run.HeadSHA = "not-a-commit" },
		"unapproved event":        func(run *AuthoritativeWorkflowRun) { run.Event = "pull_request_target" },
		"fork":                    func(run *AuthoritativeWorkflowRun) { run.Fork = true },
		"foreign head repository": func(run *AuthoritativeWorkflowRun) { run.HeadRepository++ },
		"missing label":           func(run *AuthoritativeWorkflowRun) { run.Labels = []string{"self-hosted"} },
		"unexpected label":        func(run *AuthoritativeWorkflowRun) { run.Labels = append(run.Labels, "privileged") },
		"duplicate label":         func(run *AuthoritativeWorkflowRun) { run.Labels = append(run.Labels, "hako") },
		"unsafe event":            func(run *AuthoritativeWorkflowRun) { run.Event = "pull_request_target" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.Labels = append([]string(nil), valid.Labels...)
			mutate(&candidate)
			if err := VerifyRunnerWorkflowProvenance(policy, queued, candidate); err == nil {
				t.Fatal("mismatched provenance accepted")
			}
		})
	}
	invalidPolicies := []RunnerWorkflowPolicy{
		{TenantID: policy.TenantID, IntegrationID: policy.IntegrationID, InstallationID: policy.InstallationID, RepositoryID: policy.RepositoryID,
			Owner: policy.Owner, Repository: policy.Repository, WorkflowPath: "../../evil.yml", AllowedRefs: policy.AllowedRefs,
			AllowedEvents: policy.AllowedEvents, RequiredLabels: policy.RequiredLabels, AllowedLabels: policy.AllowedLabels},
		{TenantID: policy.TenantID, IntegrationID: policy.IntegrationID, InstallationID: policy.InstallationID, RepositoryID: policy.RepositoryID,
			Owner: policy.Owner, Repository: policy.Repository, WorkflowPath: policy.WorkflowPath},
	}
	for _, invalid := range invalidPolicies {
		if err := VerifyRunnerWorkflowProvenance(invalid, queued, valid); err == nil {
			t.Fatal("incomplete policy accepted")
		}
	}
	for name, mutate := range map[string]func(*QueuedWorkflowJob){
		"wrong job":     func(job *QueuedWorkflowJob) { job.JobID++ },
		"wrong run":     func(job *QueuedWorkflowJob) { job.RunID++ },
		"wrong attempt": func(job *QueuedWorkflowJob) { job.RunAttempt++ },
		"wrong ref":     func(job *QueuedWorkflowJob) { job.WorkflowRef = "refs/heads/other" },
		"wrong sha":     func(job *QueuedWorkflowJob) { job.HeadSHA = strings.Repeat("b", 40) },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := queued
			mutate(&candidate)
			if err := VerifyRunnerWorkflowProvenance(policy, candidate, valid); err == nil {
				t.Fatal("queued identity mismatch accepted")
			}
		})
	}
}
