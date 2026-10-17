package factoryconfig

import (
	"errors"
	"strings"
	"testing"

	webhookevent "github.com/mo3789530/hako/internal/githubwebhook"
)

func TestParseValidManifestNormalizesBoundedDefaults(t *testing.T) {
	config, err := Parse([]byte(`version: 1
on:
  push:
    branches: [main]
pipelines:
  verify:
    steps: [checkout, go-test, go-vet]
    services: [postgres, redis]
    network: registry-only
`))
	if err != nil {
		t.Fatal(err)
	}
	pipeline := config.Pipelines["verify"]
	if config.Version != 1 || pipeline.RuntimeClass != "standard" || pipeline.TimeoutSecond != 1800 ||
		pipeline.MemoryMB != 2048 || pipeline.CPUMillis != 1000 || pipeline.Repository != "read" || pipeline.Network != "registry-only" {
		t.Fatalf("normalized manifest = %+v", config)
	}
}

func TestParseRejectsUnsafeOrAmbiguousManifest(t *testing.T) {
	base := manifest("    steps: [checkout]\n")
	cases := []struct {
		name string
		yaml string
	}{
		{name: "unsupported version", yaml: strings.Replace(base, "version: 1", "version: 2", 1)},
		{name: "unknown field", yaml: manifest("    steps: [checkout]\n    run: id\n")},
		{name: "arbitrary shell", yaml: manifest("    steps: [checkout, 'go-test; touch /tmp/pwn']\n")},
		{name: "unsupported image-like step", yaml: manifest("    steps: [checkout, 'docker:latest']\n")},
		{name: "missing trigger", yaml: strings.Replace(manifest("    steps: [checkout]\n"), "on:\n  push:\n    branches: [main]\n", "", 1)},
		{name: "unsupported trigger", yaml: strings.Replace(manifest("    steps: [checkout]\n"), "  push:\n", "  issue:\n", 1)},
		{name: "unsafe branch glob", yaml: strings.Replace(manifest("    steps: [checkout]\n"), "branches: [main]", "branches: ['ma*in']", 1)},
		{name: "duplicate branch filter", yaml: strings.Replace(manifest("    steps: [checkout]\n"), "branches: [main]", "branches: [main, main]", 1)},
		{name: "write permission", yaml: manifest("    steps: [checkout]\n    repository: write\n")},
		{name: "cloud role", yaml: manifest("    steps: [checkout]\n    cloud_access: production\n")},
		{name: "public egress", yaml: manifest("    steps: [checkout]\n    network: internet\n")},
		{name: "untrusted service image", yaml: manifest("    steps: [checkout]\n    services: [custom-image]\n")},
		{name: "checkout is not first", yaml: manifest("    steps: [go-test, checkout]\n")},
		{name: "duplicate key", yaml: "version: 1\non: {push: {}}\npipelines:\n  verify:\n    steps: [checkout]\n    steps: [go-test]\n"},
		{name: "anchor", yaml: "version: &version 1\npipelines: {verify: {steps: [checkout]}}"},
		{name: "alias", yaml: "steps: &steps [checkout]\nversion: 1\npipelines: {verify: {steps: *steps}}"},
		{name: "multiple documents", yaml: "version: 1\npipelines: {verify: {steps: [checkout]}}\n---\nversion: 1\npipelines: {other: {steps: [checkout]}}"},
		{name: "unknown version shape", yaml: strings.Replace(base, "version: 1", `version: "1"`, 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Parse([]byte(tc.yaml)); !errors.Is(err, ErrManifest) {
				t.Fatalf("Parse() error = %v, want ErrManifest", err)
			}
		})
	}
}

func TestParseRejectsManifestAndResourceBounds(t *testing.T) {
	if _, err := Parse(nil); !errors.Is(err, ErrManifest) {
		t.Fatalf("Parse(empty) error = %v", err)
	}
	if _, err := Parse([]byte(strings.Repeat("x", MaxManifestBytes+1))); !errors.Is(err, ErrManifest) {
		t.Fatalf("Parse(oversized) error = %v", err)
	}
	for _, yaml := range []string{
		manifest("    steps: [checkout]\n    timeout_seconds: 3601\n"),
		manifest("    steps: [checkout]\n    memory_mb: 9000\n"),
		manifest("    steps: [checkout]\n    cpu_millis: 5000\n"),
		manifest("    steps: [checkout]\n    runtime_class: large\n"),
		manifest("    steps: [checkout, checkout]\n"),
		strings.Replace(manifest("    steps: [checkout]\n"), "verify:", "Verify:", 1),
		strings.Replace(manifest("    steps: [checkout]\n"), "branches: [main]", "branches: [main, release/*, hotfix/*, develop, test, one, two, three, four, five, six, seven, eight, nine, ten, eleven, twelve, thirteen, fourteen, fifteen, sixteen, seventeen, eighteen, nineteen, twenty, twentyone, twentytwo, twentythree, twentyfour, twentyfive, twentysix, twentyseven, twentyeight, twentynine, thirty, thirtyone, thirtytwo, thirtythree]", 1),
	} {
		if _, err := Parse([]byte(yaml)); !errors.Is(err, ErrManifest) {
			t.Errorf("Parse(%q) error = %v, want ErrManifest", yaml, err)
		}
	}
}

