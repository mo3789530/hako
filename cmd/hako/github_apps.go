package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/idgen"
)

func parseGitHubAppCLI(args []string) (api.GitHubAppMetadataRequest, error) {
	var input api.GitHubAppMetadataRequest
	if len(args) == 0 {
		return input, errors.New("GitHub App action required")
	}
	input.Action = args[0]
	flags := flag.NewFlagSet("github app "+input.Action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.TenantID, "tenant", "", "Tenant ID")
	flags.StringVar(&input.IntegrationID, "integration", "", "App integration ID")
	flags.Int64Var(&input.Draft.GitHubAppID, "app-id", 0, "GitHub App ID")
	flags.StringVar(&input.Draft.Slug, "slug", "", "GitHub App slug")
	flags.StringVar(&input.Draft.ClientID, "client-id", "", "GitHub App OAuth client ID (not secret)")
	flags.StringVar(&input.Draft.CredentialBindingID, "credential-binding", "", "operator-approved binding ID (not Secret ARN)")
	flags.StringVar(&input.IdempotencyKey, "idempotency-key", "", "reuse for identical create retry")
	flags.Int64Var(&input.ExpectedRevision, "revision", 0, "expected configuration revision")
	flags.IntVar(&input.Limit, "limit", 50, "list page size")
	flags.IntVar(&input.Offset, "offset", 0, "list offset")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || strings.TrimSpace(input.TenantID) == "" {
		return input, errors.New("invalid GitHub App flags; --tenant is required")
	}
	switch input.Action {
	case "list":
	case "add":
		if input.IdempotencyKey == "" {
			key, err := idgen.New("req_")
			if err != nil {
				return input, err
			}
			input.IdempotencyKey = key
		}
	case "show", "disable", "verify", "select-default", "setup":
		if input.IntegrationID == "" || (input.Action != "show" && input.Action != "setup" && input.ExpectedRevision < 1) {
			return input, errors.New("--integration and positive --revision are required for changes")
		}
	default:
		return input, errors.New("supported actions: list, add, show, setup, disable, verify, select-default")
	}
	return input, nil
}

func manageGitHubAppCLI(args []string) (api.GitHubAppMetadataResult, string, error) {
	input, err := parseGitHubAppCLI(args)
	if err != nil {
		return api.GitHubAppMetadataResult{}, "", err
	}
	if input.Action == "setup" {
		token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
		if err != nil {
			return api.GitHubAppMetadataResult{}, "", err
		}
		setup, err := api.BeginTenantGitHubAppSetup(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token, input.TenantID, input.IntegrationID, nil)
		if err != nil {
			return api.GitHubAppMetadataResult{}, "", err
		}
		if err := cliBrowserOpener(setup.SetupURL); err != nil {
			return api.GitHubAppMetadataResult{}, "", err
		}
		return api.GitHubAppMetadataResult{SetupURL: setup.SetupURL, ExpiresAt: &setup.ExpiresAt}, "", nil
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.GitHubAppMetadataResult{}, input.IdempotencyKey, err
	}
	result, err := api.ManageGitHubApp(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token, input, nil)
	return result, input.IdempotencyKey, err
}
