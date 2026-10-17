//go:build integration

package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

func TestComposedLambdaWorkerResultAndCleanupProjectIntoPostgres(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool := testutil.NewIsolatedPostgres(t)
	if err := dsql.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	commit := strings.Repeat("b", 40)
	for _, seed := range []struct {
		query string
		args  []any
	}{
		{`INSERT INTO tenants(id,name,created_at) VALUES('tenant_1','Lambda worker E2E',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES('rp_1','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO workload_runs(id,tenant_id,kind,commit_sha,runtime_class,trust_level,desired_state,state,resource_plane_id,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES('run_1','tenant_1','job',$1,'standard','untrusted','running','provisioning','rp_1','lambda-e2e','lambda-e2e',600,$2,2,$3,$3)`, []any{commit, now.Add(10 * time.Minute), now}},
		{`INSERT INTO workload_run_attempts(id,tenant_id,workload_run_id,generation,attempt_number,resource_plane_id,idempotency_token,state,created_at,updated_at)
		 VALUES('attempt_1','tenant_1','run_1',1,1,'rp_1','token_1','running',$1,$1)`, []any{now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed composed Worker E2E: %v", err)
		}
	}

	checkout := t.TempDir()
	if err := os.WriteFile(filepath.Join(checkout, "go.mod"), []byte("module example.invalid/hako-worker-integration\n\ngo 1.27.2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	archive, err := workloadsource.CreateArchive(ctx, checkout, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	archiveBytes, err := io.ReadAll(archive.Reader())
	if err != nil {
		t.Fatal(err)
	}
	installPath := filepath.Join(t.TempDir(), "installed")
	processFactory := &compositionProcessFactory{}
	privateKey := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize))
	agent, err := workloadagent.NewFilesystemAgent(workloadagent.FilesystemAgentConfig{SourceDirectory: installPath,
		TrustedKeys: map[string]ed25519.PublicKey{"rp-v1": privateKey.Public().(ed25519.PublicKey)}, ProcessFactory: processFactory, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = agent.Close(context.Background()) })

	image := "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-job"
	version := "1"
	ingress := "arn:aws:lambda:ap-northeast-1:aws:network-connector:aws-network-connector:ALL_INGRESS"
	egress := "arn:aws:lambda:ap-northeast-1:123456789012:network-connector:restricted"
	digest := strings.Repeat("c", 64)
	deadline := now.Add(10 * time.Minute)
	identity := workloadjournal.Command{TenantID: "tenant_1", WorkloadRunID: "run_1", AttemptID: "attempt_1", Generation: 1,
		ResourcePlaneID: "rp_1", IdempotencyToken: "token_1"}
	spec := Specification{Identity: identity, ImageIdentifier: image, ImageVersion: version, ImageDigest: digest,
		SourceDigest: archive.SHA256(), CommitSHA: commit, Deadline: deadline, RequestedAt: now,
		ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/attempt_1/", IngressNetworkConnectors: []string{ingress}, EgressNetworkConnectors: []string{egress}}
	api := &lambdaStub{config: LambdaConfig{ImageIdentifier: image, ImageVersion: version}, state: types.MicrovmStateRunning,
		endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", token: map[string]string{"X-aws-proxy-auth": "short-lived-token"}}
	issuer := &workloadagent.Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	logs := &compositionLogWriter{}
	agentTransport := &compositionAgentTransport{handler: agent.Handler()}
	worker, err := NewLambdaWorkloadHandler(LambdaWorkloadHandlerConfig{ResourcePlaneID: "rp_1", Journal: &journalDB{rows: make(map[string]workloadjournal.Snapshot)},
		LambdaAPI: api, Resolver: resolveFunc(func(context.Context, workloadjournal.Command) (Specification, error) { return spec, nil }),
		SourceArchives: compositionArchiveReader{archive: archiveBytes}, AttemptLogs: logs, AgentTransport: agentTransport,
		Runtime: LambdaConfig{ImageIdentifier: image, ImageVersion: version, ImageDigest: digest, VPCEgressConnector: egress,
			IngressConnector: ingress, AgentTicketIssuer: issuer}, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatalf("compose Lambda MicroVM Worker: %v", err)
	}
	eventType := "workload_attempt.dispatch"
	body := envelope(t, nil)
	response, err := worker.Handle(ctx, events.SQSEvent{Records: []events.SQSMessage{{MessageId: "dispatch-1", Body: string(body),
		MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {DataType: "String", StringValue: &eventType}}}}})
	if err != nil || len(response.BatchItemFailures) != 0 || processFactory.starts != 1 || len(api.inputs) != 1 {
		t.Fatalf("Worker command did not launch exactly one MicroVM/Agent: response=%+v starts=%d launches=%d err=%v", response, processFactory.starts, len(api.inputs), err)
	}
	if processFactory.process == nil {
		t.Fatal("Worker Agent did not retain the running execution")
	}
	processFactory.process.finish(0, nil)

	apply := workloadjobs.ResultHandler{Pool: pool, Policy: transaction.DefaultPolicy()}
	sends := 0
	lostAck := errors.New("simulate lost acknowledgement after durable Control Plane commit")
	publisher := publishFunc(func(ctx context.Context, result workloadjobs.WorkloadResult) error {
		sends++
		outcome, err := apply.Handle(ctx, result)
		if err != nil {
			return err
		}
		if sends == 1 && outcome != workloadjobs.ResultApplied {
			t.Fatalf("first Worker result outcome = %q, want applied", outcome)
		}
		if sends == 2 && outcome != workloadjobs.ResultDuplicate {
			t.Fatalf("replayed Worker result outcome = %q, want duplicate", outcome)
		}
		if sends == 1 {
			return lostAck
		}
		return nil
	})
	var snapshot workloadjournal.Snapshot
	for until := time.Now().Add(2 * time.Second); time.Now().Before(until); {
		snapshot, err = worker.controller.Poll(ctx, body, publisher)
		if errors.Is(err, lostAck) {
			break
		}
		if err != nil {
			t.Fatalf("poll composed Worker result: %v", err)
		}
		if snapshot.Journal.CleanupState == workloadjournal.CleanupConfirmed && snapshot.Journal.ResultPublished {
			break
		}
		time.Sleep(time.Millisecond)
	}
	// The lost-ack error is intentionally opaque to this test; replay once after
	// the worker has durably frozen its result to prove Control Plane idempotency.
	if sends == 0 {
		t.Fatalf("composed Worker did not publish its result: journal=%+v err=%v", snapshot.Journal, err)
	}
	if err != nil {
		snapshot, err = worker.controller.Poll(ctx, body, publisher)
	}
	if err != nil || sends != 2 || !snapshot.Journal.ResultPublished || snapshot.Journal.CleanupState != workloadjournal.CleanupConfirmed {
		t.Fatalf("composed Worker result/cleanup replay incomplete: sends=%d snapshot=%+v err=%v", sends, snapshot.Journal, err)
	}
	var state domain.WorkloadRunState
	var artifactCount int
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_runs WHERE tenant_id='tenant_1' AND id='run_1'`).Scan(&state); err != nil {
		t.Fatal(err)
	}
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_artifacts WHERE tenant_id='tenant_1' AND workload_run_id='run_1'`).Scan(&artifactCount); err != nil {
		t.Fatal(err)
	}
	if state != domain.WorkloadSucceeded || artifactCount != 1 || logs.calls != 1 || strings.Contains(string(logs.logs), "ghp_123") || !strings.Contains(string(logs.logs), "[REDACTED]") {
		t.Fatalf("Control Plane projection or artifact redaction failed: state=%s artifacts=%d log_calls=%d logs=%q", state, artifactCount, logs.calls, logs.logs)
	}
	var attemptState string
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_run_attempts WHERE tenant_id='tenant_1' AND id='attempt_1'`).Scan(&attemptState); err != nil || attemptState != "succeeded" {
		t.Fatalf("attempt state = %q, err=%v", attemptState, err)
	}
}
