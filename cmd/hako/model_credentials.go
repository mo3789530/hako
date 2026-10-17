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
	"github.com/mo3789530/hako/internal/store/modelcredentials"
)

func parseModelCredentialCLI(args []string) (api.ModelCredentialCommand, error) {
	var command api.ModelCredentialCommand
	if len(args) == 0 {
		return command, errors.New("model credential action required")
	}
	command.Action = args[0]
	flags := flag.NewFlagSet("model credential "+command.Action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&command.TenantID, "tenant", "", "Tenant ID")
	flags.StringVar(&command.Provider, "provider", modelcredentials.ProviderAnthropic, "model provider")
	flags.StringVar(&command.BindingID, "binding", "", "operator-approved model credential binding ID")
	flags.StringVar(&command.SecretVersionID, "version-id", "", "Secrets Manager immutable VersionId; never the secret value")
	flags.Int64Var(&command.ExpectedGeneration, "generation", 0, "expected current credential generation")
	if flags.Parse(args[1:]) != nil || flags.NArg() != 0 || strings.TrimSpace(command.TenantID) == "" {
		return command, errors.New("invalid model credential flags; --tenant is required")
	}
	switch command.Action {
	case "list":
	case "add":
		if command.BindingID == "" || !modelcredentials.ValidSecretVersionID(command.SecretVersionID) {
			return command, errors.New("add requires --binding and a 32-character --version-id")
		}
	case "rotate":
		if command.ExpectedGeneration <= 0 || !modelcredentials.ValidSecretVersionID(command.SecretVersionID) {
			return command, errors.New("rotate requires --generation and a 32-character --version-id")
		}
	case "disable":
		if command.ExpectedGeneration <= 0 {
			return command, errors.New("disable requires --generation")
		}
	default:
		return command, errors.New("supported actions: list, add, rotate, disable")
	}
	return command, nil
}

func manageModelCredentialCLI(args []string) (api.ModelCredentialResult, error) {
	command, err := parseModelCredentialCLI(args)
	if err != nil {
		return api.ModelCredentialResult{}, err
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return api.ModelCredentialResult{}, err
	}
	return api.ManageModelCredential(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token, command, nil)
}
