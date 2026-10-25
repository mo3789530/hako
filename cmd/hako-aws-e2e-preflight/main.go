package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sts"
	"github.com/mo3789530/hako/internal/awse2e"
)

type identityLookup func(context.Context, string, string) (string, error)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Getenv, lookupCallerAccount, os.Stdout); err != nil {
		log.Printf("AWS E2E preflight failed: %v", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, getenv func(string) string, lookup identityLookup, output io.Writer) error {
	if len(args) != 1 || args[0] != "preflight" {
		return errors.New("usage: hako-aws-e2e-preflight preflight")
	}
	settings := configFromEnvironment(getenv)
	if err := settings.Validate(); err != nil {
		return err
	}
	if err := rejectAmbientCredentials(getenv); err != nil {
		return err
	}
	controlAccountID, err := lookup(ctx, settings.ControlPlaneProfile, settings.Region)
	if err != nil {
		return fmt.Errorf("resolve Control Plane caller account: %w", err)
	}
	resourceAccountID, err := lookup(ctx, settings.ResourcePlaneProfile, settings.Region)
	if err != nil {
		return fmt.Errorf("resolve Resource Plane caller account: %w", err)
	}
	if err := settings.ValidateCallerAccounts(controlAccountID, resourceAccountID); err != nil {
		return err
	}
	_, err = fmt.Fprintf(output, "AWS E2E preflight passed: run=%s region=%s control_account=%s resource_account=%s\n",
		settings.RunID, settings.Region, settings.ControlPlaneAccountID, settings.ResourcePlaneAccountID)
	return err
}

func configFromEnvironment(getenv func(string) string) awse2e.Config {
	return awse2e.Config{
		Enabled:                strings.EqualFold(strings.TrimSpace(getenv("HAKO_AWS_E2E_ENABLED")), "true"),
		Region:                 strings.TrimSpace(getenv("HAKO_AWS_E2E_REGION")),
		RunID:                  strings.TrimSpace(getenv("HAKO_AWS_E2E_RUN_ID")),
		ControlPlaneProfile:    strings.TrimSpace(getenv("HAKO_AWS_E2E_CONTROL_PROFILE")),
		ResourcePlaneProfile:   strings.TrimSpace(getenv("HAKO_AWS_E2E_RESOURCE_PROFILE")),
		ControlPlaneAccountID:  strings.TrimSpace(getenv("HAKO_AWS_E2E_CONTROL_ACCOUNT_ID")),
		ResourcePlaneAccountID: strings.TrimSpace(getenv("HAKO_AWS_E2E_RESOURCE_ACCOUNT_ID")),
		ManagementAccountID:    strings.TrimSpace(getenv("HAKO_AWS_E2E_MANAGEMENT_ACCOUNT_ID")),
		ProductionAccountIDs:   splitAccountIDs(getenv("HAKO_AWS_E2E_PRODUCTION_ACCOUNT_IDS")),
	}
}

func splitAccountIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return nil
	}
	parts := strings.Split(value, ",")
	result := make([]string, 0, len(parts))
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			result = append(result, strings.TrimSpace(part))
		}
	}
	return result
}

func rejectAmbientCredentials(getenv func(string) string) error {
	for _, name := range []string{
		"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_WEB_IDENTITY_TOKEN_FILE", "AWS_ROLE_ARN", "AWS_CONTAINER_CREDENTIALS_RELATIVE_URI",
		"AWS_CONTAINER_CREDENTIALS_FULL_URI", "AWS_CONTAINER_AUTHORIZATION_TOKEN",
	} {
		if strings.TrimSpace(getenv(name)) != "" {
			return fmt.Errorf("refusing ambient AWS credentials from %s; use the explicitly configured SSO profile", name)
		}
	}
	return nil
}

func lookupCallerAccount(ctx context.Context, profile, region string) (string, error) {
	awsConfig, err := config.LoadDefaultConfig(ctx,
		config.WithSharedConfigProfile(profile),
		config.WithRegion(region),
	)
	if err != nil {
		return "", errors.New("load explicitly selected AWS profile")
	}
	identity, err := sts.NewFromConfig(awsConfig).GetCallerIdentity(ctx, &sts.GetCallerIdentityInput{})
	if err != nil {
		return "", errors.New("STS GetCallerIdentity failed")
	}
	if identity.Account == nil {
		return "", errors.New("STS GetCallerIdentity returned no account ID")
	}
	return *identity.Account, nil
}
