package deploypolicy

import (
	"errors"
	"testing"
)

func TestValidateRequiresExactAudienceRefsEnvironmentAndHumanApproval(t *testing.T) {
	valid := Policy{Enabled: true, Audience: AWSSTSAudience, AllowedRefs: []string{"refs/heads/main", "refs/tags/v1.0.0"},
		Environments: []string{"production"}, AllowedWorkflowRefs: []string{"acme/api/.github/workflows/release.yml@refs/heads/main"}, RequireHumanApproval: true}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Policy){
		"wrong audience":            func(p *Policy) { p.Audience = "https://evil.example" },
		"no approval":               func(p *Policy) { p.RequireHumanApproval = false },
		"no environment":            func(p *Policy) { p.Environments = nil },
		"no workflow":               func(p *Policy) { p.AllowedWorkflowRefs = nil },
		"wildcard workflow":         func(p *Policy) { p.AllowedWorkflowRefs = []string{"acme/api/.github/workflows/*@refs/heads/main"} },
		"pull request ref":          func(p *Policy) { p.AllowedRefs = []string{"refs/pull/1/merge"} },
		"wildcard ref":              func(p *Policy) { p.AllowedRefs = []string{"refs/heads/*"} },
		"path traversal ref":        func(p *Policy) { p.AllowedRefs = []string{"refs/heads/../main"} },
		"hidden component":          func(p *Policy) { p.AllowedRefs = []string{"refs/heads/.github/workflows/release"} },
		"unsorted refs":             func(p *Policy) { p.AllowedRefs = []string{"refs/tags/v1.0.0", "refs/heads/main"} },
		"duplicate environments":    func(p *Policy) { p.Environments = []string{"production", "production"} },
		"disabled with permissions": func(p *Policy) { p.Enabled = false },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.AllowedRefs = append([]string(nil), valid.AllowedRefs...)
			candidate.Environments = append([]string(nil), valid.Environments...)
			candidate.AllowedWorkflowRefs = append([]string(nil), valid.AllowedWorkflowRefs...)
			mutate(&candidate)
			if err := candidate.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Validate() error = %v, want ErrInvalid", err)
			}
		})
	}
}

func TestDisabledPolicyMustBeEmpty(t *testing.T) {
	if err := (Policy{}).Validate(); err != nil {
		t.Fatalf("empty disabled policy rejected: %v", err)
	}
}
