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
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/agenttask"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/store/workloads"
)

var ErrAgentTaskUnavailable = errors.New("Agent task unavailable")

const agentTaskRoute = "/v1/agent/task"
const agentTaskMaxResponse = 72 << 10

var agentTaskIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var agentTaskAttemptPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,127}$`)
var agentTaskDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

type AgentTaskIdentity struct {
	TenantID        domain.TenantID
	WorkloadRunID   domain.WorkloadRunID
	AttemptID       string
	Generation      int64
	ResourcePlaneID domain.ResourcePlaneID
}

type AgentTaskResolver func(context.Context, string, AgentTaskIdentity) (workloads.AgentTaskRecord, error)

type AgentTaskService struct {
	resolve AgentTaskResolver
	allowed map[string]struct{}
}

func NewAgentTaskService(resourcePlaneIDs []string, resolve AgentTaskResolver) (*AgentTaskService, error) {
	if resolve == nil || len(resourcePlaneIDs) == 0 {
		return nil, ErrUnavailable
	}
	allowed := make(map[string]struct{}, len(resourcePlaneIDs))
	for _, id := range resourcePlaneIDs {
		if !resourcePlaneIDPattern.MatchString(id) {
			return nil, ErrUnavailable
		}
		if _, duplicate := allowed[id]; duplicate {
			return nil, ErrUnavailable
		}
		allowed[id] = struct{}{}
	}
	return &AgentTaskService{resolve: resolve, allowed: allowed}, nil
}

func (s *AgentTaskService) Resolve(ctx context.Context, caller string, identity AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
	if s == nil || ctx == nil || !validAgentTaskIdentity(identity) || !strings.HasPrefix(caller, resourcePlaneURIPrefix) {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	planeID := strings.TrimPrefix(caller, resourcePlaneURIPrefix)
	validatedCaller, err := ResourcePlaneCallerID(planeID)
	if err != nil || validatedCaller != caller || domain.ResourcePlaneID(planeID) != identity.ResourcePlaneID {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	if _, ok := s.allowed[planeID]; !ok {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	task, err := s.resolve(ctx, caller, identity)
	if err != nil || !validResolvedAgentTask(task, identity) {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	return task, nil
}

func validAgentTaskIdentity(identity AgentTaskIdentity) bool {
	return agentTaskIDPattern.MatchString(string(identity.TenantID)) && agentTaskIDPattern.MatchString(string(identity.WorkloadRunID)) &&
		agentTaskAttemptPattern.MatchString(identity.AttemptID) && identity.Generation > 0 && resourcePlaneIDPattern.MatchString(string(identity.ResourcePlaneID))
}

func validResolvedAgentTask(task workloads.AgentTaskRecord, identity AgentTaskIdentity) bool {
	return task.RequestedBy != "" && validAgentTaskPayload(task, identity)
}

func validAgentTaskPayload(task workloads.AgentTaskRecord, identity AgentTaskIdentity) bool {
	return task.TenantID == identity.TenantID && task.WorkloadRunID == identity.WorkloadRunID && task.IntegrationID != "" &&
		task.DeliveryID != "" && task.InstallationID > 0 && task.RepositoryID > 0 && task.IssueNumber > 0 && task.CommentID > 0 &&
		task.DefaultBranch != "" && len(task.DefaultBranch) <= 255 && len(task.Title) > 0 && len(task.Title) <= 1024 && len(task.Body) <= 64<<10 &&
		agentIssueCommitPattern.MatchString(task.CommitSHA) && agentTaskDigestPattern.MatchString(task.ContextSHA256) &&
		task.ContextSHA256 == agenttask.SHA256(agenttask.Context{TenantID: string(task.TenantID), WorkloadRunID: string(task.WorkloadRunID),
			IntegrationID: task.IntegrationID, DeliveryID: task.DeliveryID, InstallationID: task.InstallationID,
			RepositoryID: task.RepositoryID, IssueNumber: task.IssueNumber, CommentID: task.CommentID,
			DefaultBranch: task.DefaultBranch, CommitSHA: task.CommitSHA, Title: task.Title, Body: task.Body}) && !task.CreatedAt.IsZero()
}

// NewPostgresAgentTaskResolver returns the Issue context only for the active,
// latest Agent attempt on the calling Resource Plane. Guest MicroVMs have no
// broker certificate; this endpoint is for the trusted Resource Plane worker.
func NewPostgresAgentTaskResolver(pool transaction.Beginner, policy transaction.Policy, now func() time.Time) AgentTaskResolver {
	if now == nil {
		now = time.Now
	}
	return func(ctx context.Context, caller string, identity AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
		planeID := strings.TrimPrefix(caller, resourcePlaneURIPrefix)
		validatedCaller, err := ResourcePlaneCallerID(planeID)
		if err != nil || validatedCaller != caller || domain.ResourcePlaneID(planeID) != identity.ResourcePlaneID || pool == nil {
			return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
		}
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (workloads.AgentTaskRecord, error) {
			var storedPlane domain.ResourcePlaneID
			var commitSHA, triggerBranch, integrationID string
			var installationID, repositoryID int64
			err := tx.QueryRow(ctx, `SELECT wr.resource_plane_id,wr.commit_sha,wr.trigger_branch,
				COALESCE(wr.github_integration_id,''),COALESCE(wr.github_installation_id,0),COALESCE(wr.github_repository_id,0)
				FROM workload_runs wr JOIN workload_run_attempts a
				  ON a.tenant_id=wr.tenant_id AND a.workload_run_id=wr.id
				JOIN workload_agent_tasks task ON task.tenant_id=wr.tenant_id AND task.workload_run_id=wr.id
				JOIN github_webhook_deliveries delivery ON delivery.integration_id=task.integration_id AND delivery.delivery_id=task.delivery_id
				JOIN tenant_github_integrations integration ON integration.tenant_id=wr.tenant_id AND integration.id=wr.github_integration_id
				JOIN github_app_registrations registration ON registration.id=integration.app_registration_id AND registration.github_app_id=integration.github_app_id
				JOIN github_credential_bindings credential ON credential.tenant_id=integration.tenant_id AND credential.id=integration.credential_binding_id
				  AND credential.github_app_id=integration.github_app_id
				JOIN tenant_github_installations installation ON installation.tenant_id=wr.tenant_id AND installation.installation_id=wr.github_installation_id
				JOIN github_app_installation_bindings shared_binding ON shared_binding.tenant_id=wr.tenant_id AND shared_binding.installation_id=wr.github_installation_id
				JOIN tenant_github_app_installation_bindings app_binding ON app_binding.tenant_id=wr.tenant_id AND app_binding.integration_id=integration.id
				  AND app_binding.installation_id=wr.github_installation_id
				JOIN tenant_github_repositories repository ON repository.tenant_id=wr.tenant_id AND repository.installation_id=wr.github_installation_id
				  AND repository.github_repository_id=wr.github_repository_id AND repository.factory_enabled=true
			JOIN tenant_members membership ON membership.tenant_id=wr.tenant_id AND membership.user_id=task.requested_by
				WHERE wr.tenant_id=$1 AND wr.id=$2 AND a.id=$3 AND a.generation=$4
				  AND wr.kind='agent' AND wr.trigger_event='issue_comment' AND wr.desired_state='running'
				  AND wr.state IN ('provisioning','running') AND a.state IN ('dispatch_pending','accepted','running')
				  AND wr.timeout_at>$5 AND wr.resource_plane_id=$6 AND a.resource_plane_id=$6
				  AND wr.github_integration_id=task.integration_id AND wr.github_installation_id=task.installation_id
				  AND wr.github_repository_id=task.repository_id AND wr.commit_sha=task.commit_sha AND wr.trigger_branch=task.default_branch
				  AND integration.status='active' AND integration.config_revision=wr.github_config_revision
				  AND integration.credential_generation=wr.github_credential_generation
				  AND registration.mode='tenant_owned' AND registration.owner_tenant_id=wr.tenant_id
				  AND credential.status='approved' AND installation.status='active' AND installation.suspended_at IS NULL
			  AND repository.factory_enabled_at <= delivery.received_at AND delivery.processing_status='processed'
				  AND NOT EXISTS (SELECT 1 FROM workload_run_attempts newer WHERE newer.tenant_id=a.tenant_id
				    AND newer.workload_run_id=a.workload_run_id AND newer.generation>a.generation)`,
				identity.TenantID, identity.WorkloadRunID, identity.AttemptID, identity.Generation, now().UTC(), identity.ResourcePlaneID).
				Scan(&storedPlane, &commitSHA, &triggerBranch, &integrationID, &installationID, &repositoryID)
			if err != nil || storedPlane != identity.ResourcePlaneID {
				return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
			}
			task, err := workloads.GetAgentTask(ctx, tx, identity.TenantID, identity.WorkloadRunID)
			if err != nil || !validResolvedAgentTask(task, identity) || task.CommitSHA != commitSHA || task.DefaultBranch != triggerBranch ||
				task.IntegrationID != integrationID || task.InstallationID != installationID || task.RepositoryID != repositoryID {
				return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
			}
			return task, nil
		})
	}
}

type agentTaskRequest struct {
	TenantID        domain.TenantID        `json:"tenant_id"`
	WorkloadRunID   domain.WorkloadRunID   `json:"workload_run_id"`
	AttemptID       string                 `json:"attempt_id"`
	Generation      int64                  `json:"generation"`
	ResourcePlaneID domain.ResourcePlaneID `json:"resource_plane_id"`
}

// WithAgentTask exposes an mTLS-only read path to the Resource Plane worker.
// It returns bounded Issue context without caching and without logging input.
func WithAgentTask(next http.Handler, service *AgentTaskService) http.Handler {
	if next == nil {
		next = http.NotFoundHandler()
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != agentTaskRoute {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			writeError(w, http.StatusMethodNotAllowed)
			return
		}
		caller, err := authenticatedResourcePlaneCaller(r)
		if err != nil {
			writeError(w, http.StatusUnauthorized)
			return
		}
		if service == nil || r.Body == nil || r.ContentLength > 2048 || !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeError(w, http.StatusBadRequest)
			return
		}
		data, err := io.ReadAll(io.LimitReader(r.Body, 2049))
		if err != nil || len(data) > 2048 {
			writeError(w, http.StatusBadRequest)
			return
		}
		defer clear(data)
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.DisallowUnknownFields()
		var input agentTaskRequest
		if decoder.Decode(&input) != nil || !validAgentTaskIdentity(agentTaskIdentity(input)) {
			writeError(w, http.StatusBadRequest)
			return
		}
		var trailing any
		if decoder.Decode(&trailing) != io.EOF {
			writeError(w, http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		task, err := service.Resolve(ctx, caller, agentTaskIdentity(input))
		if err != nil {
			writeError(w, http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(task)
	})
}

type HTTPAgentTaskClient struct {
	base   string
	client *http.Client
}

func NewHTTPAgentTaskClient(baseURL, resourcePlaneID string, certificate tls.Certificate, brokerRoots *x509.CertPool) (*HTTPAgentTaskClient, error) {
	if _, err := ResourcePlaneCallerID(resourcePlaneID); err != nil {
		return nil, err
	}
	client, err := newMTLSClient(certificate, brokerRoots, 6*time.Second, resourcePlaneURIPrefix+resourcePlaneID)
	if err != nil {
		return nil, err
	}
	base, err := NewHTTPResolver(baseURL, client)
	if err != nil {
		return nil, err
	}
	return &HTTPAgentTaskClient{base: strings.TrimRight(base.BaseURL, "/"), client: client}, nil
}

func (c *HTTPAgentTaskClient) Resolve(ctx context.Context, identity AgentTaskIdentity) (workloads.AgentTaskRecord, error) {
	if c == nil || c.client == nil || ctx == nil || !validAgentTaskIdentity(identity) {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	body, err := json.Marshal(newAgentTaskRequest(identity))
	if err != nil {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	defer clear(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+agentTaskRoute, bytes.NewReader(body))
	if err != nil {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	req.Header.Set("Content-Type", "application/json")
	response, err := c.client.Do(req)
	if err != nil {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK || response.Header.Get("Cache-Control") != "no-store" {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, agentTaskMaxResponse+1))
	if err != nil || len(data) == 0 || len(data) > agentTaskMaxResponse {
		clear(data)
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	defer clear(data)
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	var task workloads.AgentTaskRecord
	if decoder.Decode(&task) != nil || !validAgentTaskPayload(task, identity) {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	var trailing any
	if decoder.Decode(&trailing) != io.EOF {
		return workloads.AgentTaskRecord{}, ErrAgentTaskUnavailable
	}
	return task, nil
}

func newAgentTaskRequest(identity AgentTaskIdentity) agentTaskRequest {
	return agentTaskRequest{TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID, AttemptID: identity.AttemptID,
		Generation: identity.Generation, ResourcePlaneID: identity.ResourcePlaneID}
}

func agentTaskIdentity(input agentTaskRequest) AgentTaskIdentity {
	return AgentTaskIdentity{TenantID: input.TenantID, WorkloadRunID: input.WorkloadRunID, AttemptID: input.AttemptID,
		Generation: input.Generation, ResourcePlaneID: input.ResourcePlaneID}
}

func NewResourcePlaneAgentTaskMTLSClient(resourcePlaneID string, certificate tls.Certificate, brokerRoots *x509.CertPool) (*http.Client, error) {
	caller, err := ResourcePlaneCallerID(resourcePlaneID)
	if err != nil {
		return nil, err
	}
	return newMTLSClient(certificate, brokerRoots, 6*time.Second, caller)
}

func authenticatedResourcePlaneCaller(r *http.Request) (string, error) {
	if r == nil || r.TLS == nil || len(r.TLS.PeerCertificates) == 0 || len(r.TLS.VerifiedChains) == 0 ||
		len(r.TLS.PeerCertificates[0].URIs) != 1 {
		return "", ErrUnavailable
	}
	caller := r.TLS.PeerCertificates[0].URIs[0].String()
	if !strings.HasPrefix(caller, resourcePlaneURIPrefix) {
		return "", ErrUnavailable
	}
	if _, err := ResourcePlaneCallerID(strings.TrimPrefix(caller, resourcePlaneURIPrefix)); err != nil {
		return "", ErrUnavailable
	}
	return caller, nil
}
