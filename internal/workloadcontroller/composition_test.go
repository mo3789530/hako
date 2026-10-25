package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type compositionLogWriter struct {
	logs   []byte
	digest string
	calls  int
}

func (w *compositionLogWriter) PutAttemptLogs(_ context.Context, id workloadjournal.Command, logs []byte, digest string) (workloadjobs.WorkloadLogArtifact, error) {
	w.calls++
	w.logs = append([]byte(nil), logs...)
	w.digest = digest
	ref := fmt.Sprintf("s3://bucket/artifacts/%s/%s/%s/logs/%s.txt", id.TenantID, id.WorkloadRunID, id.AttemptID, digest)
	return workloadjobs.WorkloadLogArtifact{StorageRef: ref, SizeBytes: int64(len(logs)), SHA256: digest, Redacted: true}, nil
}

type compositionResultPublisher struct{ results []workloadjobs.WorkloadResult }

func (p *compositionResultPublisher) Publish(_ context.Context, result workloadjobs.WorkloadResult) error {
	p.results = append(p.results, result)
	return nil
}

type compositionRecoveryJournal struct{ *memoryJournal }

func (*compositionRecoveryJournal) ListRecoveryPage(_ context.Context, continuation string, limit int32) ([]workloadjournal.Snapshot, string, error) {
	if continuation != "" || limit < 1 || limit > 1000 {
		return nil, "", ErrInvalidCommand
	}
	return []workloadjournal.Snapshot{}, "", nil
}

type compositionArchiveReader struct{ archive []byte }

func (r compositionArchiveReader) OpenSourceArchive(context.Context, workloadjournal.Command, string, string) (workloadsource.SourceArchive, error) {
	return workloadsource.SourceArchive{Body: io.NopCloser(bytes.NewReader(r.archive)), Size: int64(len(r.archive))}, nil
}

type compositionAgentTransport struct {
	handler http.Handler
	status  []int
}

func (t *compositionAgentTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	t.status = append(t.status, recorder.Code)
	return recorder.Result(), nil
}

type compositionProcessFactory struct {
	mu      sync.Mutex
	process *compositionManagedProcess
	starts  int
}

func (f *compositionProcessFactory) Start(_ context.Context, _ workloadagent.ExecutionPlan, output io.Writer) (workloadagent.ManagedProcess, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts++
	_, _ = io.WriteString(output, "agent test started ghp_123456789012345678901234567890\n")
	f.process = &compositionManagedProcess{done: make(chan struct{})}
	return f.process, nil
}

type compositionManagedProcess struct {
	done chan struct{}
	code int
	err  error
}

func (p *compositionManagedProcess) Wait() (int, error) {
	<-p.done
	return p.code, p.err
}

func (p *compositionManagedProcess) finish(code int, err error) {
	p.code, p.err = code, err
	close(p.done)
}

func TestNewLambdaWorkloadHandlerComposesTrustedResourcePlaneQueuePath(t *testing.T) {
	api := &lambdaStub{}
	config := LambdaWorkloadHandlerConfig{
		ResourcePlaneID: "rp_1",
		Journal:         &memoryJournal{},
		LambdaAPI:       api,
		Resolver: resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
			return Specification{}, ErrSpecification
		}),
		SourceArchives: &fixedSourceArchiveReader{},
		AttemptLogs:    &compositionLogWriter{},
		Runtime: LambdaConfig{
			ImageIdentifier:    "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job",
			ImageVersion:       "1",
			ImageDigest:        strings.Repeat("a", 64),
			VPCEgressConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted",
			IngressConnector:   "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS",
			AgentTicketIssuer:  &recordingTicketIssuer{},
		},
		Now: func() time.Time { return time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC) },
	}
	handler, err := NewLambdaWorkloadHandler(config)
	if err != nil {
		t.Fatalf("compose Resource Plane Lambda handler: %v", err)
	}
	body := envelope(t, func(command *Command) { command.ResourcePlaneID = "rp_other" })
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "message-1", Body: string(body)}}})
	if err != nil || len(response.BatchItemFailures) != 1 || response.BatchItemFailures[0].ItemIdentifier != "message-1" {
		t.Fatalf("foreign Resource Plane command result = %+v, %v", response, err)
	}
	if len(api.inputs) != 0 || len(api.tokenInputs) != 0 {
		t.Fatal("composition or foreign-plane command unexpectedly called Lambda MicroVM APIs")
	}
}

