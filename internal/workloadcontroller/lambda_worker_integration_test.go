//go:build integration

package workloadcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-lambda-go/events"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms/types"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
	"github.com/mo3789530/hako/internal/workloadagent"
	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
	"github.com/mo3789530/hako/internal/workloadsource"
)

type lambdaWorkerDispatchDelivery struct {
	queueURL string
	event    outbox.Event
}

type lambdaWorkerDispatchCapture struct {
	deliveries []lambdaWorkerDispatchDelivery
}

func (p *lambdaWorkerDispatchCapture) Publish(_ context.Context, queueURL string, event outbox.Event) error {
	p.deliveries = append(p.deliveries, lambdaWorkerDispatchDelivery{queueURL: queueURL, event: event})
	return nil
}

type lambdaWorkerScheduleQueue struct {
	delivery workloadjobs.ScheduleDelivery
	received bool
	deleted  int
}

func (q *lambdaWorkerScheduleQueue) Receive(context.Context, int, time.Duration) ([]workloadjobs.ScheduleDelivery, error) {
	if q.received {
		return nil, nil
	}
	q.received = true
	return []workloadjobs.ScheduleDelivery{q.delivery}, nil
}

func (q *lambdaWorkerScheduleQueue) Delete(_ context.Context, receipt string) error {
	if receipt != q.delivery.ReceiptHandle {
		return errors.New("wrong scheduler receipt")
	}
	q.deleted++
	return nil
}

