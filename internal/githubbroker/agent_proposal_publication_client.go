package githubbroker

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/workloads"
)

type HTTPAgentProposalPublicationClient struct {
	baseURL string
	client  *http.Client
}

func NewHTTPAgentProposalPublicationClient(baseURL string, client *http.Client) (*HTTPAgentProposalPublicationClient, error) {
	resolver, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &HTTPAgentProposalPublicationClient{baseURL: resolver.BaseURL, client: resolver.Client}, nil
}

func NewAgentProposalPublicationMTLSClient(certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	return newMTLSClient(certificate, brokerRoots, 55*time.Second, controlPlaneURI)
}

func (c *HTTPAgentProposalPublicationClient) Publish(ctx context.Context, request AgentProposalPublicationRequest) (AgentProposalPublicationResponse, error) {
	if c == nil || c.client == nil || ctx == nil || request.TenantID == "" || !validCheckoutRunID(request.WorkloadRunID) ||
		!validCheckoutRunID(domain.WorkloadRunID(request.AgentAttemptID)) {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	body, err := json.Marshal(request)
	if err != nil {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+agentProposalPublishRoute, bytes.NewReader(body))
	if err != nil {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 4097))
	if err != nil || len(data) == 0 || len(data) > 4096 {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var result AgentProposalPublicationResponse
	if decoder.Decode(&result) != nil || result.TenantID != request.TenantID || result.WorkloadRunID != request.WorkloadRunID ||
		!validBrokerBranch(result.DefaultBranch) || !validBrokerBranch(result.BranchName) || (result.State == workloads.AgentProposalPublicationBranchCreated &&
		(!agentProposalCommitSHA.MatchString(result.BranchCommitSHA) || result.PullRequestNumber != 0)) || (result.State == workloads.AgentProposalPublicationPullRequestCreated &&
		(!agentProposalCommitSHA.MatchString(result.BranchCommitSHA) || result.PullRequestNumber < 1)) ||
		(result.State != workloads.AgentProposalPublicationBranchCreated && result.State != workloads.AgentProposalPublicationPullRequestCreated) {
		return AgentProposalPublicationResponse{}, ErrAgentProposalPublicationDenied
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return AgentProposalPublicationResponse{}, errors.New("invalid Agent proposal publication response")
	}
	return result, nil
}
