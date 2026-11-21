// Package deploypolicy validates explicit GitHub OIDC deployment policy
// metadata. It does not issue cloud credentials or execute deployments.
package deploypolicy

import (
	"errors"
	"regexp"
	"strings"
)

const (
	MaxRefs         = 16
	MaxEnvironments = 8
	MaxWorkflows    = 8
	AWSSTSAudience  = "sts.amazonaws.com"
)

var (
	ErrInvalid      = errors.New("invalid OIDC deploy policy")
	refPattern      = regexp.MustCompile(`^refs/(heads|tags)/[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
	envPattern      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	workflowPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+/\.github/workflows/[A-Za-z0-9_.-]+\.ya?ml@refs/(heads|tags)/[A-Za-z0-9][A-Za-z0-9._/-]{0,200}$`)
)

// Policy is a declarative allowlist for a future trusted release/deploy
// workload. RequireHumanApproval is mandatory when enabled; actual execution
// remains disabled until a trusted approval and AWS identity broker exist.
type Policy struct {
	Enabled              bool     `json:"enabled"`
	Audience             string   `json:"audience,omitempty"`
	AllowedRefs          []string `json:"allowed_refs,omitempty"`
	Environments         []string `json:"environments,omitempty"`
	AllowedWorkflowRefs  []string `json:"allowed_workflow_refs,omitempty"`
	RequireHumanApproval bool     `json:"require_human_approval"`
}

func (p Policy) Validate() error {
	if len(p.AllowedRefs) > MaxRefs || len(p.Environments) > MaxEnvironments || len(p.AllowedWorkflowRefs) > MaxWorkflows {
		return ErrInvalid
	}
	if !p.Enabled {
		if p.Audience != "" || len(p.AllowedRefs) != 0 || len(p.Environments) != 0 || len(p.AllowedWorkflowRefs) != 0 || p.RequireHumanApproval {
			return ErrInvalid
		}
		return nil
	}
	if p.Audience != AWSSTSAudience || len(p.AllowedRefs) == 0 || len(p.Environments) == 0 || len(p.AllowedWorkflowRefs) == 0 || !p.RequireHumanApproval {
		return ErrInvalid
	}
	for i, ref := range p.AllowedRefs {
		if !validRef(ref) || (i > 0 && p.AllowedRefs[i-1] >= ref) {
			return ErrInvalid
		}
	}
	for i, environment := range p.Environments {
		if !envPattern.MatchString(environment) || strings.Contains(environment, "..") || (i > 0 && p.Environments[i-1] >= environment) {
			return ErrInvalid
		}
	}
	for i, workflow := range p.AllowedWorkflowRefs {
		if !workflowPattern.MatchString(workflow) || strings.Contains(workflow, "..") || strings.Contains(workflow, "//") ||
			(i > 0 && p.AllowedWorkflowRefs[i-1] >= workflow) {
			return ErrInvalid
		}
	}
	return nil
}

func validRef(ref string) bool {
	if !refPattern.MatchString(ref) || strings.Contains(ref, "..") || strings.Contains(ref, "//") ||
		strings.Contains(ref, "@{") || strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock") {
		return false
	}
	for _, component := range strings.Split(strings.TrimPrefix(strings.TrimPrefix(ref, "refs/heads/"), "refs/tags/"), "/") {
		if component == "" || strings.HasPrefix(component, ".") {
			return false
		}
	}
	return true
}
