package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspaceruntime"
)

type workspaceRepositoryCheckoutConfig struct {
	BrokerURL, BrokerCertARN, BrokerKeyARN, BrokerCAARN string
	GatewayOrigin, TicketKeyID, TicketKeyARN            string
	ReportEndpoint, ReportKeyARN                        string
}

var workspaceRepositoryTicketKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type workspaceRepositoryMicroVMAPI interface {
	GetMicrovm(context.Context, *lambdamicrovms.GetMicrovmInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.GetMicrovmOutput, error)
	CreateMicrovmAuthToken(context.Context, *lambdamicrovms.CreateMicrovmAuthTokenInput, ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error)
}

func configuredWorkspaceRepositoryCheckout(getenv func(string) string, region string) (workspaceRepositoryCheckoutConfig, bool, error) {
	if getenv == nil {
		return workspaceRepositoryCheckoutConfig{}, false, errors.New("Workspace repository checkout environment is unavailable")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	cfg := workspaceRepositoryCheckoutConfig{BrokerURL: get("HAKO_WORKSPACE_REPOSITORY_CHECKOUT_BROKER_URL"),
		BrokerCertARN: get("HAKO_WORKSPACE_REPOSITORY_BROKER_CLIENT_CERT_SECRET_ARN"), BrokerKeyARN: get("HAKO_WORKSPACE_REPOSITORY_BROKER_CLIENT_KEY_SECRET_ARN"),
		BrokerCAARN: get("HAKO_WORKSPACE_REPOSITORY_BROKER_CA_SECRET_ARN"), GatewayOrigin: get("HAKO_WORKSPACE_REPOSITORY_GATEWAY_ORIGIN"),
		TicketKeyID: get("HAKO_WORKSPACE_REPOSITORY_TICKET_KEY_ID"), TicketKeyARN: get("HAKO_WORKSPACE_REPOSITORY_TICKET_KEY_SECRET_ARN"),
		ReportEndpoint: get("HAKO_WORKSPACE_REPOSITORY_REPORT_ENDPOINT"), ReportKeyARN: get("HAKO_WORKSPACE_REPOSITORY_REPORT_KEY_SECRET_ARN")}
	values := []string{cfg.BrokerURL, cfg.BrokerCertARN, cfg.BrokerKeyARN, cfg.BrokerCAARN, cfg.GatewayOrigin, cfg.TicketKeyID, cfg.TicketKeyARN, cfg.ReportEndpoint, cfg.ReportKeyARN}
	allEmpty := true
	for _, value := range values {
		if value != "" {
			allEmpty = false
		}
	}
	if allEmpty {
		return workspaceRepositoryCheckoutConfig{}, false, nil
	}
	broker, brokerErr := url.Parse(cfg.BrokerURL)
	gateway, gatewayErr := url.Parse(cfg.GatewayOrigin)
	report, reportErr := url.Parse(cfg.ReportEndpoint)
	if strings.TrimSpace(region) == "" || brokerErr != nil || broker.Scheme != "https" || broker.Host == "" || broker.User != nil || broker.Path != "" || broker.RawQuery != "" || broker.Fragment != "" ||
		gatewayErr != nil || gateway.Scheme != "https" || gateway.Host == "" || gateway.User != nil || gateway.Path != "" || gateway.RawQuery != "" || gateway.Fragment != "" ||
		reportErr != nil || report.Scheme != "https" || report.Host == "" || report.User != nil || (report.Path != "" && report.Path != "/") || report.RawQuery != "" || report.Fragment != "" ||
		!workspaceRepositoryTicketKeyIDPattern.MatchString(cfg.TicketKeyID) || cfg.TicketKeyARN == "" || cfg.ReportKeyARN == "" ||
		!validResourcePlaneSecretARN(cfg.BrokerCertARN) || !validResourcePlaneSecretARN(cfg.BrokerKeyARN) || !validResourcePlaneSecretARN(cfg.BrokerCAARN) ||
		!validResourcePlaneSecretARN(cfg.TicketKeyARN) || !validResourcePlaneSecretARN(cfg.ReportKeyARN) {
		return workspaceRepositoryCheckoutConfig{}, false, errors.New("Workspace repository checkout Broker, Gateway, ticket, or report settings are invalid")
	}
	arns := []string{cfg.BrokerCertARN, cfg.BrokerKeyARN, cfg.BrokerCAARN, cfg.TicketKeyARN, cfg.ReportKeyARN}
	for i, arn := range arns {
		if resourcePlaneSecretARNRegion(arn) != region {
			return workspaceRepositoryCheckoutConfig{}, false, errors.New("Workspace repository checkout Secrets must match the Resource Plane Region")
		}
		for _, other := range arns[:i] {
			if arn == other {
				return workspaceRepositoryCheckoutConfig{}, false, errors.New("Workspace repository checkout Secrets must be distinct")
			}
		}
	}
	return cfg, true, nil
}

func composeWorkspaceRepositoryCheckout(ctx context.Context, awsCfg aws.Config, plane domain.ResourcePlaneID, cfg workspaceRepositoryCheckoutConfig, api workspaceRepositoryMicroVMAPI) (*workspaceruntime.WorkspaceRepositoryCheckoutWorkflow, error) {
	if ctx == nil || plane == "" || api == nil {
		return nil, errors.New("Workspace repository checkout identity and MicroVM API are required")
	}
	secretClient := secretsmanager.NewFromConfig(awsCfg)
	certPEM, err := readPreviewSecret(ctx, secretClient, cfg.BrokerCertARN, 64<<10)
	if err != nil {
		return nil, errors.New("load Workspace repository Broker certificate")
	}
	keyPEM, err := readPreviewSecret(ctx, secretClient, cfg.BrokerKeyARN, 64<<10)
	if err != nil {
		clearPreviewSecret(certPEM)
		return nil, errors.New("load Workspace repository Broker private key")
	}
	defer clearPreviewSecret(keyPEM)
	caPEM, err := readPreviewSecret(ctx, secretClient, cfg.BrokerCAARN, 64<<10)
	if err != nil {
		clearPreviewSecret(certPEM)
		return nil, errors.New("load Workspace repository Broker CA")
	}
	certificate, roots, err := previewCheckoutTLS(certPEM, keyPEM, caPEM)
	clearPreviewSecret(certPEM)
	clearPreviewSecret(caPEM)
	if err != nil {
		return nil, errors.New("configure Workspace repository Broker mTLS")
	}
	brokerClient, err := githubbroker.NewResourcePlaneCheckoutMTLSClient(string(plane), certificate, roots)
	if err != nil {
		return nil, errors.New("configure Workspace repository Broker caller identity")
	}
	provider, err := githubbroker.NewHTTPWorkspaceCheckoutProvider(cfg.BrokerURL, brokerClient)
	if err != nil {
		return nil, errors.New("configure Workspace repository Broker endpoint")
	}
	ticketPEM, err := readPreviewSecret(ctx, secretClient, cfg.TicketKeyARN, 16<<10)
	if err != nil {
		return nil, errors.New("load Workspace repository Guest ticket key")
	}
	privateKey, err := workloadagent.LoadTicketSigningKey(bytes.NewReader(ticketPEM))
	clearPreviewSecret(ticketPEM)
	if err != nil || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("parse Workspace repository Guest ticket key")
	}
	issuer := &workspaceagent.AgentTicketIssuer{KeyID: cfg.TicketKeyID, PrivateKey: privateKey}
	reportsKey, err := readPreviewSecret(ctx, secretClient, cfg.ReportKeyARN, 4096)
	if err != nil {
		for i := range privateKey {
			privateKey[i] = 0
		}
		return nil, errors.New("load Workspace repository report HMAC key")
	}
	defer clearPreviewSecret(reportsKey)
	reports, err := workspaceruntime.NewSignedWorkspaceRepositoryCheckoutReportSink(cfg.ReportEndpoint, reportsKey, nil, nil)
	if err != nil {
		for i := range privateKey {
			privateKey[i] = 0
		}
		return nil, errors.New("configure Workspace repository report sink")
	}
	guest, err := workspaceruntime.NewLambdaWorkspaceRepositoryGuestInstaller(workspaceruntime.LambdaWorkspaceRepositoryGuestInstallerConfig{
		ResourcePlaneID: plane, Region: awsCfg.Region, GatewayOrigin: cfg.GatewayOrigin, MicroVMs: api,
		Tokens: workspaceagent.LambdaMicroVMAuthTokenProvider{API: api}, Tickets: issuer})
	if err != nil {
		for i := range privateKey {
			privateKey[i] = 0
		}
		return nil, errors.New("configure Workspace repository Guest transfer")
	}
	workflow, err := workspaceruntime.NewWorkspaceRepositoryCheckoutWorkflow(workspaceruntime.WorkspaceRepositoryBrokerCredentialProvider{Provider: provider},
		workspaceruntime.NewGitWorkspaceRepositoryCheckout(nil), guest, reports, nil)
	if err != nil {
		for i := range privateKey {
			privateKey[i] = 0
		}
		return nil, errors.New("configure Workspace repository checkout workflow")
	}
	return workflow, nil
}

func resourcePlaneSecretARNRegion(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) != 7 {
		return ""
	}
	return parts[3]
}
