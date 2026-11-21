package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/softwarecatalog"
)

type softwareCatalogCLIInput struct {
	action   string
	tenantID string
	service  softwarecatalog.Service
	file     string
}

func parseSoftwareCatalogCLI(args []string) (softwareCatalogCLIInput, error) {
	var input softwareCatalogCLIInput
	if len(args) == 0 || (args[0] != "list" && args[0] != "set") {
		return input, errors.New("action must be list or set")
	}
	input.action = args[0]
	flags := flag.NewFlagSet("catalog "+input.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.StringVar(&input.service.ServiceID, "service", "", "Stable Software Catalog service ID")
	flags.StringVar(&input.file, "file", "", "Path to service metadata JSON")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" {
		return softwareCatalogCLIInput{}, errors.New("--tenant is required and positional arguments are not accepted")
	}
	input.tenantID = strings.TrimSpace(input.tenantID)
	input.service.TenantID = input.tenantID
	if input.action == "set" && (strings.TrimSpace(input.service.ServiceID) == "" || strings.TrimSpace(input.file) == "") {
		return softwareCatalogCLIInput{}, errors.New("set requires --service and --file")
	}
	if input.action == "list" && (input.service.ServiceID != "" || input.file != "") {
		return softwareCatalogCLIInput{}, errors.New("list does not accept --service or --file")
	}
	return input, nil
}

func runSoftwareCatalogCLI(args []string, output io.Writer) error {
	input, err := parseSoftwareCatalogCLI(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("output writer is required")
	}
	if input.action == "list" {
		token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
		if err != nil {
			return err
		}
		result, err := api.ListSoftwareCatalog(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token, input.tenantID, nil)
		if err != nil {
			return err
		}
		return json.NewEncoder(output).Encode(result)
	}
	data, err := os.ReadFile(input.file)
	if err != nil || len(data) == 0 || len(data) > 16<<10 {
		return errors.New("read Software Catalog metadata file")
	}
	var request struct {
		OwnerTeam          string   `json:"owner_team"`
		InstallationID     int64    `json:"installation_id"`
		GitHubRepositoryID int64    `json:"github_repository_id"`
		Runtime            string   `json:"runtime"`
		Language           string   `json:"language"`
		Dependencies       []string `json:"dependencies"`
		ProductionURL      string   `json:"production_url,omitempty"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
		return errors.New("Software Catalog metadata file must be one valid JSON object")
	}
	input.service.OwnerTeam = strings.TrimSpace(request.OwnerTeam)
	input.service.InstallationID = request.InstallationID
	input.service.GitHubRepositoryID = request.GitHubRepositoryID
	input.service.Runtime = strings.TrimSpace(request.Runtime)
	input.service.Language = strings.TrimSpace(request.Language)
	input.service.Dependencies = append([]string(nil), request.Dependencies...)
	input.service.ProductionURL = strings.TrimSpace(request.ProductionURL)
	if softwarecatalog.Validate(input.service) != nil {
		return errors.New("Software Catalog metadata is invalid")
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	stored, err := api.UpsertSoftwareCatalogService(context.Background(), strings.TrimSpace(os.Getenv("HAKO_API_URL")), token, input.service, nil)
	if err != nil {
		return err
	}
	if err := json.NewEncoder(output).Encode(stored); err != nil {
		return fmt.Errorf("write Software Catalog response: %w", err)
	}
	return nil
}
