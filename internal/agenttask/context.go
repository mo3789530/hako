// Package agenttask defines the canonical digest for an immutable Issue-to-Agent
// task. The Control Plane, Broker, Worker, and guest share this representation.
package agenttask

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type Context struct {
	TenantID       string `json:"tenant_id"`
	WorkloadRunID  string `json:"workload_run_id"`
	IntegrationID  string `json:"integration_id"`
	DeliveryID     string `json:"delivery_id"`
	InstallationID int64  `json:"installation_id"`
	RepositoryID   int64  `json:"repository_id"`
	IssueNumber    int64  `json:"issue_number"`
	CommentID      int64  `json:"comment_id"`
	DefaultBranch  string `json:"default_branch"`
	CommitSHA      string `json:"commit_sha"`
	Title          string `json:"title"`
	Body           string `json:"body"`
}

func SHA256(value Context) string {
	payload, _ := json.Marshal(value)
	hash := sha256.Sum256(payload)
	return hex.EncodeToString(hash[:])
}