func TestNewLambdaWorkloadHandlerRejectsIncompleteComposition(t *testing.T) {
	if _, err := NewLambdaWorkloadHandler(LambdaWorkloadHandlerConfig{}); err != ErrLambdaComposition {
		t.Fatalf("empty production composition error = %v", err)
	}
}

func TestNewLambdaWorkloadRecoveryHandlerComposesBoundedScheduledSweep(t *testing.T) {
	now := time.Date(2026, 10, 12, 12, 0, 0, 0, time.UTC)
	journal := &compositionRecoveryJournal{memoryJournal: &memoryJournal{}}
	workloadConfig := LambdaWorkloadHandlerConfig{
		ResourcePlaneID: "rp_1",
		Journal:         journal,
		LambdaAPI:       &lambdaStub{},
		Resolver: resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) {
			return Specification{}, ErrSpecification
		}),
		SourceArchives: &fixedSourceArchiveReader{},
		AttemptLogs:    &compositionLogWriter{},
		Runtime: LambdaConfig{
			ImageIdentifier:    "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job",
			ImageVersion:       "1",
			ImageDigest:        strings.Repeat("a", 64),
			VPCEgressConnector: "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted",
			IngressConnector:   "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS",
			AgentTicketIssuer:  &recordingTicketIssuer{},
		},
		Now: func() time.Time { return now },
	}
	leases := &recoveryLeaseStub{}
	handler, err := NewLambdaWorkloadRecoveryHandler(LambdaWorkloadRecoveryHandlerConfig{
		Workload: workloadConfig, Leases: leases, Publisher: &compositionResultPublisher{},
		Recovery: RecoveryLambdaConfig{PageSize: 20, MaxPages: 3, LeaseTTL: time.Minute},
	})
	if err != nil {
		t.Fatalf("compose scheduled Workload recovery handler: %v", err)
	}
	report, err := handler.Handle(context.Background(), events.CloudWatchEvent{})
	if err != nil || !report.LeaseAcquired || !report.Complete || report.Pages != 1 || leases.acquired != 1 || leases.released != 1 {
		t.Fatalf("scheduled recovery report=%+v err=%v leases=%+v", report, err, leases)
	}
}

func TestNewLambdaWorkloadRecoveryHandlerRequiresRecoveryCapableJournalAndDependencies(t *testing.T) {
	base := LambdaWorkloadHandlerConfig{ResourcePlaneID: "rp_1", Journal: &memoryJournal{}}
	if _, err := NewLambdaWorkloadRecoveryHandler(LambdaWorkloadRecoveryHandlerConfig{Workload: base}); err != ErrLambdaComposition {
		t.Fatalf("missing recovery dependencies should fail composition: %v", err)
	}
	config := LambdaWorkloadRecoveryHandlerConfig{Workload: base, Leases: &recoveryLeaseStub{}, Publisher: &compositionResultPublisher{},
		Recovery: RecoveryLambdaConfig{PageSize: 10, MaxPages: 1, LeaseTTL: time.Minute}}
	if _, err := NewLambdaWorkloadRecoveryHandler(config); err != ErrLambdaComposition {
		t.Fatalf("journal without recovery listing should fail composition: %v", err)
	}
}

