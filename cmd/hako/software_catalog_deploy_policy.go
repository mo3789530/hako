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
	"github.com/mo3789530/hako/internal/deploypolicy"
)

type softwareCatalogDeployPolicyCLIInput struct {
	action    string
	tenantID  string
	service   string
	file      string
	tokenFile string
}

func parseSoftwareCatalogDeployPolicyCLI(args []string) (softwareCatalogDeployPolicyCLIInput, error) {
	var input softwareCatalogDeployPolicyCLIInput
	if len(args) == 0 || (args[0] != "show" && args[0] != "set" && args[0] != "verify") {
		return input, errors.New("action must be show, set, or verify")
	}
	input.action = args[0]
	flags := flag.NewFlagSet("catalog deploy-policy "+input.action, flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	flags.StringVar(&input.tenantID, "tenant", "", "Tenant ID")
	flags.StringVar(&input.service, "service", "", "Software Catalog service ID")
	flags.StringVar(&input.file, "file", "", "Path to OIDC deploy policy JSON")
	flags.StringVar(&input.tokenFile, "token-file", "", "Path to a short-lived GitHub OIDC token file (verify only)")
	if err := flags.Parse(args[1:]); err != nil || flags.NArg() != 0 || strings.TrimSpace(input.tenantID) == "" || strings.TrimSpace(input.service) == "" {
		return softwareCatalogDeployPolicyCLIInput{}, errors.New("--tenant and --service are required; positional arguments are not accepted")
	}
	input.tenantID, input.service, input.file, input.tokenFile = strings.TrimSpace(input.tenantID), strings.TrimSpace(input.service), strings.TrimSpace(input.file), strings.TrimSpace(input.tokenFile)
	if (input.action == "set" && (input.file == "" || input.tokenFile != "")) ||
		(input.action == "show" && (input.file != "" || input.tokenFile != "")) ||
		(input.action == "verify" && (input.file != "" || input.tokenFile == "")) {
		return softwareCatalogDeployPolicyCLIInput{}, errors.New("set requires --file; verify requires --token-file; show accepts neither")
	}
	return input, nil
}

func runSoftwareCatalogDeployPolicyCLI(args []string, output io.Writer) error {
	input, err := parseSoftwareCatalogDeployPolicyCLI(args)
	if err != nil {
		return err
	}
	if output == nil {
		return errors.New("output writer is required")
	}
	token, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	baseURL := strings.TrimSpace(os.Getenv("HAKO_API_URL"))
	var result any
	if input.action == "show" {
		result, err = api.GetSoftwareCatalogDeployPolicy(context.Background(), baseURL, token, input.tenantID, input.service, nil)
	} else if input.action == "set" {
		data, readErr := readBoundedCLIFile(input.file, 16<<10)
		if readErr != nil {
			return errors.New("read OIDC deploy policy file")
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		var policy deploypolicy.Policy
		if decoder.Decode(&policy) != nil || decoder.Decode(new(any)) != io.EOF || policy.Validate() != nil {
			return errors.New("OIDC deploy policy file is invalid")
		}
		result, err = api.SetSoftwareCatalogDeployPolicy(context.Background(), baseURL, token, input.tenantID, input.service, policy, nil)
	} else {
		data, readErr := readBoundedCLIFile(input.tokenFile, 64<<10)
		if readErr != nil {
			return errors.New("read GitHub OIDC token file")
		}
		result, err = api.VerifySoftwareCatalogDeployPolicy(context.Background(), baseURL, token, input.tenantID, input.service, strings.TrimSpace(string(data)), nil)
	}
	if err != nil {
		return err
	}
	return json.NewEncoder(output).Encode(result)
}

func readBoundedCLIFile(path string, limit int64) ([]byte, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("file is not a bounded regular file")
	}
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		return nil, errors.New("file exceeds configured size limit")
	}
	return data, nil
}
