package workloadcontroller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadagentcore"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

type agentCoreArtifactWriter struct {
	command workloadjournal.Command
	data    []byte
	sha     string
	err     error
}

func (w *agentCoreArtifactWriter) PutAgentChangeBundle(_ context.Context, command workloadjournal.Command,
	data []byte, digest string) (workloadjobs.WorkloadChangeArtifact, error) {
	w.command, w.data, w.sha = command, append([]byte(nil), data...), digest
	return workloadjobs.WorkloadChangeArtifact{StorageRef: "s3://artifacts/prefix/tenant_1/run_1/attempt_1/agent-changes/" + digest + ".zip",
		SizeBytes: int64(len(data)), SHA256: digest}, w.err
}

var _ workloadagent.AttemptChangeBundleWriter = (*agentCoreArtifactWriter)(nil)

func agentCoreResultFixture(t *testing.T) (Specification, workloadagentcore.InvocationRequest,
	workloadagentcore.InvocationDraft, time.Time) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	command := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1",
		Generation: 2, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	source := []byte("trusted source archive bytes")
	sourceDigest := sha256.Sum256(source)
	commit := strings.Repeat("b", 40)
	request := workloadagentcore.InvocationRequest{Command: command, RepositoryID: 97, CommitSHA: commit,
		SourceSHA256: hex.EncodeToString(sourceDigest[:]), Task: "implement a feature", SourceArchive: source}
	spec := Specification{Identity: command, Kind: domain.WorkloadAgent, SourceDigest: request.SourceSHA256,
		CommitSHA: commit, GitHubRepositoryID: request.RepositoryID,
		CorrelationID: "req_12345678-1234-1234-1234-123456789abc", Deadline: now.Add(time.Hour),
		ArtifactPrefix: "s3://artifacts/prefix/tenant_1/run_1/attempt_1/"}
	body, err := json.Marshal(map[string]any{"schema_version": 1, "attempt": map[string]any{
		"tenant_id": command.TenantID, "run_id": command.WorkloadRunID, "attempt_id": command.AttemptID,
		"resource_plane_id": command.ResourcePlaneID, "generation": command.Generation,
		"repository_id": request.RepositoryID, "commit_sha": request.CommitSHA,
		"source_sha256": request.SourceSHA256}, "draft_status": "no_proposal", "iterations": 1,
		"tree_digest": strings.Repeat("a", 64), "proposal": nil})
	if err != nil {
		t.Fatal(err)
	}
	draft, err := workloadagentcore.DecodeInvocationResponse(body, request)
	if err != nil {
		t.Fatal(err)
	}
	return spec, request, draft, now
}

func TestAgentCoreWorkloadResultBindsNoProposalToCurrentWorkload(t *testing.T) {
	spec, request, draft, now := agentCoreResultFixture(t)
	result, err := AgentCoreWorkloadResult(context.Background(), spec, request, draft, nil, now)
	if err != nil || result.Status != domain.WorkloadSucceeded || result.ChangeBundle != nil ||
		result.CorrelationID != spec.CorrelationID || result.AttemptID != request.Command.AttemptID {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAgentCoreWorkloadResultStoresOnlyPinnedProposalDraft(t *testing.T) {
	spec, request, draft, now := agentCoreResultFixture(t)
	var archive bytes.Buffer
	zipWriter := zip.NewWriter(&archive)
	file, err := zipWriter.Create("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("package main\n")); err != nil {
		t.Fatal(err)
	}
	if err := zipWriter.Close(); err != nil {
		t.Fatal(err)
	}
	draft.Status = "proposal_ready"
	draft.ProposalData = archive.Bytes()
	digest := sha256.Sum256(draft.ProposalData)
	draft.ProposalSHA = hex.EncodeToString(digest[:])
	writer := &agentCoreArtifactWriter{}
	result, err := AgentCoreWorkloadResult(context.Background(), spec, request, draft, writer, now)
	if err != nil || result.ChangeBundle == nil || writer.command != request.Command ||
		!bytes.Equal(writer.data, draft.ProposalData) || writer.sha != draft.ProposalSHA ||
		result.ChangeBundle.SHA256 != draft.ProposalSHA || workloadjobs.ValidateResult(result) != nil {
		t.Fatalf("result=%+v artifact=%+v err=%v", result, writer, err)
	}
}

func TestAgentCoreWorkloadResultRejectsForeignMalformedAndFailedDrafts(t *testing.T) {
	spec, request, draft, now := agentCoreResultFixture(t)
	foreign := draft
	foreign.Attempt.TenantID = "foreign"
	if _, err := AgentCoreWorkloadResult(context.Background(), spec, request, foreign, nil, now); err == nil {
		t.Fatal("accepted a foreign Runtime result")
	}
	for name, mutate := range map[string]func(*workloadagentcore.InvocationDraft){
		"status":                 func(value *workloadagentcore.InvocationDraft) { value.Status = "verified" },
		"no proposal with bytes": func(value *workloadagentcore.InvocationDraft) { value.ProposalData = []byte("unexpected") },
		"late result":            func(_ *workloadagentcore.InvocationDraft) {},
	} {
		t.Run(name, func(t *testing.T) {
			changed := draft
			mutate(&changed)
			completedAt := now
			if name == "late result" {
				completedAt = spec.Deadline
			}
			if _, err := AgentCoreWorkloadResult(context.Background(), spec, request, changed, nil, completedAt); err == nil {
				t.Fatal("accepted invalid or late Runtime result")
			}
		})
	}
}
