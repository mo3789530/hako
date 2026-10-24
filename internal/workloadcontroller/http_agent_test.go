package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type agentRoundTrip func(*http.Request) (*http.Response, error)

func (f agentRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type fixedAgentTaskProvider struct {
	task  workloadagent.AgentTask
	err   error
	calls int
	id    workloadjournal.Command
}

func (p *fixedAgentTaskProvider) ResolveAgentTask(_ context.Context, id workloadjournal.Command) (workloadagent.AgentTask, error) {
	p.calls++
	p.id = id
	return p.task, p.err
}

func TestHTTPAgentRunnerFailsClosedWithoutSourceReader(t *testing.T) {
	called := false
	runner := NewHTTPAgentRunner(workloadagent.NewClient(agentRoundTrip(func(*http.Request) (*http.Response, error) {
		called = true
		return nil, nil
	})), nil, nil)
	if err := runner.Start(context.Background(), AgentConnection{MicroVMID: "vm_1"}, Specification{}); err == nil || called {
		t.Fatalf("Start without source archive reader = %v, transport called=%v", err, called)
	}
}

func TestHTTPAgentRunnerMapsTrustedSpecificationToProtocol(t *testing.T) {
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	deadline := time.Date(2026, 10, 12, 1, 0, 0, 0, time.FixedZone("JST", 9*60*60))
	archiveBytes := []byte("verified-content-addressed-tar")
	var sourceUploaded bool
	transport := agentRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Method != http.MethodPut || req.Header.Get("X-aws-proxy-auth") != "one-use-token" {
			t.Fatalf("unexpected agent request: %s %v", req.Method, req.Header)
		}
		if strings.HasSuffix(req.URL.Path, "/source") {
			if req.Header.Get("Authorization") != "Bearer source-ticket" || req.Header.Get("X-Hako-Commit-SHA") != strings.Repeat("b", 40) {
				t.Fatalf("source archive was not commit-bound: %v", req.Header)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || !bytes.Equal(body, archiveBytes) {
				t.Fatalf("source upload body = %q err=%v", body, err)
			}
			sourceUploaded = true
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		if !sourceUploaded || req.Header.Get("Authorization") != "Bearer signed-agent-ticket" {
			t.Fatal("Start was sent before authenticated source upload")
		}
		var body workloadagent.StartRequest
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Identity.Command() != id || body.SourceDigest != strings.Repeat("a", 64) || body.CommitSHA != strings.Repeat("b", 40) || body.Deadline.Location() != time.UTC {
			t.Fatalf("Agent protocol request did not preserve trusted specification: %+v", body)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	sourceReader := &fixedSourceArchiveReader{archive: workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(archiveBytes)), Size: int64(len(archiveBytes))}}
	runner := NewHTTPAgentRunner(workloadagent.NewClient(transport), sourceReader, nil)
	err := runner.Start(context.Background(), AgentConnection{MicroVMID: "vm_1", Endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", AuthToken: "one-use-token", AgentTicket: "signed-agent-ticket", SourceTicket: "source-ticket"}, Specification{
		Identity: id, SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: deadline,
		ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/",
	})
	if err != nil || !sourceUploaded || sourceReader.calls != 1 {
		t.Fatalf("Start() = %v, sourceUploaded=%v reads=%d", err, sourceUploaded, sourceReader.calls)
	}
}

func TestHTTPAgentRunnerResolvesAndTransfersBoundIssueTask(t *testing.T) {
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 2,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	issueBody := "Implement the approved issue request"
	task := workloadagent.AgentTask{TenantID: "tenant_1", WorkloadRunID: "run_1", IntegrationID: "integration_1", DeliveryID: "delivery_12345678",
		InstallationID: 71, RepositoryID: 81, IssueNumber: 33, CommentID: 44, DefaultBranch: "main", CommitSHA: strings.Repeat("b", 40),
		Title: "Add feature", Body: issueBody}
	task.ContextSHA256 = workloadagent.AgentTaskContextSHA256(task)
	provider := &fixedAgentTaskProvider{task: task}
	archiveBytes := []byte("verified Agent source archive")
	var sourceUploaded, taskStarted bool
	transport := agentRoundTrip(func(req *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(req.URL.Path, "/source"):
			sourceUploaded = true
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}},
				Body: io.NopCloser(strings.NewReader(""))}, nil
		case strings.HasSuffix(req.URL.Path, "/resource-planes/rp_1"):
			if !sourceUploaded || req.Header.Get("Authorization") != "Bearer agent-ticket" {
				t.Fatal("Agent Start preceded source upload or used the wrong ticket")
			}
			var start workloadagent.StartRequest
			if err := json.NewDecoder(req.Body).Decode(&start); err != nil {
				t.Fatal(err)
			}
			if start.Kind != domain.WorkloadAgent || start.AgentTask == nil || *start.AgentTask != task {
				t.Fatalf("Agent task context was not transferred intact: %+v", start)
			}
			taskStarted = true
			return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}},
				Body: io.NopCloser(strings.NewReader(""))}, nil
		default:
			t.Fatalf("unexpected Agent protocol route: %s", req.URL.Path)
			return nil, nil
		}
	})
	reader := &fixedSourceArchiveReader{archive: workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(archiveBytes)), Size: int64(len(archiveBytes))}}
	runner := NewHTTPAgentRunner(workloadagent.NewClient(transport), reader, nil, provider)
	err := runner.Start(context.Background(), AgentConnection{MicroVMID: "vm_1", Endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws",
		AuthToken: "one-use-token", AgentTicket: "agent-ticket", SourceTicket: "source-ticket"}, Specification{Identity: id,
		Kind: domain.WorkloadAgent, GitHubIntegrationID: task.IntegrationID, GitHubInstallationID: task.InstallationID, GitHubRepositoryID: task.RepositoryID,
		AgentTaskContextSHA256: task.ContextSHA256,
		SourceDigest:           strings.Repeat("a", 64), CommitSHA: task.CommitSHA, TriggerEvent: "issue_comment", TriggerBranch: task.DefaultBranch,
		Deadline: time.Now().UTC().Add(time.Hour), ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/"})
	if err != nil || !sourceUploaded || !taskStarted || provider.calls != 1 || provider.id != id {
		t.Fatalf("Agent task dispatch err=%v source=%t started=%t provider calls=%d identity=%+v", err, sourceUploaded, taskStarted, provider.calls, provider.id)
	}
}

func TestHTTPAgentRunnerRequiresValidIssueTaskBeforeSourceSideEffects(t *testing.T) {
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	archive := &fixedSourceArchiveReader{archive: workloadsource.SourceArchive{Body: io.NopCloser(strings.NewReader("archive")), Size: 7}}
	runner := NewHTTPAgentRunner(workloadagent.NewClient(agentRoundTrip(func(*http.Request) (*http.Response, error) {
		t.Fatal("invalid or unavailable Agent task reached the MicroVM")
		return nil, nil
	})), archive, nil)
	err := runner.Start(context.Background(), AgentConnection{MicroVMID: "vm_1", Endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws",
		AuthToken: "one-use-token", AgentTicket: "agent-ticket", SourceTicket: "source-ticket"}, Specification{Identity: id,
		Kind: domain.WorkloadAgent, GitHubIntegrationID: "integration_1", GitHubInstallationID: 71, GitHubRepositoryID: 81,
		SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), TriggerBranch: "main",
		Deadline: time.Now().UTC().Add(time.Hour), ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/"})
	if !errors.Is(err, ErrExecutionMaterialization) || archive.calls != 0 {
		t.Fatalf("Agent task without broker provider err=%v source reads=%d", err, archive.calls)
	}
}

type fixedSourceArchiveReader struct {
	archive workloadsource.SourceArchive
	err     error
	calls   int
}

func (r *fixedSourceArchiveReader) OpenSourceArchive(_ context.Context, id workloadjournal.Command, prefix, digest string) (workloadsource.SourceArchive, error) {
	r.calls++
	if id.TenantID != "tenant_1" || prefix != "s3://bucket/artifacts/tenant_1/run_1/attempt_1/" || digest != strings.Repeat("a", 64) {
		return workloadsource.SourceArchive{}, ErrExecutionMetadata
	}
	return r.archive, r.err
}

func TestHTTPAgentRunnerTransfersSourceBeforeStartingProcess(t *testing.T) {
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	archiveBytes := []byte("verified-content-addressed-tar")
	var methods []string
	transport := agentRoundTrip(func(req *http.Request) (*http.Response, error) {
		methods = append(methods, req.URL.Path)
		if req.Header.Get("X-aws-proxy-auth") != "one-use-token" {
			t.Fatal("MicroVM endpoint token missing from source/Start request")
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/source"):
			if req.Method != http.MethodPut || req.Header.Get("Authorization") != "Bearer source-ticket" || req.Header.Get("X-Hako-Source-SHA256") != strings.Repeat("a", 64) || req.ContentLength != int64(len(archiveBytes)) {
				t.Fatalf("unexpected source upload request: %+v", req)
			}
			body, err := io.ReadAll(req.Body)
			if err != nil || !bytes.Equal(body, archiveBytes) {
				t.Fatalf("source bytes = %q err=%v", body, err)
			}
		case strings.HasSuffix(req.URL.Path, "/resource-planes/rp_1"):
			if req.Method != http.MethodPut || req.Header.Get("Authorization") != "Bearer start-ticket" {
				t.Fatalf("unexpected Start request: %+v", req)
			}
		default:
			t.Fatalf("unexpected path: %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNoContent, Header: http.Header{"X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})
	sourceReader := &fixedSourceArchiveReader{archive: workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(archiveBytes)), Size: int64(len(archiveBytes))}}
	runner := NewHTTPAgentRunner(workloadagent.NewClient(transport), sourceReader, nil)
	err := runner.Start(context.Background(), AgentConnection{MicroVMID: "vm_1", Endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws",
		AuthToken: "one-use-token", AgentTicket: "start-ticket", SourceTicket: "source-ticket"}, Specification{Identity: id,
		SourceDigest: strings.Repeat("a", 64), CommitSHA: strings.Repeat("b", 40), Deadline: time.Now().Add(time.Hour),
		ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/"})
	if err != nil || sourceReader.calls != 1 || len(methods) != 2 || !strings.HasSuffix(methods[0], "/source") || !strings.HasSuffix(methods[1], "/resource-planes/rp_1") {
		t.Fatalf("source/Start order = %v source calls=%d err=%v", methods, sourceReader.calls, err)
	}
}

type captureAgentLogWriter struct {
	identity workloadjournal.Command
	logs     []byte
	digest   string
}

func (w *captureAgentLogWriter) PutAttemptLogs(_ context.Context, id workloadjournal.Command, logs []byte, digest string) (workloadjobs.WorkloadLogArtifact, error) {
	w.identity, w.logs, w.digest = id, append([]byte(nil), logs...), digest
	return workloadjobs.WorkloadLogArtifact{StorageRef: "s3://artifacts/tenant_1/run_1/attempt_1/log.txt", SizeBytes: int64(len(logs)), SHA256: digest, Redacted: true}, nil
}

func TestHTTPAgentRunnerFetchesRedactedLogsAndStoresThemFromController(t *testing.T) {
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1, ResourcePlaneID: "rp_1", IdempotencyToken: "idem_1"}
	logs := []byte("redacted test output\n")
	logSum := sha256.Sum256(logs)
	result := workloadjobs.WorkloadResult{SchemaVersion: 1, TenantID: domain.TenantID(id.TenantID), WorkloadRunID: domain.WorkloadRunID(id.WorkloadRunID),
		AttemptID: id.AttemptID, Generation: id.Generation, ResourcePlaneID: domain.ResourcePlaneID(id.ResourcePlaneID), CommitSHA: strings.Repeat("b", 40),
		Status: domain.WorkloadSucceeded, ExitCode: 0, CompletedAt: time.Date(2026, 10, 12, 1, 2, 3, 0, time.UTC)}
	resultBody, err := json.Marshal(workloadagent.ResultResponse{Identity: workloadagent.IdentityFromCommand(id), Result: result})
	if err != nil {
		t.Fatal(err)
	}
	transport := agentRoundTrip(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("X-aws-proxy-auth") != "vm-token" {
			t.Fatal("missing MicroVM-scoped auth token")
		}
		switch {
		case strings.HasSuffix(req.URL.Path, "/result"):
			if req.Header.Get("Authorization") != "Bearer result-ticket" {
				t.Fatal("result operation used the wrong ticket")
			}
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"application/json"}, "X-Hako-Agent-Protocol": []string{workloadagent.ProtocolVersion}}, Body: io.NopCloser(bytes.NewReader(resultBody)), ContentLength: int64(len(resultBody))}, nil
		case strings.HasSuffix(req.URL.Path, "/logs"):
			if req.Header.Get("Authorization") != "Bearer logs-ticket" {
				t.Fatal("logs operation used the wrong ticket")
			}
			headers := make(http.Header)
			headers.Set("Content-Type", "text/plain; charset=utf-8")
			headers.Set("X-Hako-Agent-Protocol", workloadagent.ProtocolVersion)
			headers.Set("X-Hako-Log-SHA256", hex.EncodeToString(logSum[:]))
			return &http.Response{StatusCode: http.StatusOK, Header: headers,
				Body: io.NopCloser(bytes.NewReader(logs)), ContentLength: int64(len(logs))}, nil
		default:
			t.Fatalf("unexpected agent route: %s", req.URL.Path)
			return nil, nil
		}
	})
	writer := &captureAgentLogWriter{}
	client := workloadagent.NewClient(transport)
	connection := AgentConnection{MicroVMID: "vm_1", Endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws",
		AuthToken: "vm-token", AgentTicket: "result-ticket", LogTicket: "logs-ticket"}
	if _, err := client.Result(context.Background(), connection.Endpoint, connection.AuthToken, connection.AgentTicket, id); err != nil {
		t.Fatalf("Result() failed: %v", err)
	}
	if _, _, err := client.Logs(context.Background(), connection.Endpoint, connection.AuthToken, connection.LogTicket, id); err != nil {
		t.Fatalf("Logs() failed: %v", err)
	}
	runner := NewHTTPAgentRunner(client, nil, writer)
	got, err := runner.CollectResult(context.Background(), connection, id)
	if err != nil || got.LogArtifact == nil || !got.LogArtifact.Redacted || writer.identity != id || !bytes.Equal(writer.logs, logs) || writer.digest != hex.EncodeToString(logSum[:]) {
		t.Fatalf("CollectResult() = %+v, writer=%+v, err=%v", got, writer, err)
	}
}
