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

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/store/githubregistry"
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
