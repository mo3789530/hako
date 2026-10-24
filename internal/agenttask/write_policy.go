package agenttask

import (
	"errors"
	"regexp"
	"strings"
)

const (
	MaxWriteBranchPrefixes = 16
	MaxRequiredChecks      = 16
)

var (
	branchPrefixPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,126}/$`)
	branchNamePattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,254}$`)
	checkNamePattern    = regexp.MustCompile(`^hako/[a-z][a-z0-9_/-]{0,78}$`)
	ErrWritePolicy      = errors.New("Agent repository write policy denied the operation")
)

// WritePolicy is an explicit upper bound for future Agent GitHub writes. It
// contains no token, permission grant, merge, deployment, or arbitrary API
// capability. Agent execution remains separately gated by the runtime.
type WritePolicy struct {
	Enabled             bool     `json:"enabled"`
	BranchPrefixes      []string `json:"branch_prefixes,omitempty"`
	AllowPullRequests   bool     `json:"allow_pull_requests"`
	RequiredChecks      []string `json:"required_checks,omitempty"`
	RequireChecksToPass bool     `json:"require_checks_to_pass"`
}

func (p WritePolicy) Validate() error {
	if len(p.BranchPrefixes) > MaxWriteBranchPrefixes || len(p.RequiredChecks) > MaxRequiredChecks {
		return ErrWritePolicy
	}
	if !p.Enabled {
		if p.AllowPullRequests || p.RequireChecksToPass || len(p.BranchPrefixes) != 0 || len(p.RequiredChecks) != 0 {
			return ErrWritePolicy
		}
		return nil
	}
	if len(p.BranchPrefixes) == 0 || !p.RequireChecksToPass || len(p.RequiredChecks) == 0 {
		return ErrWritePolicy
	}
	for i, prefix := range p.BranchPrefixes {
		if !branchPrefixPattern.MatchString(prefix) || strings.Contains(prefix, "..") || strings.Contains(prefix, "//") || strings.HasSuffix(prefix, ".") {
			return ErrWritePolicy
		}
		if i > 0 && p.BranchPrefixes[i-1] >= prefix {
			return ErrWritePolicy
		}
	}
	for i, check := range p.RequiredChecks {
		if !checkNamePattern.MatchString(check) || i > 0 && p.RequiredChecks[i-1] >= check {
			return ErrWritePolicy
		}
	}
	return nil
}

// AuthorizeBranchPush permits only a new, Hako-prefixed branch after every
// configured required Check passed. The caller must separately obtain an
// installation token scoped to the exact Tenant-bound Repository.
func AuthorizeBranchPush(policy WritePolicy, branch, defaultBranch string, isNewBranch bool, checks map[string]bool) error {
	if policy.Validate() != nil || !policy.Enabled || !isNewBranch || !branchNamePattern.MatchString(branch) || branch == defaultBranch ||
		strings.Contains(branch, "..") || strings.Contains(branch, "//") || strings.HasSuffix(branch, "/") || strings.HasSuffix(branch, ".") ||
		!branchAllowed(policy, branch) || !allRequiredChecksPassed(policy, checks) {
		return ErrWritePolicy
	}
	return nil
}

// AuthorizePullRequest allows a PR only from an authorized same-repository
// Agent branch into the current default branch, after required Checks pass.
func AuthorizePullRequest(policy WritePolicy, headBranch, baseBranch, defaultBranch string, sameRepository bool, checks map[string]bool) error {
	if policy.Validate() != nil || !policy.Enabled || !policy.AllowPullRequests || !sameRepository ||
		baseBranch == "" || baseBranch != defaultBranch || !branchNamePattern.MatchString(headBranch) ||
		strings.Contains(headBranch, "..") || strings.Contains(headBranch, "//") || strings.HasSuffix(headBranch, "/") ||
		strings.HasSuffix(headBranch, ".") || !branchAllowed(policy, headBranch) || !allRequiredChecksPassed(policy, checks) {
		return ErrWritePolicy
	}
	return nil
}

func branchAllowed(policy WritePolicy, branch string) bool {
	for _, prefix := range policy.BranchPrefixes {
		if strings.HasPrefix(branch, prefix) {
			return true
		}
	}
	return false
}

func allRequiredChecksPassed(policy WritePolicy, checks map[string]bool) bool {
	if !policy.RequireChecksToPass || len(policy.RequiredChecks) == 0 {
		return false
	}
	for _, name := range policy.RequiredChecks {
		if !checks[name] {
			return false
		}
	}
	return true
}
