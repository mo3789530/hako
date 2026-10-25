package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubbroker"
)

type agentProposalPublicationSubmitterAdapter struct {
	client *githubbroker.HTTPAgentProposalPublicationClient
}

func (a agentProposalPublicationSubmitterAdapter) Publish(ctx context.Context, tenantID, runID, attemptID string) (api.AgentProposalPublicationSubmission, error) {
	result, err := a.client.Publish(ctx, githubbroker.AgentProposalPublicationRequest{TenantID: domain.TenantID(tenantID),
		WorkloadRunID: domain.WorkloadRunID(runID), AgentAttemptID: attemptID})
	if err != nil {
		return api.AgentProposalPublicationSubmission{}, err
	}
	return api.AgentProposalPublicationSubmission{TenantID: string(result.TenantID), WorkloadRunID: string(result.WorkloadRunID),
		State: string(result.State), BaseBranch: result.DefaultBranch, BranchName: result.BranchName,
		BranchCommitSHA: result.BranchCommitSHA, PullRequestNumber: result.PullRequestNumber, Recovered: result.Recovered}, nil
}

type brokerSecretGetter func(context.Context, string) ([]byte, error)

func tenantAgentProposalBrokerFromEnv() (api.AgentProposalPublicationSubmitter, error) {
	secretARNs := agentProposalBrokerSecretARNs()
	secretCount := countConfigured(secretARNs)
	if secretCount == 0 {
		return tenantAgentProposalBrokerFromEnvWithSecretGetter(context.Background(), nil)
	}
	fileValues := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE")),
	}
	if strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL")) == "" || secretCount != len(secretARNs) || countConfigured(fileValues) != 0 {
		return nil, errors.New("incomplete Agent proposal Broker configuration")
	}
	awsConfig, err := config.LoadDefaultConfig(context.Background())
	if err != nil {
		return nil, errors.New("configure Agent proposal Broker Secret reader")
	}
	client := secretsmanager.NewFromConfig(awsConfig)
	getSecret := func(ctx context.Context, arn string) ([]byte, error) {
		output, err := client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &arn})
		if err != nil {
			return nil, errors.New("read Agent proposal Broker TLS Secret")
		}
		if output.SecretString != nil {
			return []byte(*output.SecretString), nil
		}
		return append([]byte(nil), output.SecretBinary...), nil
	}
	return tenantAgentProposalBrokerFromEnvWithSecretGetter(context.Background(), getSecret)
}

func tenantAgentProposalBrokerFromEnvWithSecretGetter(ctx context.Context, getSecret brokerSecretGetter) (api.AgentProposalPublicationSubmitter, error) {
	baseURL := strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL"))
	fileValues := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE")),
	}
	secretARNs := agentProposalBrokerSecretARNs()
	fileCount, secretCount := countConfigured(fileValues), countConfigured(secretARNs)
	if baseURL == "" && fileCount == 0 && secretCount == 0 {
		return nil, nil
	}
	if baseURL == "" || (fileCount != 0 && secretCount != 0) || (fileCount != len(fileValues) && secretCount != len(secretARNs)) {
		return nil, errors.New("incomplete Agent proposal Broker configuration")
	}

	var certificate tls.Certificate
	var caPEM []byte
	if secretCount == len(secretARNs) {
		if getSecret == nil {
			return nil, errors.New("Agent proposal Broker Secret reader is unavailable")
		}
		certPEM, err := getSecret(ctx, secretARNs[0])
		if err != nil {
			return nil, err
		}
		keyPEM, err := getSecret(ctx, secretARNs[1])
		if err != nil {
			return nil, err
		}
		caPEM, err = getSecret(ctx, secretARNs[2])
		if err != nil {
			return nil, err
		}
		if len(certPEM) > 64<<10 || len(keyPEM) > 64<<10 || len(caPEM) > 256<<10 {
			return nil, errors.New("Agent proposal Broker TLS Secret exceeds size limit")
		}
		certificate, err = tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, errors.New("invalid Agent proposal Broker client certificate")
		}
	} else {
		var err error
		certificate, err = tls.LoadX509KeyPair(fileValues[0], fileValues[1])
		if err != nil {
			return nil, errors.New("invalid Agent proposal Broker client certificate")
		}
		caPEM, err = os.ReadFile(fileValues[2])
		if err != nil {
			return nil, errors.New("cannot read Agent proposal Broker CA bundle")
		}
	}

	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid Agent proposal Broker client certificate")
	}
	certificate.Leaf = leaf
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Agent proposal Broker CA bundle")
	}
	client, err := githubbroker.NewAgentProposalPublicationMTLSClient(certificate, roots)
	if err != nil {
		return nil, errors.New("invalid Agent proposal Broker client identity")
	}
	brokerClient, err := githubbroker.NewHTTPAgentProposalPublicationClient(baseURL, client)
	if err != nil {
		return nil, err
	}
	return agentProposalPublicationSubmitterAdapter{client: brokerClient}, nil
}

func agentProposalBrokerSecretARNs() []string {
	return []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_SECRET_ARN")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_SECRET_ARN")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_SECRET_ARN")),
	}
}

func countConfigured(values []string) int {
	count := 0
	for _, value := range values {
		if value != "" {
			count++
		}
	}
	return count
}