func (*lambdaWorkerScheduleQueue) ChangeVisibility(context.Context, string, time.Duration) error {
	return nil
}

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
		{`INSERT INTO users(id,cognito_subject,email,created_at) VALUES('usr_1','lambda-worker-e2e','',$1)`, []any{now}},
		{`INSERT INTO resource_planes(id,provider,region,capabilities_json) VALUES('rp_1','aws','ap-northeast-1','{}')`, nil},
		{`INSERT INTO tenant_github_installations(tenant_id,installation_id,account_login,status,requested_by,requested_at,updated_at) VALUES('tenant_1',71,'lambda-worker-e2e','active','usr_1',$1,$1)`, []any{now}},
		{`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES(71,'tenant_1',$1)`, []any{now}},
		{`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at) VALUES('tenant_1',71,81,'lambda-worker-e2e','repo','main',$1)`, []any{now}},
		{`INSERT INTO workload_runs(id,tenant_id,kind,github_installation_id,github_repository_id,commit_sha,ref,runtime_class,trust_level,desired_state,state,idempotency_key,request_hash,timeout_seconds,timeout_at,revision,created_at,updated_at)
		 VALUES('run_1','tenant_1','job',71,81,$1,'refs/heads/main','standard','untrusted','running','queued','lambda-e2e','lambda-e2e',600,$2,1,$3,$3)`, []any{commit, now.Add(10 * time.Minute), now}},
	} {
		if _, err := pool.Exec(ctx, seed.query, seed.args...); err != nil {
			t.Fatalf("seed composed Worker E2E: %v", err)
		}
	}

	// Exercise scheduler placement and transactional Outbox dispatch before
	// delivering the exact command envelope to the Lambda Worker.
	scheduler := workloadjobs.DispatchHandler{Pool: pool, Policy: transaction.DefaultPolicy(),
		Placement: workloadjobs.PlacementSelectorFunc(func(context.Context, domain.WorkloadRun) (domain.ResourcePlaneID, error) { return "rp_1", nil }),
		Now:       func() time.Time { return now }}
	schedule := workloadjobs.ScheduleRequest{SchemaVersion: 1, TenantID: "tenant_1", WorkloadRunID: "run_1"}
	scheduleBody, err := json.Marshal(schedule)
	if err != nil {
		t.Fatal(err)
	}
	scheduleQueue := &lambdaWorkerScheduleQueue{delivery: workloadjobs.ScheduleDelivery{Body: scheduleBody, ReceiptHandle: "schedule-1"}}
	schedulerConsumer, err := workloadjobs.NewSchedulerConsumer(scheduleQueue, &scheduler, workloadjobs.ConsumerConfig{
		BatchSize: 1, WaitTime: 0, VisibilityTimeout: 5 * time.Second, VisibilityHeartbeat: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	scheduleStats, err := schedulerConsumer.RunOnce(ctx)
	if err != nil || scheduleStats.Handled != 1 || scheduleQueue.deleted != 1 {
		t.Fatalf("consume scheduled Run and acknowledge after Outbox commit: stats=%+v deleted=%d err=%v", scheduleStats, scheduleQueue.deleted, err)
	}
	if err := scheduler.Handle(ctx, schedule); err != nil {
		t.Fatalf("schedule Workload Run to Resource Plane: %v", err)
	}
	if err := scheduler.Handle(ctx, schedule); err != nil {
		t.Fatalf("ack duplicate scheduler delivery: %v", err)
	}
	dispatchCapture := &lambdaWorkerDispatchCapture{}
	dispatcherInstance, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, dispatchCapture,
		map[string]string{"rp_1": "https://sqs.invalid/legacy-rp"}, dispatcher.Config{
			WorkloadCommandQueueURLs: map[string]string{"rp_1": "https://sqs.invalid/workload"},
			GitHubChecksQueueURL:     "https://sqs.invalid/checks",
			Now:                      func() time.Time { return now.Add(time.Second) },
		})
	if err != nil {
		t.Fatal(err)
	}
	dispatchStats, err := dispatcherInstance.RunOnce(ctx)
	if err != nil || dispatchStats.Failed != 0 {
		t.Fatalf("dispatch Workload Outbox: stats=%+v err=%v", dispatchStats, err)
	}
	var delivery *lambdaWorkerDispatchDelivery
	for index := range dispatchCapture.deliveries {
		candidate := &dispatchCapture.deliveries[index]
		if candidate.event.EventType == "workload_attempt.dispatch" {
			delivery = candidate
			break
		}
	}
	if delivery == nil || delivery.queueURL != "https://sqs.invalid/workload" {
		t.Fatalf("Dispatcher did not route attempt to the dedicated Workload queue: %+v", dispatchCapture.deliveries)
	}
	var command workloadjobs.DispatchCommand
	if err := json.Unmarshal(delivery.event.Payload, &command); err != nil || command.SchemaVersion != 1 || command.TenantID != "tenant_1" ||
		command.WorkloadRunID != "run_1" || command.AttemptID == "" || command.Generation != 1 || command.ResourcePlane != "rp_1" || command.IdempotencyToken == "" {
		t.Fatalf("Dispatcher produced invalid attempt command: %+v err=%v", command, err)
	}
	var attemptCount int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM workload_run_attempts WHERE tenant_id='tenant_1' AND workload_run_id='run_1'`).Scan(&attemptCount); err != nil || attemptCount != 1 {
		t.Fatalf("duplicate scheduling created %d attempts, err=%v", attemptCount, err)
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
	identity := workloadjournal.Command{TenantID: string(command.TenantID), WorkloadRunID: string(command.WorkloadRunID), AttemptID: command.AttemptID, Generation: command.Generation,
		ResourcePlaneID: string(command.ResourcePlane), IdempotencyToken: command.IdempotencyToken}
	spec := Specification{Identity: identity, ImageIdentifier: image, ImageVersion: version, ImageDigest: digest,
		SourceDigest: archive.SHA256(), CommitSHA: commit, Deadline: deadline, RequestedAt: now,
		ArtifactPrefix: "s3://bucket/artifacts/tenant_1/run_1/" + command.AttemptID + "/", IngressNetworkConnectors: []string{ingress}, EgressNetworkConnectors: []string{egress}}
	api := &lambdaStub{config: LambdaConfig{ImageIdentifier: image, ImageVersion: version}, state: types.MicrovmStateRunning,
		endpoint: "https://microvm_1.lambda-microvm.ap-northeast-1.on.aws", token: map[string]string{"X-aws-proxy-auth": "short-lived-token"}}
	issuer := &workloadagent.Ed25519TicketIssuer{KeyID: "rp-v1", PrivateKey: privateKey, Now: func() time.Time { return now }}
	logs := &compositionLogWriter{}
	agentTransport := &compositionAgentTransport{handler: agent.Handler()}
	journal := &journalDB{rows: make(map[string]workloadjournal.Snapshot)}
	workerConfig := LambdaWorkloadHandlerConfig{ResourcePlaneID: "rp_1", Journal: journal,
		LambdaAPI: api, Resolver: resolveFunc(func(_ context.Context, id workloadjournal.Command) (Specification, error) {
			spec.Identity = id
			return spec, nil
		}),
		SourceArchives: compositionArchiveReader{archive: archiveBytes}, AttemptLogs: logs, AgentTransport: agentTransport,
		Runtime: LambdaConfig{ImageIdentifier: image, ImageVersion: version, ImageDigest: digest, VPCEgressConnector: egress,
			IngressConnector: ingress, AgentTicketIssuer: issuer}, Now: func() time.Time { return now }}
	worker, err := NewLambdaWorkloadHandler(workerConfig)
	if err != nil {
		t.Fatalf("compose Lambda MicroVM Worker: %v", err)
	}
	eventType := "workload_attempt.dispatch"
	body := delivery.event.Payload
	response, err := worker.Handle(ctx, events.SQSEvent{Records: []events.SQSMessage{{MessageId: delivery.event.ID, Body: string(body),
		MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {DataType: "String", StringValue: &eventType}}}}})
	if err != nil || len(response.BatchItemFailures) != 0 || processFactory.starts != 1 || len(api.inputs) != 1 {
		t.Fatalf("Worker command did not launch exactly one MicroVM/Agent: response=%+v starts=%d launches=%d err=%v", response, processFactory.starts, len(api.inputs), err)
	}
	if processFactory.process == nil {
		t.Fatal("Worker Agent did not retain the running execution")
	}
	// Simulate a Lambda Worker process restart while the MicroVM/Agent remains
	// active. The shared journal is the durable checkpoint; replay must reuse
	// its provider binding and existing Agent process instead of relaunching.
	restartedWorker, err := NewLambdaWorkloadHandler(workerConfig)
	if err != nil {
		t.Fatalf("recreate Lambda Worker after process restart: %v", err)
	}
	restartedResponse, err := restartedWorker.Handle(ctx, events.SQSEvent{Records: []events.SQSMessage{{MessageId: delivery.event.ID + "-retry", Body: string(body),
		MessageAttributes: map[string]events.SQSMessageAttribute{"HakoEventType": {DataType: "String", StringValue: &eventType}}}}})
	if err != nil || len(restartedResponse.BatchItemFailures) != 0 || processFactory.starts != 1 || len(api.inputs) != 1 {
		t.Fatalf("duplicate dispatch after Worker restart relaunched execution: response=%+v starts=%d launches=%d err=%v", restartedResponse, processFactory.starts, len(api.inputs), err)
	}
	worker = restartedWorker
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
	if err := pool.QueryRow(ctx, `SELECT state FROM workload_run_attempts WHERE tenant_id='tenant_1' AND id=$1`, command.AttemptID).Scan(&attemptState); err != nil || attemptState != "succeeded" {
		t.Fatalf("attempt state = %q, err=%v", attemptState, err)
	}
}
