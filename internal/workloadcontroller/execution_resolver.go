package workloadcontroller

import (
	"context"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrExecutionMaterialization = errors.New("trusted Workload source or runtime profile is unavailable")

// ExecutionMetadataResolver returns Control Plane-authorized immutable Run
// metadata. Implementations must not source execution parameters from a queue
// message beyond its identity.
type ExecutionMetadataResolver interface {
	Resolve(context.Context, workloadjournal.Command) (ControlPlaneExecutionMetadata, error)
}

// SourceMaterializer checks out the exact trusted commit and makes it
// available to the one MicroVM attempt. It must never run repository code and
// must be idempotent for a command identity because launch reconciliation
// resolves the same specification again after uncertain failures. SourceDigest
// covers the staged source representation consumed by the Agent.
type SourceMaterializer interface {
	Materialize(context.Context, ControlPlaneExecutionMetadata) (MaterializedSource, error)
}

type MaterializedSource = workloadsource.MaterializedSource

type RuntimeProfileKey struct {
	RuntimeClass string
	TrustLevel   domain.WorkloadTrustLevel
}

// RuntimeProfile is Resource Plane-owned configuration. It is deliberately
// not returned by the Control Plane metadata API.
type RuntimeProfile struct {
	ImageIdentifier          string
	ImageVersion             string
	ImageDigest              string
	IngressNetworkConnectors []string
	EgressNetworkConnectors  []string
}

type MetadataSpecificationResolverConfig struct {
	ResourcePlaneID domain.ResourcePlaneID
	Metadata        ExecutionMetadataResolver
	Sources         SourceMaterializer
	AgentTasks      AgentTaskProvider
	Profiles        map[RuntimeProfileKey]RuntimeProfile
	Now             func() time.Time
}

type MetadataSpecificationResolver struct {
	planeID    domain.ResourcePlaneID
	metadata   ExecutionMetadataResolver
	sources    SourceMaterializer
	agentTasks AgentTaskProvider
	profiles   map[RuntimeProfileKey]RuntimeProfile
	now        func() time.Time
}

func NewMetadataSpecificationResolver(config MetadataSpecificationResolverConfig) (*MetadataSpecificationResolver, error) {
	if config.ResourcePlaneID == "" || config.Metadata == nil || config.Sources == nil || len(config.Profiles) == 0 {
		return nil, ErrExecutionMaterialization
	}
	profiles := make(map[RuntimeProfileKey]RuntimeProfile, len(config.Profiles))
	for key, profile := range config.Profiles {
		if key.RuntimeClass == "" || key.TrustLevel == "" || profile.ImageIdentifier == "" || profile.ImageVersion == "" ||
			!digestPattern.MatchString(profile.ImageDigest) || len(profile.IngressNetworkConnectors) != 1 ||
			len(profile.EgressNetworkConnectors) != 1 || profile.IngressNetworkConnectors[0] == "" || profile.EgressNetworkConnectors[0] == "" {
			return nil, ErrExecutionMaterialization
		}
		profile.IngressNetworkConnectors = append([]string(nil), profile.IngressNetworkConnectors...)
		profile.EgressNetworkConnectors = append([]string(nil), profile.EgressNetworkConnectors...)
		profiles[key] = profile
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	return &MetadataSpecificationResolver{planeID: config.ResourcePlaneID, metadata: config.Metadata, sources: config.Sources,
		agentTasks: config.AgentTasks, profiles: profiles, now: now}, nil
}

func (r *MetadataSpecificationResolver) Resolve(ctx context.Context, id workloadjournal.Command) (Specification, error) {
	if r == nil || ctx == nil || id.ResourcePlaneID != string(r.planeID) {
		return Specification{}, ErrExecutionMetadata
	}
	metadata, err := r.metadata.Resolve(ctx, id)
	if err != nil || !metadata.matches(id) || !validExecutionMetadata(metadata, r.now().UTC()) {
		return Specification{}, ErrExecutionMetadata
	}
	profile, ok := r.profiles[RuntimeProfileKey{RuntimeClass: metadata.RuntimeClass, TrustLevel: metadata.TrustLevel}]
	if !ok {
		return Specification{}, ErrExecutionMaterialization
	}
	agentTaskDigest := ""
	if effectiveWorkloadKind(metadata) == domain.WorkloadAgent {
		if r.agentTasks == nil {
			return Specification{}, ErrExecutionMaterialization
		}
		task, taskErr := r.agentTasks.ResolveAgentTask(ctx, id)
		if taskErr != nil || !workloadagent.ValidateAgentTask(task, workloadagent.IdentityFromCommand(id), metadata.CommitSHA) ||
			task.DefaultBranch != metadata.TriggerBranch || task.IntegrationID != metadata.GitHubIntegrationID ||
			task.InstallationID != metadata.GitHubInstallationID || task.RepositoryID != metadata.GitHubRepositoryID {
			return Specification{}, ErrExecutionMaterialization
		}
		agentTaskDigest = task.ContextSHA256
	}
	source, err := r.sources.Materialize(ctx, metadata)
	if err != nil || !digestPattern.MatchString(source.Digest) || !validArtifactPrefix(source.ArtifactPrefix, id) {
		return Specification{}, ErrExecutionMaterialization
	}
	spec := Specification{Identity: id, ImageDigest: profile.ImageDigest, SourceDigest: source.Digest,
		Kind:      effectiveWorkloadKind(metadata),
		CommitSHA: metadata.CommitSHA, TriggerEvent: metadata.TriggerEvent, TriggerBranch: metadata.TriggerBranch, CorrelationID: metadata.CorrelationID,
		RequestedAt: metadata.RequestedAt.UTC(), Deadline: metadata.Deadline.UTC(),
		ArtifactPrefix: source.ArtifactPrefix, ImageIdentifier: profile.ImageIdentifier, ImageVersion: profile.ImageVersion,
		IngressNetworkConnectors: append([]string(nil), profile.IngressNetworkConnectors...),
		EgressNetworkConnectors:  append([]string(nil), profile.EgressNetworkConnectors...)}
	if spec.Kind == domain.WorkloadAgent {
		spec.GitHubIntegrationID = metadata.GitHubIntegrationID
		spec.GitHubInstallationID = metadata.GitHubInstallationID
		spec.GitHubRepositoryID = metadata.GitHubRepositoryID
		spec.AgentTaskContextSHA256 = agentTaskDigest
	}
	if _, err := specificationHash(spec, id); err != nil {
		return Specification{}, ErrExecutionMaterialization
	}
	return spec, nil
}

func validArtifactPrefix(value string, id workloadjournal.Command) bool {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme != "s3" || parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		!strings.HasSuffix(parsed.Path, "/") || strings.Contains(parsed.Path, "\\") {
		return false
	}
	for _, part := range strings.Split(parsed.Path, "/") {
		if part == "." || part == ".." {
			return false
		}
	}
	wantSuffix := "/" + string(id.TenantID) + "/" + string(id.WorkloadRunID) + "/" + id.AttemptID + "/"
	return strings.HasSuffix(parsed.Path, wantSuffix)
}
