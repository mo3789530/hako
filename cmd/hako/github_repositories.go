package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/workspacesession"
)

type githubRepositoryFactoryCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	enabled        bool
}

func parseGitHubRepositoryFactoryCLI(args []string) (githubRepositoryFactoryCLIInput, error) {
	var input githubRepositoryFactoryCLIInput
	if len(args) == 0 || (args[0] != "enable" && args[0] != "disable") {
		return input, errors.New("action must be enable or disable")
	}
	input.enabled = args[0] == "enable"
	flags := flag.NewFlagSet("github repository factory "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 || input.repositoryID <= 0 {
		return githubRepositoryFactoryCLIInput{}, errors.New("--tenant, positive --installation, and positive --repository are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	return input, nil
}

func manageGitHubRepositoryFactoryCLI(args []string) (api.GitHubRepositoryFactoryResult, error) {
	input, err := parseGitHubRepositoryFactoryCLI(args)
	if err != nil {
		return api.GitHubRepositoryFactoryResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryFactoryResult{}, err
	}
	return api.SetGitHubRepositoryFactoryEnabled(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.enabled, nil)
}

func manageGitHubRepositoryPreviewCLI(args []string) (api.GitHubRepositoryPreviewResult, error) {
	input, err := parseGitHubRepositoryFactoryCLI(args)
	if err != nil {
		return api.GitHubRepositoryPreviewResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryPreviewResult{}, err
	}
	return api.SetGitHubRepositoryPreviewEnabled(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.enabled, nil)
}

type githubRepositoryPreviewsCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	limit          int
}

func parseGitHubRepositoryPreviewsCLI(args []string) (githubRepositoryPreviewsCLIInput, error) {
	var input githubRepositoryPreviewsCLIInput
	if len(args) == 0 || args[0] != "list" {
		return input, errors.New("action must be list")
	}
	flags := flag.NewFlagSet("github repository previews list", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	flags.IntVar(&input.limit, "limit", 50, "Maximum Previews to return (1-100)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 || input.repositoryID <= 0 || input.limit < 1 || input.limit > 100 {
		return githubRepositoryPreviewsCLIInput{}, errors.New("--tenant, positive --installation, positive --repository, and --limit between 1 and 100 are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	return input, nil
}

func listGitHubRepositoryPreviewsCLI(args []string) (api.GitHubRepositoryPreviewsResult, error) {
	input, err := parseGitHubRepositoryPreviewsCLI(args)
	if err != nil {
		return api.GitHubRepositoryPreviewsResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryPreviewsResult{}, err
	}
	return api.ListGitHubRepositoryPreviews(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.limit, nil)
}

type githubRepositoryPreviewSessionCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	pullRequest    int64
	sessionID      string
	permissions    []workspacesession.Permission
}

func parseGitHubRepositoryPreviewSessionCLI(args []string, action string) (githubRepositoryPreviewSessionCLIInput, error) {
	var input githubRepositoryPreviewSessionCLIInput
	if len(args) == 0 || args[0] != action {
		return input, errors.New("action must be " + action)
	}
	permissionText := "vscode"
	flags := flag.NewFlagSet("github repository previews session "+action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	flags.Int64Var(&input.pullRequest, "pull-request", 0, "Pull Request number")
	flags.StringVar(&input.sessionID, "session", "", "Preview Session ID to revoke")
	if action == "create" {
		flags.StringVar(&permissionText, "permissions", "vscode", "Comma-separated Gateway permissions")
	}
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 ||
		input.repositoryID <= 0 || input.pullRequest <= 0 || (action == "revoke" && len(input.sessionID) != 48) {
		return githubRepositoryPreviewSessionCLIInput{}, errors.New("--tenant, positive --installation, positive --repository, positive --pull-request, and for revoke --session are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	if action == "create" {
		parts := strings.Split(permissionText, ",")
		input.permissions = make([]workspacesession.Permission, 0, len(parts))
		for _, part := range parts {
			permission := workspacesession.Permission(strings.TrimSpace(part))
			if permission == "" {
				return githubRepositoryPreviewSessionCLIInput{}, errors.New("--permissions contains an empty permission")
			}
			input.permissions = append(input.permissions, permission)
		}
		if workspacesession.ValidatePermissions(input.permissions) != nil {
			return githubRepositoryPreviewSessionCLIInput{}, errors.New("--permissions contains an invalid or duplicate permission")
		}
	}
	return input, nil
}

func createGitHubRepositoryPreviewSessionCLI(args []string) (api.RepositoryPreviewSession, error) {
	input, err := parseGitHubRepositoryPreviewSessionCLI(args, "create")
	if err != nil {
		return api.RepositoryPreviewSession{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.RepositoryPreviewSession{}, err
	}
	return api.CreateRepositoryPreviewSession(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.pullRequest, input.permissions, nil)
}

func revokeGitHubRepositoryPreviewSessionCLI(args []string) error {
	input, err := parseGitHubRepositoryPreviewSessionCLI(append([]string{"revoke"}, args...), "revoke")
	if err != nil {
		return err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	return api.RevokeRepositoryPreviewSession(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.pullRequest, input.sessionID, nil)
}

type githubRepositoryActionsRunnerCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	enabled        bool
}

func parseGitHubRepositoryActionsRunnerCLI(args []string) (githubRepositoryActionsRunnerCLIInput, error) {
	var input githubRepositoryActionsRunnerCLIInput
	if len(args) == 0 || (args[0] != "enable" && args[0] != "disable") {
		return input, errors.New("action must be enable or disable")
	}
	input.enabled = args[0] == "enable"
	flags := flag.NewFlagSet("github repository runner "+args[0], flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 || input.repositoryID <= 0 {
		return githubRepositoryActionsRunnerCLIInput{}, errors.New("--tenant, positive --installation, and positive --repository are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	return input, nil
}

func manageGitHubRepositoryActionsRunnerCLI(args []string) (api.GitHubRepositoryActionsRunnerResult, error) {
	input, err := parseGitHubRepositoryActionsRunnerCLI(args)
	if err != nil {
		return api.GitHubRepositoryActionsRunnerResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryActionsRunnerResult{}, err
	}
	return api.SetGitHubRepositoryActionsRunnerEnabled(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.enabled, nil)
}

type githubRepositoryActionsRunnerWorkflowPolicyCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	policyFile     string
}

func parseGitHubRepositoryActionsRunnerWorkflowPolicyCLI(args []string) (githubRepositoryActionsRunnerWorkflowPolicyCLIInput, error) {
	var input githubRepositoryActionsRunnerWorkflowPolicyCLIInput
	if len(args) == 0 || args[0] != "policy" {
		return input, errors.New("action must be policy")
	}
	flags := flag.NewFlagSet("github repository runner policy", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	flags.StringVar(&input.policyFile, "policy-file", "", "Path to JSON workflow policy")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 || input.repositoryID <= 0 || strings.TrimSpace(input.policyFile) == "" {
		return githubRepositoryActionsRunnerWorkflowPolicyCLIInput{}, errors.New("--tenant, positive --installation, positive --repository, and --policy-file are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	input.policyFile = strings.TrimSpace(input.policyFile)
	return input, nil
}

func manageGitHubRepositoryActionsRunnerWorkflowPolicyCLI(args []string) (api.GitHubRepositoryActionsRunnerWorkflowPolicyResult, error) {
	input, err := parseGitHubRepositoryActionsRunnerWorkflowPolicyCLI(args)
	if err != nil {
		return api.GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, err
	}
	data, err := os.ReadFile(input.policyFile)
	if err != nil || len(data) == 0 || len(data) > 16<<10 {
		return api.GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("read workflow policy file")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var policy githubregistry.ActionsRunnerWorkflowPolicy
	if decoder.Decode(&policy) != nil {
		return api.GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("decode workflow policy file")
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return api.GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, errors.New("workflow policy file must contain one JSON object")
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryActionsRunnerWorkflowPolicyResult{}, err
	}
	return api.SetGitHubRepositoryActionsRunnerWorkflowPolicy(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, policy, nil)
}

type githubRepositoryAgentWritePolicyCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	policyFile     string
}

func parseGitHubRepositoryAgentWritePolicyCLI(args []string) (githubRepositoryAgentWritePolicyCLIInput, error) {
	var input githubRepositoryAgentWritePolicyCLIInput
	if len(args) == 0 || args[0] != "set" {
		return input, errors.New("action must be set")
	}
	flags := flag.NewFlagSet("github repository agent-policy set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	flags.StringVar(&input.policyFile, "policy-file", "", "Path to JSON Agent write policy")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || input.installationID <= 0 || input.repositoryID <= 0 || strings.TrimSpace(input.policyFile) == "" {
		return githubRepositoryAgentWritePolicyCLIInput{}, errors.New("--tenant, positive --installation, positive --repository, and --policy-file are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	input.policyFile = strings.TrimSpace(input.policyFile)
	return input, nil
}

func manageGitHubRepositoryAgentWritePolicyCLI(args []string) (api.GitHubRepositoryAgentWritePolicyResult, error) {
	input, err := parseGitHubRepositoryAgentWritePolicyCLI(args)
	if err != nil {
		return api.GitHubRepositoryAgentWritePolicyResult{}, err
	}
	data, err := os.ReadFile(input.policyFile)
	if err != nil || len(data) == 0 || len(data) > 16<<10 {
		return api.GitHubRepositoryAgentWritePolicyResult{}, errors.New("read Agent write policy file")
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	var policy agenttask.WritePolicy
	if decoder.Decode(&policy) != nil || decoder.Decode(new(any)) != io.EOF || policy.Validate() != nil {
		return api.GitHubRepositoryAgentWritePolicyResult{}, errors.New("Agent write policy file is invalid")
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryAgentWritePolicyResult{}, err
	}
	return api.SetGitHubRepositoryAgentWritePolicy(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, policy, nil)
}

type githubRepositoryAgentModelAccessCLIInput struct {
	tenantID       string
	installationID int64
	repositoryID   int64
	enabled        bool
}

func parseGitHubRepositoryAgentModelAccessCLI(args []string) (githubRepositoryAgentModelAccessCLIInput, error) {
	var input githubRepositoryAgentModelAccessCLIInput
	if len(args) == 0 || args[0] != "set" {
		return input, errors.New("action must be set")
	}
	enabledText := ""
	flags := flag.NewFlagSet("github repository agent-model-access set", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.Int64Var(&input.installationID, "installation", 0, "GitHub Installation ID")
	flags.Int64Var(&input.repositoryID, "repository", 0, "GitHub Repository ID")
	flags.StringVar(&enabledText, "enabled", "", "Whether Native Agent model access is enabled")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" ||
		input.installationID <= 0 || input.repositoryID <= 0 || (enabledText != "true" && enabledText != "false") {
		return githubRepositoryAgentModelAccessCLIInput{}, errors.New("--tenant, positive --installation, positive --repository, and --enabled true|false are required")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	input.enabled = enabledText == "true"
	return input, nil
}

func manageGitHubRepositoryAgentModelAccessCLI(args []string) (api.GitHubRepositoryAgentModelAccessResult, error) {
	input, err := parseGitHubRepositoryAgentModelAccessCLI(args)
	if err != nil {
		return api.GitHubRepositoryAgentModelAccessResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubRepositoryAgentModelAccessResult{}, err
	}
	return api.SetGitHubRepositoryAgentModelAccess(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token,
		input.tenantID, input.installationID, input.repositoryID, input.enabled, nil)
}
