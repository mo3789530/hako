package agenttask

import (
	"errors"
	"testing"
)

func TestAgentWritePolicyRequiresNarrowBranchAndPassingChecks(t *testing.T) {
	policy := WritePolicy{Enabled: true, BranchPrefixes: []string{"hako/agent/"}, AllowPullRequests: true,
		RequiredChecks: []string{"hako/lint", "hako/test"}, RequireChecksToPass: true}
	if err := policy.Validate(); err != nil {
		t.Fatal(err)
	}
	passed := map[string]bool{"hako/lint": true, "hako/test": true}
	for _, test := range []struct {
		name        string
		branch      string
		defaultName string
		isNew       bool
		checks      map[string]bool
		wantErr     bool
		policy      *WritePolicy
	}{
		{name: "authorized new branch", branch: "hako/agent/issue-17", defaultName: "main", isNew: true, checks: passed},
		{name: "disabled policy", branch: "hako/agent/issue-17", defaultName: "main", isNew: true, checks: passed, wantErr: true, policy: &WritePolicy{}},
		{name: "existing branch update", branch: "hako/agent/issue-17", defaultName: "main", checks: passed, wantErr: true},
		{name: "default branch", branch: "main", defaultName: "main", isNew: true, checks: passed, wantErr: true},
		{name: "outside prefix", branch: "feature/issue-17", defaultName: "main", isNew: true, checks: passed, wantErr: true},
		{name: "traversal branch", branch: "hako/agent/../main", defaultName: "main", isNew: true, checks: passed, wantErr: true},
		{name: "required Check missing", branch: "hako/agent/issue-17", defaultName: "main", isNew: true, checks: map[string]bool{"hako/lint": true}, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			selected := policy
			if test.policy != nil {
				selected = *test.policy
			}
			err := AuthorizeBranchPush(selected, test.branch, test.defaultName, test.isNew, test.checks)
			if errors.Is(err, ErrWritePolicy) != test.wantErr {
				t.Fatalf("AuthorizeBranchPush() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestAgentWritePolicyRestrictsPullRequests(t *testing.T) {
	policy := WritePolicy{Enabled: true, BranchPrefixes: []string{"hako/agent/"}, AllowPullRequests: true,
		RequiredChecks: []string{"hako/test"}, RequireChecksToPass: true}
	checks := map[string]bool{"hako/test": true}
	for _, test := range []struct {
		name       string
		head       string
		base       string
		defaultRef string
		sameRepo   bool
		wantErr    bool
	}{
		{name: "authorized PR", head: "hako/agent/issue-17", base: "main", defaultRef: "main", sameRepo: true},
		{name: "non-default base", head: "hako/agent/issue-17", base: "release", defaultRef: "main", sameRepo: true, wantErr: true},
		{name: "fork head", head: "hako/agent/issue-17", base: "main", defaultRef: "main", wantErr: true},
		{name: "unapproved head", head: "feature/issue-17", base: "main", defaultRef: "main", sameRepo: true, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := AuthorizePullRequest(policy, test.head, test.base, test.defaultRef, test.sameRepo, checks)
			if errors.Is(err, ErrWritePolicy) != test.wantErr {
				t.Fatalf("AuthorizePullRequest() error=%v wantErr=%v", err, test.wantErr)
			}
		})
	}
}

func TestAgentWritePolicyRejectsBroadAndUnsortedConfiguration(t *testing.T) {
	for _, policy := range []WritePolicy{
		{Enabled: true, BranchPrefixes: []string{"*"}, RequiredChecks: []string{"hako/test"}, RequireChecksToPass: true},
		{Enabled: true, BranchPrefixes: []string{"hako/agent/"}, RequiredChecks: []string{"hako/test", "hako/lint"}, RequireChecksToPass: true},
		{Enabled: false, BranchPrefixes: []string{"hako/agent/"}},
	} {
		if err := policy.Validate(); !errors.Is(err, ErrWritePolicy) {
			t.Errorf("Validate(%+v) = %v, want ErrWritePolicy", policy, err)
		}
	}
}