func TestLambdaWorkloadHandlerDispatchRunsAuthenticatedAgentWithVerifiedSource(t *testing.T) {
	now := time.Now().UTC()
	id := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.invalid/hako-agent-e2e\n\ngo 1.27.3\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(context.Background(), checkout, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	installPath := filepath.Join(t.TempDir(), "installed")
	deadline := now.Add(time.Hour)
	commit := strings.Repeat("b", 40)
	processFactory := &compositionProcessFactory{}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	agent, err := workloadagent.NewFilesystemAgent(workloadagent.FilesystemAgentConfig{SourceDirectory: installPath,
		TrustedKeys: map[string]ed25519.PublicKey{"rp-v1": privateKey.Public().(ed25519.PublicKey)}, ProcessFactory: processFactory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	defer agent.Close(context.Background())
	image := "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job"
	version := "1"
	egress := "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted"
	ingress := "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS"
	profile := RuntimeProfile{ImageIdentifier: image, ImageVersion: version, ImageDigest: strings.Repeat("c", 64),
		JobMemoryLimitMB: 8192, JobCPULimitMillis: 4000,
		IngressNetworkConnectors: []string{ingress}, EgressNetworkConnectors: []string{egress}}
	spec := Specification{Identity: id, ImageIdentifier: image, ImageVersion: version, ImageDigest: profile.ImageDigest,
		SourceDigest: archive.SHA256(), CommitSHA: commit, Deadline: deadline, RequestedAt: now,
		ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/", JobMemoryLimitMB: profile.JobMemoryLimitMB,
		JobCPULimitMillis: profile.JobCPULimitMillis, IngressNetworkConnectors: []string{ingress}, EgressNetworkConnectors: []string{egress}}
	api := &lambdaStub{config: LambdaConfig{ImageIdentifier: image, ImageVersion: version}, state: types.MicrovmStateRunning,
		endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", token: map[string]string{"X-aws-proxy-auth": "short-lived-token"}}
	ticketIssuer := &workloadagent.Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	lambdaConfig := LambdaConfig{ImageIdentifier: image, ImageVersion: version, ImageDigest: profile.ImageDigest,
		VPCEgressConnector: egress, IngressConnector: ingress, AgentTicketIssuer: ticketIssuer}
	journal := &journalDB{rows: make(map[string]workloadjournal.Snapshot)}
	agentTransport := &compositionAgentTransport{handler: agent.Handler()}
	handler, err := NewLambdaWorkloadHandler(LambdaWorkloadHandlerConfig{ResourcePlaneID: string(id.ResourcePlaneID), Journal: journal,
		LambdaAPI: api, Resolver: resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) { return spec, nil }),
		SourceArchives: compositionArchiveReader{archive: func() []byte {
			data, readErr := io.ReadAll(archive.Reader())
			if readErr != nil {
				t.Fatal(readErr)
			}
			return data
		}()}, AttemptLogs: &compositionLogWriter{}, AgentTransport: agentTransport, Runtime: lambdaConfig,
		Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("compose local Lambda→Agent execution path: %v", err)
	}
	eventType := "workload_attempt.dispatch"
	body := envelope(t, nil)
	response, err := handler.Handle(context.Background(), events.SQSEvent{Records: []events.SQSMessage{{MessageId: "dispatch-1", Body: string(body),
		MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {DataType: "String", StringValue: &eventType}}}}})
	if err != nil || len(response.BatchItemFailures) != 0 || processFactory.starts != 1 || len(api.inputs) != 1 {
		_, cause := handler.controller.HandleEvent(context.Background(), eventType, body)
		t.Fatalf("signed source dispatch did not reach one Agent process: response=%+v starts=%d launches=%d agent_statuses=%v err=%v retry=%v", response, processFactory.starts, len(api.inputs), agentTransport.status, err, cause)
	}
	if processFactory.process == nil {
		t.Fatal("Agent did not retain the active process")
	}
	processFactory.process.finish(0, nil)
	publisher := &compositionResultPublisher{}
	var snapshot workloadjournal.Snapshot
	for until := time.Now().Add(time.Second); time.Now().Before(until); {
		snapshot, err = handler.controller.Poll(context.Background(), body, publisher)
		if err != nil {
			t.Fatalf("poll signed MicroVM Agent result: %v", err)
		}
		if snapshot.Journal.ResultPublished && snapshot.Journal.CleanupState == workloadjournal.CleanupConfirmed {
			break
		}
		time.Sleep(time.Millisecond)
	}
	logWriter := handler.controller.runtime.(*LambdaMicroVM).agent.(*HTTPAgentRunner).logs.(*compositionLogWriter)
	if !snapshot.Journal.ResultPublished || snapshot.Journal.CleanupState != workloadjournal.CleanupConfirmed || len(publisher.results) != 1 ||
		publisher.results[0].Status != domain.WorkloadSucceeded || publisher.results[0].LogArtifact == nil ||
		logWriter.calls != 1 || strings.Contains(string(logWriter.logs), "ghp_123") || !strings.Contains(string(logWriter.logs), "[REDACTED]") {
		t.Fatalf("composed result/log/cleanup path incomplete: journal=%+v published=%+v log_calls=%d logs=%q",
			snapshot.Journal, publisher.results, logWriter.calls, logWriter.logs)
	}
}
