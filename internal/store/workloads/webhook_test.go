package workloads

import (
	"testing"

	"github.com/mo3789530/hako/internal/githubwebhook"
)

func TestWebhookPipelineTriggerPinsTheSelectionBranch(t *testing.T) {
	tests := []struct {
		name   string
		event  githubwebhook.RepositoryEvent
		kind   string
		branch string
	}{
		{name: "push strips refs heads prefix", event: githubwebhook.RepositoryEvent{GitHubEvent: "push", Type: "repository.push", Ref: "refs/heads/release/2026.10"}, kind: "push", branch: "release/2026.10"},
		{name: "pull request selects base branch", event: githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: "synchronize", Type: "repository.pull_request.synchronize", PullRequest: &githubwebhook.PullRequestRef{State: "open", HeadRef: "feature/a", BaseRef: "main"}}, kind: "pull_request", branch: "main"},
		{name: "deleted push has no trigger", event: githubwebhook.RepositoryEvent{GitHubEvent: "push", Type: "repository.push", Ref: "refs/heads/main", Deleted: true}},
		{name: "closed pull request has no trigger", event: githubwebhook.RepositoryEvent{GitHubEvent: "pull_request", Action: "closed", Type: "repository.pull_request.closed", PullRequest: &githubwebhook.PullRequestRef{State: "closed", BaseRef: "main"}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			kind, branch := webhookPipelineTrigger(test.event)
			if kind != test.kind || branch != test.branch {
				t.Fatalf("webhookPipelineTrigger() = %q, %q; want %q, %q", kind, branch, test.kind, test.branch)
			}
		})
	}
}

func TestValidTriggerBranchRejectsGitRefControlCharacters(t *testing.T) {
	for _, branch := range []string{"", "main", "feature/add-api", "release/2026.10"} {
		if !validTriggerBranch(branch) {
			t.Errorf("validTriggerBranch(%q) = false", branch)
		}
	}
	for _, branch := range []string{"../main", "main//next", "main/", "main.", "main@{1}", "main;touch", "main\nnext"} {
		if validTriggerBranch(branch) {
			t.Errorf("validTriggerBranch(%q) = true", branch)
		}
	}
}