func manifest(pipelineFields string) string {
	return "version: 1\non:\n  push:\n    branches: [main]\npipelines:\n  verify:\n" + pipelineFields
}

func TestParseRejectsExcessiveYAMLDepth(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("version: 1\non:\n  push:\n    branches: [main]\npipelines:\n  verify:\n    steps: [checkout]\n    extra:")
	for i := 0; i < MaxYAMLDepth+2; i++ {
		builder.WriteString("\n      nested:")
	}
	builder.WriteString(" true\n")
	if _, err := Parse([]byte(builder.String())); !errors.Is(err, ErrManifest) {
		t.Fatalf("deep YAML error = %v, want ErrManifest", err)
	}
}

func TestSelectPipelinesForNormalizedEvents(t *testing.T) {
	config, err := Parse([]byte(`version: 1
on:
  push:
    branches: [main, "release/*"]
  pull_request:
    branches: [main]
pipelines:
  z-lint:
    steps: [checkout, golangci-lint]
  a-test:
    steps: [checkout, go-test]
`))
	if err != nil {
		t.Fatal(err)
	}
	baseEvent := webhookevent.RepositoryEvent{SchemaVersion: webhookevent.RepositoryEventSchemaVersion}
	tests := []struct {
		name  string
		event webhookevent.RepositoryEvent
		want  []string
	}{
		{
			name: "push exact branch",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Type, event.Ref = "push", "repository.push", "refs/heads/main"
				return event
			}(),
			want: []string{"a-test", "z-lint"},
		},
		{
			name: "push wildcard branch",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Type, event.Ref = "push", "repository.push", "refs/heads/release/2026.10"
				return event
			}(),
			want: []string{"a-test", "z-lint"},
		},
		{
			name: "pull request filters by base branch",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Action, event.Type = "pull_request", "synchronize", "repository.pull_request.synchronize"
				event.PullRequest = &webhookevent.PullRequestRef{State: "open", HeadRef: "feature", BaseRef: "main"}
				return event
			}(),
			want: []string{"a-test", "z-lint"},
		},
		{
			name: "deleted branch",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Type, event.Ref, event.Deleted = "push", "repository.push", "refs/heads/main", true
				return event
			}(),
		},
		{
			name: "tag push",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Type, event.Ref = "push", "repository.push", "refs/tags/v1.0"
				return event
			}(),
		},
		{
			name: "closed pull request",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Action, event.Type = "pull_request", "closed", "repository.pull_request.closed"
				event.PullRequest = &webhookevent.PullRequestRef{State: "closed", BaseRef: "main"}
				return event
			}(),
		},
		{
			name: "unsupported schema version",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.SchemaVersion++
				event.GitHubEvent, event.Type, event.Ref = "push", "repository.push", "refs/heads/main"
				return event
			}(),
		},
		{
			name: "PR head branch does not control selection",
			event: func() webhookevent.RepositoryEvent {
				event := baseEvent
				event.GitHubEvent, event.Action, event.Type = "pull_request", "opened", "repository.pull_request.opened"
				event.PullRequest = &webhookevent.PullRequestRef{State: "open", HeadRef: "feature", BaseRef: "develop"}
				return event
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := Select(config, test.event); !equalStrings(got, test.want) {
				t.Fatalf("Select() = %v, want %v", got, test.want)
			}
		})
	}
}

func TestSelectEmptyBranchListMatchesAllBranches(t *testing.T) {
	config, err := Parse([]byte("version: 1\non:\n  push: {}\npipelines:\n  verify:\n    steps: [checkout]\n"))
	if err != nil {
		t.Fatal(err)
	}
	event := webhookevent.RepositoryEvent{
		SchemaVersion: webhookevent.RepositoryEventSchemaVersion,
		GitHubEvent:   "push",
		Type:          "repository.push",
		Ref:           "refs/heads/feature/new",
	}
	if got := Select(config, event); !equalStrings(got, []string{"verify"}) {
		t.Fatalf("Select() = %v, want [verify]", got)
	}
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}
