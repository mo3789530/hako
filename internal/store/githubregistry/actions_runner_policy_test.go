package githubregistry

import "testing"

func TestValidActionsRunnerWorkflowPolicy(t *testing.T) {
	valid := ActionsRunnerWorkflowPolicy{WorkflowPath: ".github/workflows/claude.yml", AllowedRefs: []string{"refs/heads/main"},
		AllowedEvents: []string{"push", "workflow_dispatch"}, RequiredLabels: []string{"self-hosted", "hako"},
		AllowedLabels: []string{"self-hosted", "hako", "arm64"}}
	if !validActionsRunnerWorkflowPolicy(valid) {
		t.Fatal("valid Tenant workflow policy rejected")
	}
	tests := map[string]func(*ActionsRunnerWorkflowPolicy){
		"unsafe path":                func(p *ActionsRunnerWorkflowPolicy) { p.WorkflowPath = "../../run.yml" },
		"pull request target":        func(p *ActionsRunnerWorkflowPolicy) { p.AllowedEvents = []string{"pull_request_target"} },
		"mutable ref":                func(p *ActionsRunnerWorkflowPolicy) { p.AllowedRefs = []string{"refs/heads/../main"} },
		"duplicate ref":              func(p *ActionsRunnerWorkflowPolicy) { p.AllowedRefs = []string{"refs/heads/main", "refs/heads/main"} },
		"required label not allowed": func(p *ActionsRunnerWorkflowPolicy) { p.AllowedLabels = []string{"self-hosted"} },
		"empty labels":               func(p *ActionsRunnerWorkflowPolicy) { p.AllowedLabels = nil },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			candidate.AllowedRefs = append([]string(nil), valid.AllowedRefs...)
			candidate.AllowedEvents = append([]string(nil), valid.AllowedEvents...)
			candidate.RequiredLabels = append([]string(nil), valid.RequiredLabels...)
			candidate.AllowedLabels = append([]string(nil), valid.AllowedLabels...)
			mutate(&candidate)
			if validActionsRunnerWorkflowPolicy(candidate) {
				t.Fatal("unsafe Tenant workflow policy accepted")
			}
		})
	}
}
