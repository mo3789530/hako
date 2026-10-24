package main

import (
	"context"
	"crypto/ed25519"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"log"
	"os"
	"regexp"
	"strings"

	"github.com/aws/aws-lambda-go/lambda"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspaceruntime"
)

type deploymentConfig struct {
	ResourcePlaneID    domain.ResourcePlaneID
	Region             string
	JournalBucket      string
	JournalPrefix      string
	TicketKeyID        string
	TicketKeySecretARN string
	ReportEndpoint     string
	ReportKeySecretARN string
}

func main() {
	config, err := loadDeploymentConfig(os.Getenv)
	if err != nil {
		log.Fatal("configure Workspace Service controller failed (failure_class=configuration)")
	}
	handler, err := buildHandler(context.Background(), config)
	if err != nil {
		log.Fatal("configure Workspace Service controller")
	}
	lambda.Start(handler.Handle)
}

func loadDeploymentConfig(getenv func(string) string) (deploymentConfig, error) {
	if getenv == nil {
		return deploymentConfig{}, errors.New("environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	cfg := deploymentConfig{
		ResourcePlaneID: domain.ResourcePlaneID(get("HAKO_RESOURCE_PLANE_ID")), Region: get("AWS_REGION"),
		JournalBucket: get("HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET"), JournalPrefix: get("HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX"),
		TicketKeyID: get("HAKO_WORKSPACE_SERVICE_TICKET_KEY_ID"), TicketKeySecretARN: get("HAKO_WORKSPACE_SERVICE_TICKET_KEY_SECRET_ARN"),
		ReportEndpoint: get("HAKO_WORKSPACE_SERVICE_REPORT_ENDPOINT"), ReportKeySecretARN: get("HAKO_WORKSPACE_SERVICE_REPORT_KEY_SECRET_ARN"),
	}
	if cfg.ResourcePlaneID == "" || !serviceKeyIDPattern.MatchString(cfg.TicketKeyID) || cfg.Region == "" ||
		cfg.JournalBucket == "" || cfg.JournalPrefix == "" || !validSecretARN(cfg.TicketKeySecretARN) || cfg.ReportEndpoint == "" || !validSecretARN(cfg.ReportKeySecretARN) {
		return deploymentConfig{}, errors.New("Workspace Service controller requires Plane, Region, journal, ticket-key, and report settings")
	}
	if cfg.TicketKeySecretARN == cfg.ReportKeySecretARN || secretARNRegion(cfg.TicketKeySecretARN) != cfg.Region || secretARNRegion(cfg.ReportKeySecretARN) != cfg.Region {
		return deploymentConfig{}, errors.New("Workspace Service keys must use distinct Secrets Manager ARNs in the Resource Plane Region")
	}
	return cfg, nil
}

func buildHandler(ctx context.Context, cfg deploymentConfig) (*workspaceruntime.WorkspaceServiceLambdaHandler, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return nil, fmt.Errorf("load AWS SDK configuration: %w", err)
	}
	secretClient := secretsmanager.NewFromConfig(awsCfg)
	ticketKeyPEM, err := getSecretValue(ctx, secretClient, cfg.TicketKeySecretARN)
	if err != nil {
		return nil, err
	}
	privateKey, err := parseEd25519PrivateKey(ticketKeyPEM)
	clear(ticketKeyPEM)
	if err != nil {
		return nil, err
	}
	reportKey, err := getSecretValue(ctx, secretClient, cfg.ReportKeySecretARN)
	if err != nil {
		return nil, err
	}
	defer clear(reportKey)
	if len(reportKey) < 32 || len(reportKey) > 4096 {
		return nil, errors.New("Workspace Service report HMAC Secret has an invalid size")
	}
	journal, err := workspacejournal.NewS3Store(s3.NewFromConfig(awsCfg), cfg.JournalBucket, cfg.JournalPrefix)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace journal: %w", err)
	}
	microVMs := lambdamicrovms.NewFromConfig(awsCfg)
	issuer := &workspaceagent.ServiceCapabilityIssuer{KeyID: cfg.TicketKeyID, PrivateKey: append(ed25519.PrivateKey(nil), privateKey...)}
	clear(privateKey)
	reports, err := workspaceruntime.NewSignedWorkspaceServiceReportSink(cfg.ReportEndpoint, reportKey, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace Service report sink: %w", err)
	}
	resolver, err := workspaceruntime.NewSignedWorkspaceServiceCommandResolver(cfg.ReportEndpoint, cfg.ResourcePlaneID, reportKey, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace Service desired-state resolver: %w", err)
	}
	secretProvider, err := workspaceruntime.NewAWSWorkspaceServiceSecretProvider(secretClient)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace Service generated-secret provider: %w", err)
	}
	consumer, err := workspaceruntime.NewWorkspaceServiceCommandConsumer(cfg.ResourcePlaneID, cfg.Region, journal, microVMs,
		issuer, workspaceruntime.NewWorkspaceServiceHTTPGuest(nil), reports, resolver, nil)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace Service command consumer: %w", err)
	}
	consumer.WithSecretProvider(secretProvider)
	handler, err := workspaceruntime.NewWorkspaceServiceLambdaHandler(consumer)
	if err != nil {
		return nil, fmt.Errorf("configure Workspace Service SQS handler: %w", err)
	}
	return handler, nil
}

var serviceKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

func parseEd25519PrivateKey(data []byte) (ed25519.PrivateKey, error) {
	if len(data) == 0 || len(data) > 16<<10 {
		return nil, errors.New("Workspace Service ticket key Secret is unavailable or oversized")
	}
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 || block.Type != "PRIVATE KEY" {
		return nil, errors.New("Workspace Service ticket key must be one PKCS#8 PEM block")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, errors.New("Workspace Service ticket key is invalid")
	}
	privateKey, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("Workspace Service ticket key must be Ed25519")
	}
	return append(ed25519.PrivateKey(nil), privateKey...), nil
}

type secretValueAPI interface {
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
}

func getSecretValue(ctx context.Context, client secretValueAPI, arn string) ([]byte, error) {
	if ctx == nil || client == nil || !validSecretARN(arn) {
		return nil, errors.New("Workspace Service Secret reference is invalid")
	}
	output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
	if err != nil || output == nil {
		return nil, errors.New("Workspace Service Secret is unavailable")
	}
	if output.SecretString != nil {
		if *output.SecretString == "" {
			return nil, errors.New("Workspace Service Secret is empty")
		}
		return []byte(*output.SecretString), nil
	}
	if len(output.SecretBinary) > 0 {
		return append([]byte(nil), output.SecretBinary...), nil
	}
	return nil, errors.New("Workspace Service Secret is empty")
}

func validSecretARN(value string) bool {
	return secretARNPattern.MatchString(value)
}

func secretARNRegion(value string) string {
	parts := strings.Split(value, ":")
	if len(parts) < 7 {
		return ""
	}
	return parts[3]
}

var secretARNPattern = regexp.MustCompile(`^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$`)
