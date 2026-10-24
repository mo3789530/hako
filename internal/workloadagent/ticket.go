package workloadagent

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const AgentTicketMaxTTL = 5 * time.Minute

var (
	ErrAgentTicket          = errors.New("invalid Hako Agent credential")
	keyIDPattern            = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	agentIntegrationPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
	correlationIDPattern    = regexp.MustCompile(`^req_[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

type Operation string

const (
	OperationStart  Operation = "start"
	OperationSource Operation = "source"
	OperationStatus Operation = "status"
	OperationResult Operation = "result"
	OperationLogs   Operation = "logs"
)

func validOperation(op Operation) bool {
	return op == OperationStart || op == OperationSource || op == OperationStatus || op == OperationResult || op == OperationLogs
}

type TicketIssuer interface {
	Issue(microVMID string, identity workloadjournal.Command, operation Operation) (string, error)
	IssueExecution(microVMID string, identity workloadjournal.Command, operation Operation, execution ExecutionBinding) (string, error)
}

// ExecutionBinding is signed into Start and Source tickets so request-body or
// upload-header changes cannot alter the attempt's immutable execution input.
type ExecutionBinding struct {
	Kind                   domain.WorkloadKind `json:"kind,omitempty"`
	AgentIntegrationID     string              `json:"agent_integration_id,omitempty"`
	AgentInstallationID    int64               `json:"agent_installation_id,omitempty"`
	AgentRepositoryID      int64               `json:"agent_repository_id,omitempty"`
	AgentTaskContextSHA256 string              `json:"agent_task_context_sha256,omitempty"`
	SourceDigest           string              `json:"source_digest"`
	CommitSHA              string              `json:"commit_sha"`
	TriggerEvent           string              `json:"trigger_event,omitempty"`
	TriggerBranch          string              `json:"trigger_branch,omitempty"`
	CorrelationID          string              `json:"correlation_id,omitempty"`
	JobMemoryLimitMB       int                 `json:"job_memory_limit_mb,omitempty"`
	JobCPULimitMillis      int                 `json:"job_cpu_limit_millis,omitempty"`
	Deadline               time.Time           `json:"deadline"`
}

type TicketClaims struct {
	Version   int               `json:"version"`
	MicroVMID string            `json:"microvm_id"`
	Identity  Identity          `json:"identity"`
	Operation Operation         `json:"operation"`
	Execution *ExecutionBinding `json:"execution,omitempty"`
	IssuedAt  int64             `json:"issued_at"`
	ExpiresAt int64             `json:"expires_at"`
	Nonce     string            `json:"nonce"`
}

type Ed25519TicketIssuer struct {
	KeyID      string
	PrivateKey ed25519.PrivateKey
	TTL        time.Duration
	Now        func() time.Time
	Random     io.Reader
}

func (i *Ed25519TicketIssuer) Issue(microVMID string, identity workloadjournal.Command, operation Operation) (string, error) {
	if operation == OperationStart || operation == OperationSource {
		return "", ErrAgentTicket
	}
	return i.issue(microVMID, identity, operation, nil)
}

func (i *Ed25519TicketIssuer) IssueExecution(microVMID string, identity workloadjournal.Command, operation Operation, execution ExecutionBinding) (string, error) {
	if (operation != OperationStart && operation != OperationSource) || !validExecutionBinding(execution) {
		return "", ErrAgentTicket
	}
	return i.issue(microVMID, identity, operation, &execution)
}

func (i *Ed25519TicketIssuer) issue(microVMID string, identity workloadjournal.Command, operation Operation, execution *ExecutionBinding) (string, error) {
	if i == nil || !keyIDPattern.MatchString(i.KeyID) || len(i.PrivateKey) != ed25519.PrivateKeySize ||
		!protocolID.MatchString(microVMID) || !validIdentity(identity) || !validOperation(operation) ||
		((operation == OperationStart || operation == OperationSource) != (execution != nil)) {
		return "", ErrAgentTicket
	}
	ttl := i.TTL
	if ttl <= 0 || ttl > AgentTicketMaxTTL {
		ttl = AgentTicketMaxTTL
	} else if ttl < time.Minute {
		ttl = time.Minute
	}
	now := time.Now
	if i.Now != nil {
		now = i.Now
	}
	random := io.Reader(rand.Reader)
	if i.Random != nil {
		random = i.Random
	}
	var nonce [16]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", ErrAgentTicket
	}
	issued := now().UTC()
	if execution != nil && (!execution.Deadline.After(issued) || execution.Deadline.Sub(issued) > 8*time.Hour) {
		return "", ErrAgentTicket
	}
	version := 1
	if execution != nil && execution.Kind == domain.WorkloadAgent {
		version = 3
	} else if execution != nil && execution.TriggerEvent != "" {
		version = 2
	}
	claims := TicketClaims{Version: version, MicroVMID: microVMID, Identity: IdentityFromCommand(identity), Operation: operation, Execution: execution,
		IssuedAt: issued.Unix(), ExpiresAt: issued.Add(ttl).Unix(), Nonce: hex.EncodeToString(nonce[:])}
	payload, err := json.Marshal(claims)
	if err != nil || len(payload) > 4096 {
		return "", ErrAgentTicket
	}
	encoded := base64.RawURLEncoding.EncodeToString(payload)
	signed := i.KeyID + "." + encoded
	signature := ed25519.Sign(i.PrivateKey, []byte(signed))
	return signed + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func VerifyTicket(token string, keys map[string]ed25519.PublicKey, microVMID string, identity workloadjournal.Command, operation Operation, now time.Time) error {
	claims, err := VerifyTicketClaims(token, keys, identity, operation, now)
	if err != nil || claims.MicroVMID != microVMID {
		return ErrAgentTicket
	}
	return nil
}

func VerifyTicketClaims(token string, keys map[string]ed25519.PublicKey, identity workloadjournal.Command, operation Operation, now time.Time) (TicketClaims, error) {
	if len(token) == 0 || len(token) > 8192 || !validIdentity(identity) || !validOperation(operation) {
		return TicketClaims{}, ErrAgentTicket
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || !keyIDPattern.MatchString(parts[0]) {
		return TicketClaims{}, ErrAgentTicket
	}
	key := keys[parts[0]]
	if len(key) != ed25519.PublicKeySize {
		return TicketClaims{}, ErrAgentTicket
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || len(payload) == 0 || len(payload) > 4096 {
		return TicketClaims{}, ErrAgentTicket
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || !ed25519.Verify(key, []byte(parts[0]+"."+parts[1]), signature) {
		return TicketClaims{}, ErrAgentTicket
	}
	var claims TicketClaims
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&claims) != nil || decoder.Decode(new(any)) != io.EOF || (claims.Version != 1 && claims.Version != 2 && claims.Version != 3) || !protocolID.MatchString(claims.MicroVMID) ||
		claims.Identity.Command() != identity || claims.Operation != operation || !validOperation(claims.Operation) || len(claims.Nonce) != 32 ||
		((claims.Operation == OperationStart || claims.Operation == OperationSource) != (claims.Execution != nil)) {
		return TicketClaims{}, ErrAgentTicket
	}
	if claims.Execution != nil && !validExecutionBinding(*claims.Execution) {
		return TicketClaims{}, ErrAgentTicket
	}
	if claims.Version == 1 && claims.Execution != nil && (claims.Execution.TriggerEvent != "" || claims.Execution.Kind == domain.WorkloadAgent) ||
		claims.Version == 2 && (claims.Execution == nil || claims.Execution.TriggerEvent == "" || claims.Execution.Kind == domain.WorkloadAgent) ||
		claims.Version == 3 && (claims.Execution == nil || claims.Execution.Kind != domain.WorkloadAgent || claims.Execution.TriggerEvent != "issue_comment") {
		return TicketClaims{}, ErrAgentTicket
	}
	if nonce, err := hex.DecodeString(claims.Nonce); err != nil || len(nonce) != 16 {
		return TicketClaims{}, ErrAgentTicket
	}
	if now.IsZero() {
		now = time.Now()
	}
	issuedAt := time.Unix(claims.IssuedAt, 0)
	expiresAt := time.Unix(claims.ExpiresAt, 0)
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || expiresAt.Sub(issuedAt) > AgentTicketMaxTTL ||
		issuedAt.After(now.Add(30*time.Second)) || !now.Before(expiresAt) || now.Sub(issuedAt) > AgentTicketMaxTTL+30*time.Second {
		return TicketClaims{}, ErrAgentTicket
	}
	// IssuedAt is stored at Unix-second precision; allow that truncation when
	// validating an otherwise valid exact eight-hour execution deadline.
	if claims.Execution != nil && (!claims.Execution.Deadline.After(issuedAt) || claims.Execution.Deadline.Sub(issuedAt) > 8*time.Hour+time.Second) {
		return TicketClaims{}, ErrAgentTicket
	}
	return claims, nil
}

func validExecutionBinding(binding ExecutionBinding) bool {
	if binding.Kind != "" && binding.Kind != domain.WorkloadJob && binding.Kind != domain.WorkloadAgent ||
		(binding.Kind == domain.WorkloadAgent && (!agentIntegrationPattern.MatchString(binding.AgentIntegrationID) || binding.AgentInstallationID <= 0 || binding.AgentRepositoryID <= 0 || !protocolDigest.MatchString(binding.AgentTaskContextSHA256))) ||
		(binding.Kind != domain.WorkloadAgent && (binding.AgentIntegrationID != "" || binding.AgentInstallationID != 0 || binding.AgentRepositoryID != 0 || binding.AgentTaskContextSHA256 != "")) ||
		(binding.CorrelationID != "" && !correlationIDPattern.MatchString(binding.CorrelationID)) ||
		!protocolDigest.MatchString(binding.SourceDigest) || !protocolCommit.MatchString(binding.CommitSHA) || binding.Deadline.IsZero() || binding.Deadline.Location() != time.UTC {
		return false
	}
	if (binding.TriggerEvent == "") != (binding.TriggerBranch == "") ||
		(binding.Kind == domain.WorkloadAgent && binding.TriggerEvent != "issue_comment") ||
		(binding.Kind != domain.WorkloadAgent && binding.TriggerEvent != "" && binding.TriggerEvent != "push" && binding.TriggerEvent != "pull_request") ||
		(binding.Kind == domain.WorkloadAgent && binding.TriggerBranch == "") ||
		len(binding.TriggerBranch) > 255 ||
		strings.Contains(binding.TriggerBranch, "..") || strings.Contains(binding.TriggerBranch, "//") ||
		strings.HasSuffix(binding.TriggerBranch, "/") || strings.HasSuffix(binding.TriggerBranch, ".") || strings.Contains(binding.TriggerBranch, "@{") {
		return false
	}
	if (binding.JobMemoryLimitMB == 0) != (binding.JobCPULimitMillis == 0) ||
		(binding.JobMemoryLimitMB != 0 && (binding.Kind == domain.WorkloadAgent || binding.JobMemoryLimitMB < 256 || binding.JobMemoryLimitMB > 16384 ||
			binding.JobCPULimitMillis < 250 || binding.JobCPULimitMillis > 8000)) ||
		(binding.TriggerEvent != "" && binding.Kind != domain.WorkloadAgent && (binding.JobMemoryLimitMB == 0 || binding.JobCPULimitMillis == 0)) {
		return false
	}
	for _, char := range binding.TriggerBranch {
		if !((char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || strings.ContainsRune("._/-", char)) {
			return false
		}
	}
	return true
}
