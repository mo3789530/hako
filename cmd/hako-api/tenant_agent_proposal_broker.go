package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"os"
	"strings"

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

func tenantAgentProposalBrokerFromEnv() (api.AgentProposalPublicationSubmitter, error) {
	values := []string{
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_URL")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_CERT_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_CLIENT_KEY_FILE")),
		strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_OAUTH_BROKER_CA_FILE")),
	}
	configured := 0
	for _, value := range values {
		if value != "" {
			configured++
		}
	}
	if configured == 0 {
		return nil, nil
	}
	if configured != len(values) {
		return nil, errors.New("incomplete Agent proposal Broker configuration")
	}
	certificate, err := tls.LoadX509KeyPair(values[1], values[2])
	if err != nil {
		return nil, errors.New("invalid Agent proposal Broker client certificate")
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return nil, errors.New("invalid Agent proposal Broker client certificate")
	}
	certificate.Leaf = leaf
	caPEM, err := os.ReadFile(values[3])
	if err != nil {
		return nil, errors.New("cannot read Agent proposal Broker CA bundle")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid Agent proposal Broker CA bundle")
	}
	client, err := githubbroker.NewAgentProposalPublicationMTLSClient(certificate, roots)
	if err != nil {
		return nil, errors.New("invalid Agent proposal Broker client identity")
	}
	brokerClient, err := githubbroker.NewHTTPAgentProposalPublicationClient(values[0], client)
	if err != nil {
		return nil, err
	}
	return agentProposalPublicationSubmitterAdapter{client: brokerClient}, nil
}
