// Package factoryconfig parses the deliberately restricted Hako-native
// pipeline manifest. It describes allowlisted catalog actions, not shell
// scripts or arbitrary container images.
package factoryconfig

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	webhookevent "github.com/mo3789530/hako/internal/githubwebhook"
	"gopkg.in/yaml.v3"
)

const (
	MaxManifestBytes = 64 << 10
	MaxYAMLNodes     = 2048
	MaxYAMLDepth     = 20
	MaxPipelines     = 16
	MaxSteps         = 32
)

var (
	ErrManifest  = errors.New("invalid Hako factory manifest")
	namePattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,39}$`)
	allowedSteps = map[string]struct{}{
		"checkout": {}, "go-test": {}, "go-vet": {}, "golangci-lint": {},
		"npm-test": {}, "npm-build": {}, "python-test": {},
	}
	allowedServices = map[string]struct{}{"postgres": {}, "redis": {}}
)

type Config struct {
	Version   int                 `yaml:"version"`
	On        Triggers            `yaml:"on"`
	Pipelines map[string]Pipeline `yaml:"pipelines"`
}

type Triggers struct {
	Push        *BranchFilter `yaml:"push,omitempty"`
	PullRequest *BranchFilter `yaml:"pull_request,omitempty"`
}

type BranchFilter struct {
	Branches []string `yaml:"branches,omitempty"`
}

type Pipeline struct {
	Steps         []string `yaml:"steps"`
	Services      []string `yaml:"services,omitempty"`
	RuntimeClass  string   `yaml:"runtime_class,omitempty"`
	TimeoutSecond int      `yaml:"timeout_seconds,omitempty"`
	MemoryMB      int      `yaml:"memory_mb,omitempty"`
	CPUMillis     int      `yaml:"cpu_millis,omitempty"`
	Network       string   `yaml:"network,omitempty"`
	Repository    string   `yaml:"repository,omitempty"`
	CloudAccess   string   `yaml:"cloud_access,omitempty"`
}

// Command is a deterministic execution-plan entry. Executable and Args are
// selected only from the versioned catalog below; manifest text is never
// interpreted as a command or shell fragment.
type Command struct {
	Step       string
	Executable string
	Args       []string
}

// ExecutionPlan contains the static commands and declared local services for
// one validated pipeline. It is a policy artifact only; a trusted worker must
// still bind it to the immutable Run commit and enforce the runtime boundary.
type ExecutionPlan struct {
	Pipeline      string
	RuntimeClass  string
	TimeoutSecond int
	MemoryMB      int
	CPUMillis     int
	Network       string
	Services      []string
	Commands      []Command
}

// Parse rejects YAML features that make manifests difficult to audit, then
// decodes with unknown-field rejection and validates every executable choice.
func Parse(data []byte) (Config, error) {
	if len(data) == 0 || len(data) > MaxManifestBytes {
		return Config{}, manifestError("manifest must be between 1 byte and 64 KiB")
	}
	var node yaml.Node
	nodeDecoder := yaml.NewDecoder(bytes.NewReader(data))
	if err := nodeDecoder.Decode(&node); err != nil {
		return Config{}, manifestError("malformed YAML")
	}
	var trailing yaml.Node
	if err := nodeDecoder.Decode(&trailing); err != io.EOF {
		return Config{}, manifestError("exactly one YAML document is allowed")
	}
	nodes := 0
	if err := inspectYAML(&node, 0, &nodes); err != nil {
		return Config{}, err
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	var config Config
	if err := decoder.Decode(&config); err != nil {
		return Config{}, manifestError("unknown or invalid manifest field")
	}
	if err := validate(&config); err != nil {
		return Config{}, err
	}
	return config, nil
}

func inspectYAML(node *yaml.Node, depth int, count *int) error {
	if node == nil {
		return nil
	}
	(*count)++
	if *count > MaxYAMLNodes || depth > MaxYAMLDepth {
		return manifestError("YAML structure exceeds the node or nesting limit")
	}
	if node.Kind == yaml.AliasNode || node.Alias != nil || node.Anchor != "" || node.Style&yaml.TaggedStyle != 0 {
		return manifestError("YAML aliases, anchors, and explicit tags are not allowed")
	}
	if node.Kind == yaml.MappingNode {
		seen := make(map[string]struct{}, len(node.Content)/2)
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i]
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return manifestError("mapping keys must be strings")
			}
			if _, duplicate := seen[key.Value]; duplicate {
				return manifestError("duplicate mapping key")
			}
			seen[key.Value] = struct{}{}
		}
	}
	for _, child := range node.Content {
		if err := inspectYAML(child, depth+1, count); err != nil {
			return err
		}
	}
	return nil
}

func validate(config *Config) error {
	if config == nil || config.Version != 1 || len(config.Pipelines) == 0 || len(config.Pipelines) > MaxPipelines {
		return manifestError("version 1 and between 1 and 16 pipelines are required")
	}
	if config.On.Push == nil && config.On.PullRequest == nil {
		return manifestError("at least one push or pull_request trigger is required")
	}
	for _, filter := range []*BranchFilter{config.On.Push, config.On.PullRequest} {
		if filter == nil {
			continue
		}
		if len(filter.Branches) > 32 {
			return manifestError("each trigger may declare at most 32 branch filters")
		}
		seen := make(map[string]struct{}, len(filter.Branches))
		for _, branch := range filter.Branches {
			if !validBranchPattern(branch) {
				return manifestError("branch filters must be exact names or end in /*")
			}
			if _, duplicate := seen[branch]; duplicate {
				return manifestError("trigger branch filters must not be duplicated")
			}
			seen[branch] = struct{}{}
		}
	}
	for name, pipeline := range config.Pipelines {
		if !namePattern.MatchString(name) || len(pipeline.Steps) == 0 || len(pipeline.Steps) > MaxSteps {
			return manifestError("pipeline names and step counts are invalid")
		}
		if pipeline.RuntimeClass == "" {
			pipeline.RuntimeClass = "standard"
		}
		if pipeline.RuntimeClass != "small" && pipeline.RuntimeClass != "standard" {
			return manifestError("runtime_class must be small or standard")
		}
		if pipeline.TimeoutSecond == 0 {
			pipeline.TimeoutSecond = 1800
		}
		if pipeline.TimeoutSecond < 30 || pipeline.TimeoutSecond > 3600 {
			return manifestError("timeout_seconds must be between 30 and 3600")
		}
		if pipeline.MemoryMB == 0 {
			pipeline.MemoryMB = 2048
		}
		if pipeline.MemoryMB < 256 || pipeline.MemoryMB > 8192 {
			return manifestError("memory_mb must be between 256 and 8192")
		}
		if pipeline.CPUMillis == 0 {
			pipeline.CPUMillis = 1000
		}
		if pipeline.CPUMillis < 250 || pipeline.CPUMillis > 4000 {
			return manifestError("cpu_millis must be between 250 and 4000")
		}
		if pipeline.Network == "" {
			pipeline.Network = "registry-only"
		}
		if pipeline.Network != "none" && pipeline.Network != "registry-only" {
			return manifestError("network must be none or registry-only")
		}
		if pipeline.Repository == "" {
			pipeline.Repository = "read"
		}
		if pipeline.Repository != "read" || (pipeline.CloudAccess != "" && pipeline.CloudAccess != "none") {
			return manifestError("only repository read access is supported; cloud access must be none")
		}
		seen := make(map[string]struct{}, len(pipeline.Steps))
		for _, step := range pipeline.Steps {
			if _, ok := allowedSteps[step]; !ok {
				return manifestError("pipeline contains an unsupported step")
			}
			if _, duplicate := seen[step]; duplicate {
				return manifestError("pipeline steps must not be duplicated")
			}
			seen[step] = struct{}{}
		}
		if pipeline.Steps[0] != "checkout" {
			return manifestError("checkout must be the first pipeline step")
		}
		if len(pipeline.Services) > 4 {
			return manifestError("pipeline may declare at most four services")
		}
		seen = make(map[string]struct{}, len(pipeline.Services))
		for _, service := range pipeline.Services {
			if _, ok := allowedServices[service]; !ok {
				return manifestError("pipeline contains an unsupported service")
			}
			if _, duplicate := seen[service]; duplicate {
				return manifestError("pipeline services must not be duplicated")
			}
			seen[service] = struct{}{}
		}
		config.Pipelines[name] = pipeline
	}
	return nil
}

// Select returns configured pipeline names that match an authenticated,
// normalized Repository push or pull-request event. The supplied config must
// have been returned by Parse. Results are sorted for deterministic
// idempotency and never use raw provider payload values.
func Select(config Config, event webhookevent.RepositoryEvent) []string {
	if event.SchemaVersion != webhookevent.RepositoryEventSchemaVersion {
		return nil
	}
	var branch string
	var filter *BranchFilter
	switch {
	case event.GitHubEvent == "push" && event.Type == "repository.push" && !event.Deleted:
		const prefix = "refs/heads/"
		if !strings.HasPrefix(event.Ref, prefix) || len(event.Ref) == len(prefix) {
			return nil
		}
		branch, filter = strings.TrimPrefix(event.Ref, prefix), config.On.Push
	case event.GitHubEvent == "pull_request" && (event.Action == "opened" || event.Action == "reopened" || event.Action == "synchronize") &&
		event.Type == "repository.pull_request."+event.Action && event.PullRequest != nil && event.PullRequest.State == "open":
		branch, filter = event.PullRequest.BaseRef, config.On.PullRequest
	default:
		return nil
	}
	if filter == nil || !matchesBranchFilter(filter.Branches, branch) {
		return nil
	}
	names := make([]string, 0, len(config.Pipelines))
	for name := range config.Pipelines {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Plan converts one validated pipeline to catalog commands. Checkout is a
// source-staging action and therefore intentionally has no process command.
// The returned slices are newly allocated and safe for callers to modify.
func Plan(config Config, name string) (ExecutionPlan, error) {
	pipeline, ok := config.Pipelines[name]
	if !ok || !namePattern.MatchString(name) {
		return ExecutionPlan{}, manifestError("pipeline does not exist")
	}
	plan := ExecutionPlan{Pipeline: name, RuntimeClass: pipeline.RuntimeClass,
		TimeoutSecond: pipeline.TimeoutSecond, MemoryMB: pipeline.MemoryMB,
		CPUMillis: pipeline.CPUMillis, Network: pipeline.Network,
		Services: append([]string(nil), pipeline.Services...),
		Commands: make([]Command, 0, len(pipeline.Steps))}
	for _, step := range pipeline.Steps {
		command, ok := catalogCommand(step)
		if !ok {
			return ExecutionPlan{}, manifestError("pipeline contains an unsupported step")
		}
		if command.Step != "checkout" {
			command.Args = append([]string(nil), command.Args...)
			plan.Commands = append(plan.Commands, command)
		}
	}
	return plan, nil
}

func catalogCommand(step string) (Command, bool) {
	switch step {
	case "checkout":
		return Command{Step: step}, true
	case "go-test":
		return Command{Step: step, Executable: "/usr/local/go/bin/go", Args: []string{"test", "./..."}}, true
	case "go-vet":
		return Command{Step: step, Executable: "/usr/local/go/bin/go", Args: []string{"vet", "./..."}}, true
	case "golangci-lint":
		return Command{Step: step, Executable: "/usr/local/bin/golangci-lint", Args: []string{"run", "--timeout=5m"}}, true
	case "npm-test":
		return Command{Step: step, Executable: "/usr/bin/npm", Args: []string{"test"}}, true
	case "npm-build":
		return Command{Step: step, Executable: "/usr/bin/npm", Args: []string{"run", "build"}}, true
	case "python-test":
		return Command{Step: step, Executable: "/usr/bin/python3", Args: []string{"-m", "unittest", "discover"}}, true
	default:
		return Command{}, false
	}
}

// ValidateExecutionPlan verifies that a command plan still exactly matches
// the built-in catalog. Runtime consumers must call this because plans may
// cross a trust boundary after they are created.
func ValidateExecutionPlan(plan ExecutionPlan) error {
	if !namePattern.MatchString(plan.Pipeline) ||
		(plan.RuntimeClass != "small" && plan.RuntimeClass != "standard") ||
		plan.TimeoutSecond < 30 || plan.TimeoutSecond > 3600 || plan.MemoryMB < 256 || plan.MemoryMB > 8192 ||
		plan.CPUMillis < 250 || plan.CPUMillis > 4000 || (plan.Network != "none" && plan.Network != "registry-only") ||
		len(plan.Services) > 4 || len(plan.Commands) >= MaxSteps {
		return manifestError("execution plan metadata is invalid")
	}
	seen := make(map[string]struct{}, len(plan.Commands)+1)
	seen["checkout"] = struct{}{}
	seenServices := make(map[string]struct{}, len(plan.Services))
	for _, service := range plan.Services {
		if _, ok := allowedServices[service]; !ok {
			return manifestError("execution plan contains an unsupported service")
		}
		if _, duplicate := seenServices[service]; duplicate {
			return manifestError("execution plan repeats a service")
		}
		seenServices[service] = struct{}{}
	}
	for _, command := range plan.Commands {
		want, ok := catalogCommand(command.Step)
		if !ok || command.Step == "checkout" || command.Executable != want.Executable || !equalStringSlices(command.Args, want.Args) {
			return manifestError("execution plan command differs from the fixed catalog")
		}
		if _, duplicate := seen[command.Step]; duplicate {
			return manifestError("execution plan repeats a step")
		}
		seen[command.Step] = struct{}{}
	}
	return nil
}

func equalStringSlices(left, right []string) bool {
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

func matchesBranchFilter(patterns []string, branch string) bool {
	if branch == "" {
		return false
	}
	if len(patterns) == 0 {
		return true
	}
	for _, pattern := range patterns {
		if strings.HasSuffix(pattern, "/*") {
			prefix := strings.TrimSuffix(pattern, "*")
			if strings.HasPrefix(branch, prefix) && len(branch) > len(prefix) {
				return true
			}
		} else if pattern == branch {
			return true
		}
	}
	return false
}

func validBranchPattern(value string) bool {
	if len(value) == 0 || len(value) > 255 || strings.TrimSpace(value) != value || strings.Contains(value, "..") || strings.HasPrefix(value, "/") || strings.ContainsAny(value, "\\ ~^:?[") {
		return false
	}
	if strings.Count(value, "*") > 1 || (strings.Contains(value, "*") && !strings.HasSuffix(value, "/*")) {
		return false
	}
	plain := strings.TrimSuffix(value, "/*")
	if plain == "" || strings.HasSuffix(plain, "/") {
		return false
	}
	for _, r := range plain {
		if !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || strings.ContainsRune("._/-", r)) {
			return false
		}
	}
	return true
}

func manifestError(message string) error { return fmt.Errorf("%w: %s", ErrManifest, message) }
