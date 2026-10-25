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
		if !branchPrefixPattern.MatchString(prefix) || !validBranchComponents(strings.TrimSuffix(prefix, "/")) {
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
	if policy.Validate() != nil || !policy.Enabled || !isNewBranch || !validBranchName(branch) || branch == defaultBranch ||
		!branchAllowed(policy, branch) || !allRequiredChecksPassed(policy, checks) {
		return ErrWritePolicy
	}
	return nil
}

// AuthorizePullRequest allows a PR only from an authorized same-repository
// Agent branch into the current default branch, after required Checks pass.
func AuthorizePullRequest(policy WritePolicy, headBranch, baseBranch, defaultBranch string, sameRepository bool, checks map[string]bool) error {
	if policy.Validate() != nil || !policy.Enabled || !policy.AllowPullRequests || !sameRepository ||
		!validBranchName(defaultBranch) || baseBranch != defaultBranch || !validBranchName(headBranch) ||
		!branchAllowed(policy, headBranch) || !allRequiredChecksPassed(policy, checks) {
		return ErrWritePolicy
	}
	return nil
}

func validBranchName(branch string) bool {
	return branchNamePattern.MatchString(branch) && validBranchComponents(branch)
}

// Git refs cannot contain empty components, dot-prefixed components, or
// components ending in .lock. Validate each component because a full branch
// name can be well-formed while an interior component violates those rules.
func validBranchComponents(branch string) bool {
	if branch == "" || strings.Contains(branch, "..") || strings.Contains(branch, "//") ||
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
