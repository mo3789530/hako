//go:build integration

package actionsrunner

import (
	"context"
	"crypto/ed25519"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/mo3789530/hako/internal/dispatcher"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/githubchecks"
	hakowebhook "github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/modelcredentialbroker"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/modelcredentials"
	"github.com/mo3789530/hako/internal/store/outbox"
	"github.com/mo3789530/hako/internal/store/transaction"
	"github.com/mo3789530/hako/internal/testutil"
)

// This composes signed Tenant webhook ingress, authorized queued admission,
// runner startup/status, durable Check publication, and Broker HTTP cleanup
// recovery. External providers and GitHub are fakes.
func TestActionsRunnerTerminalWebhookCleanupRecovery(t *testing.T) {
	for _, conclusion := range []string{"success", "failure", "cancelled", "timed_out"} {
		t.Run(conclusion, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			pool := testutil.NewIsolatedPostgres(t)
			if err := dsql.Migrate(ctx, pool); err != nil {
				t.Fatal(err)
			}
			for _, query := range []string{
				`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_cleanup','Cleanup',now())`,
				`INSERT INTO tenants(id,name,created_at) VALUES ('tenant_foreign','Foreign',now())`,
				`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_cleanup','cleanup','',now())`,
				`INSERT INTO users(id,cognito_subject,email,created_at) VALUES ('usr_foreign','foreign','',now())`,
				`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_foreign','9901','tenant_owned','tenant_foreign','foreign','foreign-client',now())`,
				`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_foreign','tenant_foreign',9901,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:foreign-key','foreign-oauth','foreign-webhook',now(),'arn:aws:iam::123456789012:role/hako/foreign-key-reader','22222222222222222222222222222222')`,
				`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_foreign','tenant_foreign','registration_foreign',9901,'binding_foreign','active',1,1,'foreign-key','` + strings.Repeat("f", 64) + `','usr_foreign',now(),now())`,
				`INSERT INTO tenant_github_installations(tenant_id,installation_id,github_app_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_foreign',9902,9901,'foreign','active','usr_foreign',now(),now())`,
				`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (9902,'tenant_foreign',now())`,
				`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_foreign','integration_foreign',9902,now())`,
				`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_foreign',9902,9903,'foreign','api','main',now(),true,now())`,
				`INSERT INTO github_app_registrations(id,github_app_id,mode,owner_tenant_id,slug,client_id,created_at) VALUES ('registration_cleanup',9900,'tenant_owned','tenant_cleanup','cleanup','client-cleanup',now())`,
				`INSERT INTO github_credential_bindings(id,tenant_id,github_app_id,status,private_key_secret_ref,oauth_secret_ref,webhook_secret_ref,approved_at,private_key_reader_role_arn,private_key_secret_version_id) VALUES ('binding_cleanup','tenant_cleanup',9900,'approved','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:cleanup-key','oauth-cleanup','webhook-cleanup',now(),'arn:aws:iam::123456789012:role/hako/cleanup-key-reader','0123456789abcdef0123456789abcdef')`,
				`INSERT INTO tenant_github_integrations(id,tenant_id,app_registration_id,github_app_id,credential_binding_id,status,config_revision,credential_generation,idempotency_key,request_sha256,created_by,created_at,updated_at) VALUES ('integration_cleanup','tenant_cleanup','registration_cleanup',9900,'binding_cleanup','active',1,1,'cleanup-key','` + strings.Repeat("c", 64) + `','usr_cleanup',now(),now())`,
				`INSERT INTO tenant_github_installations(tenant_id,installation_id,github_app_id,account_login,status,requested_by,requested_at,updated_at) VALUES ('tenant_cleanup',8801,9900,'acme','active','usr_cleanup',now(),now())`,
				`INSERT INTO github_app_installation_bindings(installation_id,tenant_id,bound_at) VALUES (8801,'tenant_cleanup',now())`,
				`INSERT INTO tenant_github_app_installation_bindings(tenant_id,integration_id,installation_id,bound_at) VALUES ('tenant_cleanup','integration_cleanup',8801,now())`,
				`INSERT INTO tenant_github_repositories(tenant_id,installation_id,github_repository_id,owner_login,repository_name,default_branch,synchronized_at,actions_runner_enabled,actions_runner_enabled_at) VALUES ('tenant_cleanup',8801,8802,'acme','api','main',now(),true,now()-interval '1 minute')`,
				`INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,secret_arn,reader_role_arn,status,created_at,updated_at) VALUES ('model_binding_clean','tenant_cleanup','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:cleanup-model','arn:aws:iam::123456789012:role/hako/cleanup-model-reader','approved',now(),now())`,
				`INSERT INTO tenant_model_credentials(id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at) VALUES ('mcred_cleanup01','tenant_cleanup','anthropic','model_binding_clean','11111111111111111111111111111111','active',1,'usr_cleanup','usr_cleanup',now(),now())`,
				`INSERT INTO tenant_model_credential_bindings(id,tenant_id,provider,secret_arn,reader_role_arn,status,created_at,updated_at) VALUES ('model_binding_foreign','tenant_foreign','anthropic','arn:aws:secretsmanager:ap-northeast-1:123456789012:secret:foreign-model','arn:aws:iam::123456789012:role/hako/foreign-model-reader','approved',now(),now())`,
				`INSERT INTO tenant_model_credentials(id,tenant_id,provider,binding_id,secret_version_id,status,credential_generation,created_by,updated_by,created_at,updated_at) VALUES ('mcred_foreign01','tenant_foreign','anthropic','model_binding_foreign','22222222222222222222222222222222','active',1,'usr_foreign','usr_foreign',now(),now())`,
			} {
				if _, err := pool.Exec(ctx, query); err != nil {
					t.Fatal(err)
				}
			}
			now := time.Now().UTC().Add(-10 * time.Second).Truncate(time.Microsecond)
			workflowPolicy := RunnerWorkflowPolicy{TenantID: "tenant_cleanup", IntegrationID: "integration_cleanup", InstallationID: 8801, RepositoryID: 8802,
				Owner: "acme", Repository: "api", WorkflowPath: ".github/workflows/claude.yml", AllowedRefs: []string{"refs/heads/main"},
				AllowedEvents: []string{"push"}, RequiredLabels: []string{"self-hosted", "hako"}, AllowedLabels: []string{"self-hosted", "hako"}}
			workflowPolicyJSON := []byte(`{"workflow_path":".github/workflows/claude.yml","allowed_refs":["refs/heads/main"],"allowed_events":["push"],"required_labels":["self-hosted","hako"],"allowed_labels":["self-hosted","hako"]}`)
			if _, err := pool.Exec(ctx, `UPDATE tenant_github_repositories SET actions_runner_workflow_policy=$1::jsonb WHERE tenant_id='tenant_cleanup' AND installation_id=8801 AND github_repository_id=8802`, workflowPolicyJSON); err != nil {
				t.Fatal(err)
			}
			var canonicalWorkflowPolicy []byte
			if err := pool.QueryRow(ctx, `SELECT actions_runner_workflow_policy FROM tenant_github_repositories WHERE tenant_id='tenant_cleanup' AND installation_id=8801 AND github_repository_id=8802`).Scan(&canonicalWorkflowPolicy); err != nil {
				t.Fatal(err)
			}
			policyDigest := sha256.Sum256(canonicalWorkflowPolicy)
			policyHash := hex.EncodeToString(policyDigest[:])
			queuedProvenance := QueuedWorkflowJob{RunID: 9902, RunAttempt: 1, JobID: 9901, WorkflowRef: "refs/heads/main", HeadSHA: strings.Repeat("a", 40)}
			authoritativeRun := AuthoritativeWorkflowRun{TenantID: "tenant_cleanup", IntegrationID: "integration_cleanup", InstallationID: 8801, RepositoryID: 8802,
				Owner: "acme", Repository: "api", WorkflowPath: ".github/workflows/claude.yml", WorkflowRef: "refs/heads/main", HeadSHA: strings.Repeat("a", 40),
				RunID: 9902, RunAttempt: 1, JobID: 9901, Event: "push", HeadRepository: 8802, Labels: []string{"self-hosted", "hako"}}
			if err := VerifyRunnerWorkflowProvenance(workflowPolicy, queuedProvenance, authoritativeRun); err != nil {
				t.Fatalf("valid authoritative workflow provenance was rejected: %v", err)
			}
			for name, mutate := range map[string]func(*AuthoritativeWorkflowRun){
				"fork":                    func(run *AuthoritativeWorkflowRun) { run.Fork = true },
				"pull request target":     func(run *AuthoritativeWorkflowRun) { run.Event = "pull_request_target" },
				"foreign head repo":       func(run *AuthoritativeWorkflowRun) { run.HeadRepository = 9900 },
				"cross tenant":            func(run *AuthoritativeWorkflowRun) { run.TenantID = "tenant_foreign" },
				"wrong authoritative sha": func(run *AuthoritativeWorkflowRun) { run.HeadSHA = strings.Repeat("b", 40) },
			} {
				candidate := authoritativeRun
				mutate(&candidate)
				if err := VerifyRunnerWorkflowProvenance(workflowPolicy, queuedProvenance, candidate); err == nil {
					t.Errorf("%s provenance was accepted", name)
				}
			}
			inbox := githubwebhook.Inbox{Pool: pool, Policy: transaction.DefaultPolicy()}
			const integrationID, webhookSecret = "integration_cleanup", "synthetic-cleanup-webhook-secret"
			webhookHandler, err := hakowebhook.NewTenantHTTPHandler(cleanupWebhookCredentials{secret: webhookSecret}, githubregistry.CredentialAuthorizer{Pool: pool, Policy: transaction.DefaultPolicy()}, inbox, hakowebhook.DefaultMaxBodyBytes)
			if err != nil {
				t.Fatal(err)
			}
			requestWebhook := func(deliveryID string, payload []byte, tamperSignature bool) int {
				req := httptest.NewRequest(http.MethodPost, "/v1/integrations/github/apps/"+integrationID+"/webhook", strings.NewReader(string(payload)))
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-GitHub-Event", "workflow_job")
				req.Header.Set("X-GitHub-Delivery", deliveryID)
				mac := hmac.New(sha256.New, []byte(webhookSecret))
				_, _ = mac.Write(payload)
				signature := mac.Sum(nil)
				if tamperSignature {
					signature[0] ^= 0xff
				}
				req.Header.Set("X-Hub-Signature-256", "sha256="+hex.EncodeToString(signature))
				recorder := httptest.NewRecorder()
				webhookHandler.ServeHTTP(recorder, req)
				return recorder.Code
			}
			postWebhook := func(deliveryID string, payload []byte) {
				if status := requestWebhook(deliveryID, payload, false); status != http.StatusAccepted {
					t.Fatalf("signed workflow_job webhook returned %d", status)
				}
			}
			queuedPayload := []byte(fmt.Sprintf(`{"action":"queued","installation":{"id":8801,"app_id":9900},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"queued","labels":["self-hosted","hako"]}}`, strings.Repeat("a", 40)))
			if status := requestWebhook("043b2c3d-4e5f-4789-8abc-def012345679", queuedPayload, true); status != http.StatusUnauthorized {
				t.Fatalf("tampered workflow_job signature returned %d, want 401", status)
			}
			postWebhook("043b2c3d-4e5f-4789-8abc-def012345679", queuedPayload)
			admittedAt := time.Now().UTC()
			admission, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, admittedAt, nil)
			if err != nil || admission.ActionsRunnerJobsAdmitted != 1 {
				t.Fatalf("admit authorized workflow job: stats=%+v err=%v", admission, err)
			}
			claimTx, err := pool.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			claimed, found, err := githubwebhook.ClaimNextActionsRunnerJob(ctx, claimTx, "runner-cleanup-claim", admittedAt.Add(time.Second), 5*time.Minute)
			if err != nil || !found || claimed.State != "claimed" || claimed.TenantID != "tenant_cleanup" || claimed.JobID != 9901 {
				_ = claimTx.Rollback(ctx)
				t.Fatalf("claim authorized workflow job: job=%+v found=%v err=%v", claimed, found, err)
			}
			if err := claimTx.Commit(ctx); err != nil {
				t.Fatal(err)
			}
			// Model the Resource Plane launch broker's atomic claimed→starting
			// transition, then run the real startup worker over injected providers.
			launchAt := time.Now().UTC()
			if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_jobs SET state='starting',resource_plane_id='rp-tokyo-01',provenance_verified_at=$1,provenance_policy_sha256=$2,updated_at=$1
				WHERE integration_id='integration_cleanup' AND workflow_job_id=9901 AND run_attempt=1 AND state='claimed' AND claim_id=$3`, launchAt, policyHash, claimed.ClaimID); err != nil {
				t.Fatal(err)
			}
			request := githubbroker.ActionsRunnerRegistrationRequest{IntegrationID: claimed.IntegrationID, WorkflowJobID: claimed.JobID, RunAttempt: claimed.RunAttempt, ClaimID: claimed.ClaimID}
			command := githubbroker.ActionsRunnerLaunchCommand{ResourcePlaneID: "rp-tokyo-01", IntegrationID: claimed.IntegrationID, TenantID: domain.TenantID(claimed.TenantID),
				Installation: claimed.Installation, Repository: claimed.Repository, WorkflowJobID: claimed.JobID, WorkflowRunID: claimed.WorkflowRunID,
				RunAttempt: claimed.RunAttempt, WorkflowName: claimed.WorkflowName, JobName: claimed.JobName, HeadBranch: claimed.HeadBranch, HeadSHA: claimed.HeadSHA,
				Labels: append([]string(nil), claimed.Labels...), ClaimID: claimed.ClaimID, LeaseExpires: claimed.LeaseExpires, Owner: "acme", Name: "api", State: "starting"}
			instance := LambdaRunnerInstance{ResourcePlaneID: "rp-tokyo-01", MicroVMID: "microvm-cleanup", Endpoint: "https://microvm-cleanup.lambda-microvm.ap-northeast-1.on.aws",
				ImageIdentifier: "arn:aws:lambda:ap-northeast-1:123456789012:microvm-image:hako-actions-runner"}
			runnerToken, err := githubapp.NewRunnerRegistrationToken("synthetic-github-runner-token", time.Now().Add(time.Hour))
			if err != nil {
				t.Fatal(err)
			}
			events := []string{}
			modelReader := &cleanupModelSecretReader{tenantID: "tenant_cleanup", secret: []byte("sk-ant-cleanup-synthetic")}
			modelResolver, err := modelcredentialbroker.New(pool, transaction.DefaultPolicy(), modelReader)
			if err != nil {
				t.Fatal(err)
			}
			modelCredentialService, err := githubbroker.NewPostgresActionsRunnerModelCredentialService([]string{"rp-tokyo-01"}, pool,
				transaction.DefaultPolicy(), modelReader)
			if err != nil {
				t.Fatal(err)
			}
			modelBroker := githubbroker.HandlerWithActionsRunnerExecutionLifecycle(nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil,
				nil, []string{"spiffe://hako/resource-plane/rp-tokyo-01"}, nil, nil, modelCredentialService)
			modelBrokerClient, err := githubbroker.NewHTTPActionsRunnerModelCredentialClient("https://broker.invalid", &http.Client{
				Transport: cleanupModelCredentialBrokerTransport{handler: modelBroker},
			})
			if err != nil {
				t.Fatal(err)
			}
			modelCredentialIssuer := brokerRunnerModelCredentialIssuer{client: modelBrokerClient}
			foreignIdentity := modelcredentials.RunnerClaimIdentity{IntegrationID: "integration_foreign", WorkflowJobID: 9901, RunAttempt: 1,
				ClaimID: claimed.ClaimID, ResourcePlaneID: "rp-tokyo-01", LambdaMicroVMID: instance.MicroVMID}
			if err := modelResolver.WithCredential(ctx, foreignIdentity, func(context.Context, []byte) error {
				t.Fatal("foreign Tenant integration resolved the model credential")
				return nil
			}); err == nil || modelReader.reads != 0 {
				t.Fatalf("foreign Tenant attempted model Secret retrieval: reads=%d err=%v", modelReader.reads, err)
			}
			guestRoot := t.TempDir()
			writeRunnerScript(t, filepath.Join(guestRoot, "config.sh"), "#!/bin/sh\nprintf '%s' \"$*\" > config.args\ntouch .credentials\n")
			runnerExit := 0
			runnerScript := "#!/bin/sh\ntest \"$ANTHROPIC_API_KEY\" = 'sk-ant-cleanup-synthetic' || exit 77\ntouch model-key-present\nmkdir -p _work/repository _temp _diag\n"
			switch conclusion {
			case "failure":
				runnerExit = 1
			case "cancelled", "timed_out":
				runnerScript += "sleep 30\n"
			}
			runnerScript += fmt.Sprintf("exit %d\n", runnerExit)
			writeRunnerScript(t, filepath.Join(guestRoot, "run.sh"), runnerScript)
			processResults := make(chan RunnerProcessResult, 1)
			processConsumer, err := NewRunnerProcessConsumer(RunnerProcessConfig{RootDirectory: guestRoot, SetupTimeout: time.Second, MaximumRun: time.Minute,
				OnExit: func(result RunnerProcessResult) { processResults <- result }})
			if err != nil {
				t.Fatal(err)
			}
			if conclusion == "timed_out" {
				// Exercise the real timeout path without a one-minute integration test.
				processConsumer.config.MaximumRun = 150 * time.Millisecond
			}
			publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
			if err != nil {
				t.Fatal(err)
			}
			guest, err := NewRunnerGuestRuntime(publicKey, processConsumer)
			if err != nil {
				t.Fatal(err)
			}
			launchPayload := RunnerLaunchPayload{SchemaVersion: 1, WorkflowJobID: command.WorkflowJobID, WorkflowRunID: command.WorkflowRunID,
				RunAttempt: command.RunAttempt, RepositoryURL: "https://github.com/acme/api", RunnerName: "hako-9901-1", Labels: append([]string(nil), command.Labels...)}
			if response := sendRunnerRunHook(guest, instance.MicroVMID, launchPayload); response.Code != http.StatusOK {
				t.Fatalf("bind actual guest /run lifecycle: status=%d body=%s", response.Code, response.Body.String())
			}
			proxyAuth := &cleanupRunnerBootstrapAuth{}
			bootstrapTransport := cleanupGuestBootstrapTransport{guest: guest}
			bootstrapper, err := NewRunnerBootstrapper(proxyAuth, &http.Client{Transport: &bootstrapTransport}, privateKey)
			if err != nil {
				t.Fatal(err)
			}
			startup, err := NewRunnerStartupWorker(RunnerStartupWorkerConfig{OperationTimeout: time.Minute}, startBrokerStub{command: command, events: &events},
				runnerLauncherStub{instance: instance, events: &events}, postgresRunnerBinder{pool: pool},
				cleanupRunnerTokenIssuer{runnerTokenIssuerStub: runnerTokenIssuerStub{grant: githubbroker.ActionsRunnerRegistrationGrant{Token: runnerToken, Repository: githubapp.RepositoryAccess{Owner: "acme", Name: "api"}}, events: &events}, model: modelCredentialIssuer},
				bootstrapper, startupFailureStub{events: &events}, "rp-tokyo-01")
			if err != nil {
				t.Fatal(err)
			}
			if err := startup.Start(ctx, request); err != nil {
				t.Fatalf("start authorized workflow runner: events=%v err=%v", events, err)
			}
			if modelReader.reads != 1 || len(modelReader.tenantIDs) != 1 || modelReader.tenantIDs[0] != "tenant_cleanup" || proxyAuth.calls != 1 || !bootstrapTransport.called {
				t.Fatalf("Tenant model credential/bootstrap boundary mismatch: tenant_reads=%v read_count=%d auth_calls=%d guest_called=%v", modelReader.tenantIDs, modelReader.reads, proxyAuth.calls, bootstrapTransport.called)
			}
			waitForRunnerMarker(t, filepath.Join(guestRoot, "model-key-present"))
			if conclusion == "cancelled" {
				processConsumer.Stop()
			}
			processResult := awaitRunnerProcessResult(t, processResults)
			if arguments, err := os.ReadFile(filepath.Join(guestRoot, "config.args")); err != nil || strings.Contains(string(arguments), "sk-ant-cleanup-synthetic") {
				t.Fatalf("model key leaked to runner registration arguments: args=%q err=%v", arguments, err)
			}
			if processResult.IntegrationID != integrationID || processResult.WorkflowJobID != 9901 || processResult.WorkflowRunID != 9902 || processResult.RunAttempt != 1 {
				t.Fatalf("ephemeral runner result escaped workflow identity: %+v", processResult)
			}
			switch conclusion {
			case "success":
				if processResult.ExitCode != 0 || processResult.Cancelled || processResult.TimedOut {
					t.Fatalf("successful ephemeral runner result mismatch: %+v", processResult)
				}
			case "failure":
				if processResult.ExitCode != 1 || processResult.Cancelled || processResult.TimedOut {
					t.Fatalf("failed ephemeral runner result mismatch: %+v", processResult)
				}
			case "cancelled":
				if !processResult.Cancelled || processResult.TimedOut {
					t.Fatalf("cancelled workflow did not stop the guest process: %+v", processResult)
				}
			case "timed_out":
				if !processResult.TimedOut || processResult.Cancelled {
					t.Fatalf("timed-out workflow did not stop the guest process: %+v", processResult)
				}
			}
			inProgressPayload := []byte(fmt.Sprintf(`{"action":"in_progress","installation":{"id":8801,"app_id":9900},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"in_progress","labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-9901-1"}}`, strings.Repeat("a", 40)))
			postWebhook("143b2c3d-4e5f-4789-8abc-def012345678", inProgressPayload)
			statusStats, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, time.Now().UTC(), nil)
			if err != nil || statusStats.Processed != 1 {
				t.Fatalf("apply GitHub runner assignment: stats=%+v err=%v", statusStats, err)
			}
			payload := []byte(fmt.Sprintf(`{"action":"completed","installation":{"id":8801,"app_id":9900},"repository":{"id":8802,"name":"api","owner":{"login":"acme"}},"workflow_job":{"id":9901,"run_id":9902,"run_attempt":1,"workflow_name":"CI","name":"test","head_branch":"main","head_sha":%q,"status":"completed","conclusion":%q,"labels":["self-hosted","hako"],"runner_id":9810,"runner_name":"hako-9901-1"}}`, strings.Repeat("a", 40), conclusion))
			for _, deliveryID := range []string{"243b2c3d-4e5f-4789-8abc-def012345679", "343b2c3d-4e5f-4789-8abc-def012345679"} {
				postWebhook(deliveryID, payload)
				stats, err := githubwebhook.ProcessBatchWithActionsRunnerAdmissions(ctx, pool, transaction.DefaultPolicy(), 10, time.Now().UTC(), nil)
				if err != nil || stats.Processed != 1 {
					t.Fatalf("apply terminal webhook: stats=%+v err=%v", stats, err)
				}
			}
			var checkConclusion string
			var checkRevision int64
			if err := pool.QueryRow(ctx, `SELECT conclusion,revision FROM github_actions_runner_checks WHERE integration_id='integration_cleanup' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&checkConclusion, &checkRevision); err != nil || checkConclusion != conclusion || checkRevision != 3 {
				t.Fatalf("terminal Check identity/conclusion: conclusion=%q revision=%d err=%v", checkConclusion, checkRevision, err)
			}
			checkQueue := &cleanupCheckQueue{}
			d, err := dispatcher.New(dispatcher.StoreAdapter{Pool: pool, Policy: transaction.DefaultPolicy()}, cleanupCheckPublisher{queue: checkQueue},
				map[string]string{"rp-tokyo-01": "https://sqs.invalid/rp"}, dispatcher.Config{BatchSize: 10, GitHubChecksQueueURL: "https://sqs.invalid/checks"})
			if err != nil {
				t.Fatal(err)
			}
			dispatchStats, err := d.RunOnce(ctx)
			if err != nil || dispatchStats.Claimed != 3 || dispatchStats.Published != 3 || len(checkQueue.deliveries) != 3 {
				t.Fatalf("dispatch queued/in-progress/terminal Check outbox: stats=%+v queued=%d err=%v", dispatchStats, len(checkQueue.deliveries), err)
			}
			var publishedStatuses []githubapp.CheckRunStatus
			var fakeCheckID int64 = 9901
			api, err := githubapp.NewChecksClient(&http.Client{Transport: runnerChecksHTTPTransport(func(r *http.Request) (*http.Response, error) {
				if r.Header.Get("Authorization") != "Bearer ghs_cleanup_check_token" || r.URL.Path != "/repos/acme/api/check-runs" && !strings.HasPrefix(r.URL.Path, "/repos/acme/api/check-runs/") {
					return nil, fmt.Errorf("unexpected GitHub Check target or credential: %s %s", r.Method, r.URL.Path)
				}
				recorder := httptest.NewRecorder()
				recorder.Header().Set("Content-Type", "application/json")
				if r.Method == http.MethodGet {
					_ = json.NewEncoder(recorder).Encode(map[string]any{"check_runs": []any{}})
				} else if r.Method == http.MethodPost || r.Method == http.MethodPatch {
					var request githubapp.CheckRunRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						return nil, err
					}
					if request.HeadSHA != strings.Repeat("a", 40) || request.Conclusion != githubapp.CheckRunConclusion(conclusion) {
						return nil, fmt.Errorf("unexpected Check identity/result: %+v", request)
					}
					publishedStatuses = append(publishedStatuses, request.Status)
					if r.Method == http.MethodPost {
						recorder.WriteHeader(http.StatusCreated)
					} else {
						fakeCheckID = 9901
					}
					_ = json.NewEncoder(recorder).Encode(githubapp.CheckRun{ID: fakeCheckID, Name: request.Name, ExternalID: request.ExternalID,
						HeadSHA: request.HeadSHA, Status: request.Status, Conclusion: request.Conclusion})
				} else {
					return nil, fmt.Errorf("unexpected GitHub API method %s", r.Method)
				}
				return recorder.Result(), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			mintCalls := 0
			rotateDuringMint := false
			checkService, err := githubbroker.NewActionsRunnerChecksService(githubbroker.NewPostgresActionsRunnerChecksResolver(pool, transaction.DefaultPolicy()),
				func(_ context.Context, identity githubbroker.ActionsRunnerCheckIdentity) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
					mintCalls++
					if rotateDuringMint {
						rotateDuringMint = false
						if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET credential_generation=2 WHERE id='integration_cleanup'`); err != nil {
							return nil, githubapp.RepositoryAccess{}, err
						}
					}
					return cleanupCheckToken{value: "ghs_cleanup_check_token", expiresAt: time.Now().Add(time.Hour)}, identity.RepositoryURL, nil
				})
			if err != nil {
				t.Fatal(err)
			}
			checkCredentials := cleanupCheckCredentials{service: checkService}
			if _, _, err := checkCredentials.ActionsRunnerChecksToken(ctx, "foreign_integration", 9901, 1); err == nil || mintCalls != 0 {
				t.Fatalf("foreign Tenant integration reached token mint: mintCalls=%d err=%v", mintCalls, err)
			}
			if _, _, err := checkCredentials.ActionsRunnerChecksToken(ctx, integrationID, 9903, 1); err == nil || mintCalls != 0 {
				t.Fatalf("wrong workflow job reached token mint: mintCalls=%d err=%v", mintCalls, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET status='disabled' WHERE id='integration_cleanup'`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := checkCredentials.ActionsRunnerChecksToken(ctx, integrationID, 9901, 1); err == nil || mintCalls != 0 {
				t.Fatalf("revoked Tenant App reached token mint: mintCalls=%d err=%v", mintCalls, err)
			}
			if _, err := pool.Exec(ctx, `UPDATE tenant_github_integrations SET status='active' WHERE id='integration_cleanup'`); err != nil {
				t.Fatal(err)
			}
			rotateDuringMint = true
			if _, _, err := checkCredentials.ActionsRunnerChecksToken(ctx, integrationID, 9901, 1); err == nil || mintCalls != 1 {
				t.Fatalf("credential rotation during token mint was not fenced: mintCalls=%d err=%v", mintCalls, err)
			}
			checkConsumer, err := githubchecks.New(checkQueue, githubchecks.DBStore{Pool: pool, Policy: transaction.DefaultPolicy()}, checkCredentials, api,
				githubchecks.Config{BatchSize: 10, WaitTime: 0})
			if err != nil {
				t.Fatal(err)
			}
			checkStats, err := checkConsumer.RunOnce(ctx)
			if err != nil || checkStats.Received != 3 || checkStats.Applied != 3 || len(checkQueue.deleted) != 3 || len(publishedStatuses) != 3 || publishedStatuses[len(publishedStatuses)-1] != githubapp.CheckCompleted {
				t.Fatalf("publish durable Actions Runner Checks: stats=%+v statuses=%v deleted=%d err=%v", checkStats, publishedStatuses, len(checkQueue.deleted), err)
			}
			var checkRunID int64
			if err := pool.QueryRow(ctx, `SELECT check_run_id FROM github_actions_runner_checks WHERE integration_id='integration_cleanup' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&checkRunID); err != nil || checkRunID != fakeCheckID {
				t.Fatalf("persisted GitHub Check run ID=%d err=%v", checkRunID, err)
			}
			service, err := githubbroker.NewActionsRunnerCleanupQueueService(pool, transaction.DefaultPolicy(), []string{"rp-tokyo-01"})
			if err != nil {
				t.Fatal(err)
			}
			handler := githubbroker.WithActionsRunnerCleanupQueue(http.NotFoundHandler(), service)
			queue, err := githubbroker.NewHTTPActionsRunnerCleanupQueueClient("https://broker.invalid", "rp-tokyo-01", &http.Client{Transport: runnerCleanupHTTPTransport(func(r *http.Request) (*http.Response, error) {
				cert := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
				r.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}, VerifiedChains: [][]*x509.Certificate{{cert}}}
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, r)
				return rec.Result(), nil
			})})
			if err != nil {
				t.Fatal(err)
			}
			// Both provider operations fail once. Recreate the worker using the
			// same stable worker name between attempts, as after a process restart.
			vmExists, runnerExists := true, true
			terminateCalls, deregisterCalls := 0, 0
			terminate := func(_ context.Context, plane, vm string) error {
				if plane != "rp-tokyo-01" || vm != "microvm-cleanup" {
					t.Fatal("cleanup escaped its pinned provider identity")
				}
				terminateCalls++
				if terminateCalls == 1 {
					return errors.New("synthetic provider failure")
				}
				vmExists = false
				return nil
			}
			deregister := func(_ context.Context, job githubwebhook.ActionsRunnerCleanupJob) error {
				if vmExists || job.TenantID != "tenant_cleanup" || job.RunnerID == nil || *job.RunnerID != 9810 {
					t.Fatal("deregistration preceded termination or used a foreign identity")
				}
				deregisterCalls++
				if deregisterCalls == 1 {
					return errors.New("synthetic GitHub failure")
				}
				runnerExists = false
				return nil
			}
			for attempt := 1; attempt <= 3; attempt++ {
				worker := cleanupWorker(t, queue, terminate, deregister, time.Now)
				processed, err := worker.ProcessOne(ctx)
				if !processed || (attempt < 3 && !errors.Is(err, ErrCleanupDeferred)) || (attempt == 3 && err != nil) {
					t.Fatalf("cleanup attempt %d: processed=%v err=%v", attempt, processed, err)
				}
				var state, code string
				var count int
				var available time.Time
				if err := pool.QueryRow(ctx, `SELECT state,last_error,attempt_count,available_at FROM github_actions_runner_cleanup_outbox WHERE integration_id='integration_cleanup' AND workflow_job_id=9901 AND run_attempt=1`).Scan(&state, &code, &count, &available); err != nil {
					t.Fatal(err)
				}
				if count != attempt {
					t.Fatalf("cleanup attempt count=%d, want %d", count, attempt)
				}
				if attempt < 3 {
					wantCode := "microvm_terminate_failed"
					if attempt == 2 {
						wantCode = "runner_deregister_failed"
					}
					if state != "pending" || code != wantCode || !available.After(now) {
						t.Fatalf("cleanup retry not durable: state=%q code=%q available=%v", state, code, available)
					}
					if processed, err := worker.ProcessOne(ctx); err != nil || processed {
						t.Fatalf("cleanup retried before its backoff: processed=%v err=%v", processed, err)
					}
					// Make the isolated test row due; the Broker still controls
					// lease/retry timestamps, without wall-clock sleeps in CI.
					if _, err := pool.Exec(ctx, `UPDATE github_actions_runner_cleanup_outbox SET available_at=now()-interval '1 second'`); err != nil {
						t.Fatal(err)
					}
				} else if state != "completed" || code != "" || vmExists || runnerExists {
					t.Fatalf("cleanup incomplete: state=%q code=%q vm=%v runner=%v", state, code, vmExists, runnerExists)
				}
			}
			if terminateCalls != 3 || deregisterCalls != 2 {
				t.Fatalf("unexpected provider cleanup calls: termination=%d deregistration=%d", terminateCalls, deregisterCalls)
			}
			worker := cleanupWorker(t, queue, terminate, deregister, time.Now)
			if processed, err := worker.ProcessOne(ctx); err != nil || processed {
				t.Fatalf("completed cleanup was re-executed: processed=%v err=%v", processed, err)
			}
		})
	}
}

type postgresRunnerBinder struct {
	pool transaction.Beginner
}

func (b postgresRunnerBinder) Bind(ctx context.Context, job githubwebhook.ActionsRunnerJob, microVMID string) error {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return err
	}
	bound, err := githubwebhook.BindActionsRunnerMicroVM(ctx, tx, job.IntegrationID, job.JobID, job.RunAttempt,
		job.ClaimID, "rp-tokyo-01", microVMID, time.Now().UTC())
	if err != nil || !bound {
		_ = tx.Rollback(ctx)
		if err != nil {
			return err
		}
		return ErrMicroVMIdentityMismatch
	}
	return tx.Commit(ctx)
}

type runnerCleanupHTTPTransport func(*http.Request) (*http.Response, error)

func (f runnerCleanupHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type cleanupWebhookCredentials struct{ secret string }

func (c cleanupWebhookCredentials) ResolveWebhookCredential(_ context.Context, integrationID string) (hakowebhook.WebhookCredential, error) {
	return hakowebhook.WebhookCredential{IntegrationID: integrationID, Generation: 1, CurrentSecret: []byte(c.secret)}, nil
}

type cleanupCheckQueue struct {
	deliveries []githubchecks.Delivery
	deleted    []string
}

func (q *cleanupCheckQueue) Receive(_ context.Context, limit int, _ time.Duration) ([]githubchecks.Delivery, error) {
	if limit > len(q.deliveries) {
		limit = len(q.deliveries)
	}
	return append([]githubchecks.Delivery(nil), q.deliveries[:limit]...), nil
}

func (q *cleanupCheckQueue) ChangeVisibility(context.Context, string, time.Duration) error {
	return nil
}

func (q *cleanupCheckQueue) Delete(_ context.Context, receipt string) error {
	q.deleted = append(q.deleted, receipt)
	return nil
}

type cleanupCheckPublisher struct{ queue *cleanupCheckQueue }

func (p cleanupCheckPublisher) Publish(_ context.Context, _ string, event outbox.Event) error {
	p.queue.deliveries = append(p.queue.deliveries, githubchecks.Delivery{Body: append([]byte(nil), event.Payload...), ReceiptHandle: event.ID})
	return nil
}

type cleanupCheckCredentials struct {
	service *githubbroker.ActionsRunnerChecksService
}

func (cleanupCheckCredentials) ChecksRunToken(context.Context, domain.WorkloadRun) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	return nil, githubapp.RepositoryAccess{}, errors.New("Workload Run credential path is not expected")
}

func (c cleanupCheckCredentials) ActionsRunnerChecksToken(ctx context.Context, integrationID string, workflowJobID, runAttempt int64) (githubapp.CheckToken, githubapp.RepositoryAccess, error) {
	grant, err := c.service.Issue(ctx, "spiffe://hako/control-plane", githubbroker.ActionsRunnerChecksRequest{
		IntegrationID: integrationID, WorkflowJobID: workflowJobID, RunAttempt: runAttempt,
	})
	if err != nil {
		return nil, githubapp.RepositoryAccess{}, err
	}
	return cleanupCheckToken{value: grant.Token(), expiresAt: grant.ExpiresAt()}, grant.Repository(), nil
}

type cleanupCheckToken struct {
	value     string
	expiresAt time.Time
}

func (t cleanupCheckToken) Value() string        { return t.value }
func (t cleanupCheckToken) ExpiresAt() time.Time { return t.expiresAt }

type runnerChecksHTTPTransport func(*http.Request) (*http.Response, error)

func (f runnerChecksHTTPTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func waitForRunnerMarker(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("ephemeral runner did not write expected marker %q", path)
}

type cleanupRunnerBootstrapAuth struct {
	calls int
	input *lambdamicrovms.CreateMicrovmAuthTokenInput
}

func (a *cleanupRunnerBootstrapAuth) CreateMicrovmAuthToken(_ context.Context, input *lambdamicrovms.CreateMicrovmAuthTokenInput, _ ...func(*lambdamicrovms.Options)) (*lambdamicrovms.CreateMicrovmAuthTokenOutput, error) {
	a.calls++
	a.input = input
	return &lambdamicrovms.CreateMicrovmAuthTokenOutput{AuthToken: map[string]string{"X-aws-proxy-auth": "synthetic-microvm-proxy-token"}}, nil
}

type cleanupGuestBootstrapTransport struct {
	guest  http.Handler
	called bool
}

func (t *cleanupGuestBootstrapTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t == nil || t.guest == nil || request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Path != runnerBootstrapPath ||
		request.Header.Get("X-aws-proxy-auth") != "synthetic-microvm-proxy-token" || request.Header.Get("Cache-Control") != "no-store" {
		return nil, errors.New("invalid fake Lambda MicroVM bootstrap request")
	}
	t.called = true
	recorder := httptest.NewRecorder()
	t.guest.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

type cleanupModelCredentialBrokerTransport struct{ handler http.Handler }

func (t cleanupModelCredentialBrokerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if t.handler == nil || request.Method != http.MethodPost || request.URL.Scheme != "https" || request.URL.Path != "/v1/actions-runner/model-credential" {
		return nil, errors.New("invalid model credential broker request")
	}
	certificate := &x509.Certificate{URIs: []*url.URL{{Scheme: "spiffe", Host: "hako", Path: "/resource-plane/rp-tokyo-01"}}}
	request.TLS = &tls.ConnectionState{PeerCertificates: []*x509.Certificate{certificate}, VerifiedChains: [][]*x509.Certificate{{certificate}}}
	recorder := httptest.NewRecorder()
	t.handler.ServeHTTP(recorder, request)
	return recorder.Result(), nil
}

type cleanupModelSecretReader struct {
	tenantID  string
	secret    []byte
	reads     int
	tenantIDs []string
}

func (r *cleanupModelSecretReader) ReadSecret(_ context.Context, access modelcredentials.SecretAccess) ([]byte, error) {
	r.reads++
	r.tenantIDs = append(r.tenantIDs, string(access.TenantID))
	if string(access.TenantID) != r.tenantID {
		return nil, errors.New("fake Secret reader refused a foreign Tenant")
	}
	return append([]byte(nil), r.secret...), nil
}

type brokerRunnerModelCredentialIssuer struct {
	client *githubbroker.HTTPActionsRunnerModelCredentialClient
}

func (i brokerRunnerModelCredentialIssuer) IssueModelCredential(ctx context.Context, request githubbroker.ActionsRunnerRegistrationRequest, microVMID string) ([]byte, error) {
	return i.client.IssueModelCredential(ctx, request, microVMID)
}

type cleanupRunnerTokenIssuer struct {
	runnerTokenIssuerStub
	model brokerRunnerModelCredentialIssuer
}

func (i cleanupRunnerTokenIssuer) IssueModelCredential(ctx context.Context, request githubbroker.ActionsRunnerRegistrationRequest, microVMID string) ([]byte, error) {
	return i.model.IssueModelCredential(ctx, request, microVMID)
}
