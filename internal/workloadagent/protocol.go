// Package workloadagent defines the private, versioned protocol used by the
// Resource Plane controller to operate one Lambda MicroVM Workload attempt.
package workloadagent

import (
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const ProtocolVersion = "1"

var agentTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var agentTaskCommitPattern = regexp.MustCompile(`^(?:[a-f0-9]{40}|[a-f0-9]{64})$`)
var agentTaskDigestPattern = regexp.MustCompile(`^[a-f0-9]{64}$`)

type Identity struct {
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
}

func IdentityFromCommand(id workloadjournal.Command) Identity {
	return Identity{TenantID: id.TenantID, WorkloadRunID: id.WorkloadRunID, AttemptID: id.AttemptID,
		Generation: id.Generation, ResourcePlaneID: id.ResourcePlaneID, IdempotencyToken: id.IdempotencyToken}
}

func (i Identity) Command() workloadjournal.Command {
	return workloadjournal.Command{TenantID: i.TenantID, WorkloadRunID: i.WorkloadRunID, AttemptID: i.AttemptID,
		Generation: i.Generation, ResourcePlaneID: i.ResourcePlaneID, IdempotencyToken: i.IdempotencyToken}
}

type StartRequest struct {
	Identity             Identity            `json:"identity"`
	Kind                 domain.WorkloadKind `json:"kind,omitempty"`
	AgentTask            *AgentTask          `json:"agent_task,omitempty"`
	AgentModelCredential []byte              `json:"agent_model_credential,omitempty"`
	SourceDigest         string              `json:"source_digest"`
	CommitSHA            string              `json:"commit_sha"`
	Deadline             time.Time           `json:"deadline"`
}

func (StartRequest) String() string   { return "[REDACTED Workload Agent start request]" }
func (StartRequest) GoString() string { return "[REDACTED Workload Agent start request]" }

const maxAgentModelCredentialBytes = 8 << 10

func bytesContainNUL(value []byte) bool {
	for _, b := range value {
		if b == 0 || b == '\r' || b == '\n' {
			return true
		}
	}
	return false
}

// AgentTask contains untrusted Issue text for an explicitly admitted Agent
// Run. It is carried only over the private MicroVM Agent protocol and must not
// be logged or returned by user-facing Run APIs.
type AgentTask struct {
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
	ContextSHA256  string `json:"context_sha256"`
}

func ValidateAgentTask(task AgentTask, identity Identity, commitSHA string) bool {
	if !agentTaskIDPattern.MatchString(task.TenantID) || !agentTaskIDPattern.MatchString(task.WorkloadRunID) ||
		!agentTaskIDPattern.MatchString(task.IntegrationID) || len(task.DeliveryID) < 16 || len(task.DeliveryID) > 128 ||
		task.InstallationID <= 0 || task.RepositoryID <= 0 || task.IssueNumber <= 0 || task.CommentID <= 0 ||
		task.DefaultBranch == "" || len(task.DefaultBranch) > 255 || strings.ContainsAny(task.DefaultBranch, "\\\r\n\x00") ||
		!agentTaskCommitPattern.MatchString(task.CommitSHA) || task.CommitSHA != commitSHA ||
		task.Title == "" || len(task.Title) > 1024 || len(task.Body) > 64<<10 || !agentTaskDigestPattern.MatchString(task.ContextSHA256) ||
		task.ContextSHA256 != AgentTaskContextSHA256(task) || task.TenantID != identity.TenantID || task.WorkloadRunID != identity.WorkloadRunID {
		return false
	}
	for _, char := range task.DefaultBranch {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}

// AgentTaskContextSHA256 hashes every immutable field delivered by the Broker,
// excluding only the hash itself. JSON struct field order provides a stable
// encoding across the Control Plane, Worker, and guest.
func AgentTaskContextSHA256(task AgentTask) string {
	return agenttask.SHA256(agenttask.Context{TenantID: task.TenantID, WorkloadRunID: task.WorkloadRunID,
		IntegrationID: task.IntegrationID, DeliveryID: task.DeliveryID, InstallationID: task.InstallationID,
		RepositoryID: task.RepositoryID, IssueNumber: task.IssueNumber, CommentID: task.CommentID,
		DefaultBranch: task.DefaultBranch, CommitSHA: task.CommitSHA, Title: task.Title, Body: task.Body})
}

type StatusResponse struct {
	Identity Identity                       `json:"identity"`
	State    workloadjournal.ExecutionState `json:"state"`
}

type ResultResponse struct {
	Identity Identity                    `json:"identity"`
	Result   workloadjobs.WorkloadResult `json:"result"`
}
