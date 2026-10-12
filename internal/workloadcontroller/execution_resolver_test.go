package workloadcontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type fixedExecutionMetadataResolver struct {
	metadata ControlPlaneExecutionMetadata
	err      error
	calls    int
}

func (r *fixedExecutionMetadataResolver) Resolve(context.Context, workloadjournal.Command) (ControlPlaneExecutionMetadata, error) {
	r.calls++
	return r.metadata, r.err
}

type fixedSourceMaterializer struct {
	source MaterializedSource
	err    error
	calls  int
}

func (m *fixedSourceMaterializer) Materialize(context.Context, ControlPlaneExecutionMetadata) (MaterializedSource, error) {
	m.calls++
	return m.source, m.err
}

func TestMetadataSpecificationResolverCombinesTrustedMetadataAndPlaneProfile(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	metadata := ControlPlaneExecutionMetadata{SchemaVersion: 1, TenantID: domain.TenantID(id.TenantID), WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID),
		AttemptID: id.AttemptID, Generation: id.Generation, ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID),
		GitHubInstallationID: 71, GitHubRepositoryID: 81, GitHubOwner: "acme", GitHubRepository: "api",
		CommitSHA: strings.Repeat("a", 40), Ref: "refs/heads/main", RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		RequestedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	metaResolver := &fixedExecutionMetadataResolver{metadata: metadata}
	sources := &fixedSourceMaterializer{source: MaterializedSource{Digest: strings.Repeat("b", 64),
		ArtifactPrefix: "s3://hako-artifacts/workloads/tenant_1/run_1/attempt_1/"}}
	profile := RuntimeProfile{ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako", ImageVersion: "7",
		ImageDigest: strings.Repeat("c", 64), IngressNetworkConnectors: []string{"ingress"}, EgressNetworkConnectors: []string{"egress"}}
	resolver, err := NewMetadataSpecificationResolver(MetadataSpecificationResolverConfig{ResourcePlaneID: "rp_1", Metadata: metaResolver,
		Sources: sources, Profiles: map[RuntimeProfileKey]RuntimeProfile{{RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted}: profile}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	profile.EgressNetworkConnectors[0] = "mutated"
	spec, err := resolver.Resolve(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if spec.Identity != id || spec.CommitSHA != metadata.CommitSHA || !spec.RequestedAt.Equal(metadata.RequestedAt) || !spec.Deadline.Equal(metadata.Deadline) ||
		spec.SourceDigest != sources.source.Digest || spec.ArtifactPrefix != sources.source.ArtifactPrefix || spec.ImageDigest != strings.Repeat("c", 64) ||
		spec.EgressNetworkConnectors[0] != "egress" || metaResolver.calls != 1 || sources.calls != 1 {
		t.Fatalf("unexpected resolved specification: %+v", spec)
	}
}

func TestMetadataSpecificationResolverFailsClosedBeforeSourceMaterialization(t *testing.T) {
	now := time.Now().UTC()
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	metadata := ControlPlaneExecutionMetadata{SchemaVersion: 1, TenantID: domain.TenantID(id.TenantID), WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID),
		AttemptID: id.AttemptID, Generation: id.Generation, ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID),
		GitHubInstallationID: 71, GitHubRepositoryID: 81, GitHubOwner: "acme", GitHubRepository: "api",
		CommitSHA: strings.Repeat("a", 40), RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted,
		RequestedAt: now.Add(-time.Minute), Deadline: now.Add(time.Hour)}
	profile := RuntimeProfile{ImageIdentifier: "image", ImageVersion: "1", ImageDigest: strings.Repeat("c", 64),
		IngressNetworkConnectors: []string{"ingress"}, EgressNetworkConnectors: []string{"egress"}}
	validSource := MaterializedSource{Digest: strings.Repeat("b", 64), ArtifactPrefix: "s3://bucket/logs/tenant_1/run_1/attempt_1/"}
	for _, tc := range []struct {
		name     string
		id       workloadjournal.Command
		metadata ControlPlaneExecutionMetadata
		source   MaterializedSource
		metaErr  error
		srcErr   error
	}{
		{name: "foreign plane command", id: func() workloadjournal.Command { v := id; v.ResourcePlaneID = "rp_other"; return v }(), metadata: metadata, source: validSource},
		{name: "foreign metadata identity", id: id, metadata: func() ControlPlaneExecutionMetadata { v := metadata; v.TenantID = "tenant_other"; return v }(), source: validSource},
		{name: "unsupported runtime trust profile", id: id, metadata: func() ControlPlaneExecutionMetadata { v := metadata; v.TrustLevel = domain.WorkloadTrusted; return v }(), source: validSource},
		{name: "wrong attempt artifact prefix", id: id, metadata: metadata, source: MaterializedSource{Digest: strings.Repeat("b", 64), ArtifactPrefix: "s3://bucket/logs/tenant_1/run_1/attempt_other/"}},
		{name: "path traversal", id: id, metadata: metadata, source: MaterializedSource{Digest: strings.Repeat("b", 64), ArtifactPrefix: "s3://bucket/logs/tenant_1/../run_1/attempt_1/"}},
		{name: "metadata unavailable", id: id, metadata: metadata, metaErr: ErrExecutionMetadata, source: validSource},
		{name: "source unavailable", id: id, metadata: metadata, source: validSource, srcErr: errors.New("private source error")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			metadataResolver := &fixedExecutionMetadataResolver{metadata: tc.metadata, err: tc.metaErr}
			sources := &fixedSourceMaterializer{source: tc.source, err: tc.srcErr}
			resolver, err := NewMetadataSpecificationResolver(MetadataSpecificationResolverConfig{ResourcePlaneID: "rp_1", Metadata: metadataResolver,
				Sources: sources, Profiles: map[RuntimeProfileKey]RuntimeProfile{{RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted}: profile}, Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := resolver.Resolve(context.Background(), tc.id); !errors.Is(err, ErrExecutionMetadata) && !errors.Is(err, ErrExecutionMaterialization) {
				t.Fatalf("invalid trusted specification error = %v", err)
			}
			expectedMetadataCalls := 1
			if tc.name == "foreign plane command" {
				expectedMetadataCalls = 0
			}
			expectedSourceCalls := 1
			if tc.name == "foreign plane command" || tc.name == "foreign metadata identity" || tc.name == "unsupported runtime trust profile" || tc.name == "metadata unavailable" {
				expectedSourceCalls = 0
			}
			if metadataResolver.calls != expectedMetadataCalls || sources.calls != expectedSourceCalls {
				t.Fatalf("unexpected resolver call sequence: metadata=%d source=%d", metadataResolver.calls, sources.calls)
			}
		})
	}
}

func TestNewMetadataSpecificationResolverRejectsIncompleteProfile(t *testing.T) {
	_, err := NewMetadataSpecificationResolver(MetadataSpecificationResolverConfig{ResourcePlaneID: "rp_1", Metadata: &fixedExecutionMetadataResolver{},
		Sources: &fixedSourceMaterializer{}, Profiles: map[RuntimeProfileKey]RuntimeProfile{{RuntimeClass: "standard", TrustLevel: domain.WorkloadUntrusted}: {
			ImageIdentifier: "image", ImageVersion: "1", ImageDigest: strings.Repeat("a", 64), EgressNetworkConnectors: []string{"egress"}}}})
	if !errors.Is(err, ErrExecutionMaterialization) {
		t.Fatalf("incomplete runtime profile error = %v", err)
	}
}
