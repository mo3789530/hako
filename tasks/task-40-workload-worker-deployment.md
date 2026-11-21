# Task 40: Workload Worker, Queue, and Artifact Deployment

Status: In Progress. Tasks 63–65 provide the controller/provider library and Task 39 provides an SQS partial-batch adapter plus bounded multi-page recovery sweep primitives. Controller-side Lambda MicroVM auth-token issuance, operation-scoped Ed25519 agent tickets, bounded protocol-v1 client/handler, a fixed Go-test process backend, a Controller-owned encrypted S3 attempt-log writer, the signed Control Plane execution-metadata HTTP client, the composite metadata/profile Resolver, a repository materializer wired to immutable checkout, and deterministic attempt-scoped S3 source archive staging are implemented locally. Tenant-owned Runs now have a broker-only checkout composition: the repository authorizer is derived from validated Control Plane metadata, credentials are requested with the immutable Tenant/Run pin over the mTLS checkout provider, and missing Tenant pins fail closed without shared-token fallback. The agent source endpoint streams and verifies the bounded archive, binds it to full attempt/digest/commit identity, and the fixed Go-test factory verifies the extracted tree digest before execution. Completed redacted Agent logs are fetched using a separate bounded `OperationLogs` ticket, digest-verified, and written to S3 by the Resource Plane Controller; the MicroVM has no S3 credential or direct AWS write path. `HTTPAgentRunner` requires the source archive reader and fails closed if it is not configured. `NewLambdaWorkloadHandler` and `NewLambdaWorkloadRecoveryHandler` now share the same injected Controller composition; recovery composition requires a recovery-capable journal, lease manager, result publisher, and bounded EventBridge settings. Fake-backed tests exercise SQS dispatch → MicroVM launch → signed source transfer/install → fixed process start → status/result/log collection → controller-side artifact write → result publish → confirmed cleanup, and a scheduled recovery sweep over the composed runtime; a foreign-plane command is rejected before any provider call. The optional dedicated Workload command/result queues now have manifest validation, fail-closed Dispatcher routing, separate queue/DLQ/alarm resources, cross-account policy, queue/log permissions, optional exact Secret-read permission, and a separate default-off runtime-policy block. Terraform defines the combined Worker Lambda, private VPC attachment, queue event source, bounded recovery and artifact-cleanup schedules, and alarms, but nothing has been applied or attached in AWS. An environment-validated `cmd/hako-workload-controller-lambda` locally composes those existing providers with AWS clients, bounded Resource Plane-owned Secrets Manager references, a Resource Plane-identity checkout broker client, and separate command/recovery Lambda handlers. `make build-workload-controller-lambda` packages this binary as Lambda ARM64 ZIP. These are local executable contracts only. S3 conditional recovery lease, lease-aware bounded sweep orchestration, and an injectable EventBridge Lambda handler have local tests. Runtime policy scope is based on the AWS Service Authorization Reference: image actions use the configured image ARN, journal/source/artifact permissions are prefix-scoped, and the lease uses its exact object ARN. AWS currently marks `lambda:PassNetworkConnector` and `lambda:ConnectMicrovm` as resource-less actions; these require `Resource: "*"` and remain constrained only by dedicated RP role isolation/profile configuration. VPC/NAT reachability, bucket/KMS resource policies, broker certificate/allowlist provisioning and rotation, in-VM image/git wiring, and Task 61 image/network PoC remain. Priority: P0.

Deployment sequencing, admission shutdown, rollback, DLQ triage, and artifact
retention guidance are documented in the [Workload Controller runbook](../docs/workload-controller-deployment.md).

Local verification refresh (2026-10-18): `GOCACHE=/tmp/hako-go-cache make test-workload-runtime-integration` passed for `internal/workloadcontroller`, `internal/workloadjobs`, and `internal/store/workloads` against the already-running local PostgreSQL. The Task 42 Podman/PostgreSQL E2E also passed on this date. These tests do not resolve the documented live AWS/MicroVM deployment gates.

Local verification refresh (2026-10-24): `make test-workload-runtime-integration`
and `make test-software-factory-e2e` passed against the already-running local
PostgreSQL; the latter requires and exercised Podman. Rebuilt
`build/hako-workload-microvm.zip` from the current source with the public
test-only trusted-key fixture and inspected it as a Dockerfile, public key
file, and statically linked ARM64 Agent ELF. The x86_64 host has no ARM64
binfmt/QEMU runtime, so this does not build or execute the ARM64 Lambda
MicroVM image. No AWS resources or real Secrets were accessed.

OpenTofu local verification refresh (2026-10-18): the dev Resource Plane root
`validate` succeeded and `tofu -chdir=infra/terraform/modules/resource-plane
test` passed all six mock-provider runs (default-off, workspace service,
Actions runner, Workload controller wiring, and malformed-input rejection).
No provider credentials or AWS apply were used.

OpenTofu verification refresh (2026-10-25): `make TERRAFORM=tofu
terraform-test` passed all 28 mock-provider runs across the Resource Plane,
Workspace Storage, GitHub Broker Tenant IAM, and Tenant model credential reader
modules. This verifies the current default-off and opt-in plan contracts; it
does not prove AWS deployment, network reachability, Secret provisioning, or
MicroVM runtime behavior. The regular `make terraform-test` command also works
with the default Terraform binary when an accepted version is installed.

Artifact retention update: the Workload Controller `combined` Lambda now also
accepts a separate EventBridge `artifact-cleanup` envelope and runs a bounded
Resource Plane cleanup batch through the HMAC-authenticated Control Plane API.
Terraform keeps it default-off with the existing Worker deployment, creates a
separate schedule/invoke permission/alarm, injects the explicit batch bound,
and grants `s3:DeleteObject` only under the configured Artifact prefix. It does
not add any DSQL access to the Resource Plane. The local module plan tests
verify the schedule and exact IAM scope. The Control Plane Lambda resolves a
separate per-Plane cleanup HMAC key from its explicit ARN map and receives
`GetSecretValue` only for those ARNs; cross-account Secret resource policy is
an operator-owned prerequisite. Deployment, bucket versioning/lifecycle
review, and live cleanup remain unverified and unapplied.

## Goal

Deploy the Resource Plane controller and per-attempt Lambda MicroVM runtime. The repository now contains a default-off combined Controller Lambda, dedicated queue/DLQ, partial-batch event source, bounded recovery and artifact-cleanup schedules, and scoped IAM contracts. These resources have not been applied to AWS; the production MicroVM worker path is not deployed. See [ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md).

## Local composition progress (2026-10-12)

- `NewLambdaWorkloadController` now builds the same injected controller core for command and recovery handlers. `NewLambdaWorkloadRecoveryHandler` requires a journal that supports bounded recovery listing, a lease manager, a result publisher, and explicit page/lease bounds; local tests run an empty scheduled sweep and reject incomplete wiring.
- `SQSResultPublisher` validates Workload result metadata before sending to a dedicated HTTPS result queue. Standard queues get the JSON result body; FIFO queues receive deterministic Tenant/Run group IDs and content-based deduplication IDs. It must not target the existing Workspace Operation result queue, whose consumer expects a different envelope. The dedicated Workload result queue and its IAM/EventBridge/Lambda wiring are not yet deployed.
- The Resource Plane manifest now accepts an optional paired Workload command/result URL. Dispatcher and its Lambda variant route attempt dispatch/cancel to the dedicated Workload command queue; when absent, they leave the Outbox item retryable and never fall back to Workspace commands. The opt-in Resource Plane Terraform creates separate encrypted command/result queues and DLQs, redrive/age/backlog alarms, exact dispatcher/result-consumer queue policies, and a queue-only worker role that consumes only Workload commands. Control Plane Dispatcher IAM derives `SendMessage` scope from the optional manifest URL. The queue, Lambda, event-source, recovery schedule, and artifact-cleanup schedule contracts are implemented and default-off; no AWS resources are attached or applied.
- `workloadagent.LoadTicketSigningKeyFile` remains available for tightly controlled file-based callers; the Workload Lambda composition accepts six distinct concrete Secrets Manager ARNs for metadata HMAC, separate Artifact cleanup HMAC, broker client certificate/key/CA, and Agent ticket signing key. Secret responses are size-bounded, parsed in memory, errors do not expose values, and temporary byte slices are cleared after parsing. Tests use a fake Secrets Manager API only; they never retrieve real secrets. Runtime IAM and rotation/redeployment procedures are still deployment gates.
- Added `cmd/hako-workload-controller-lambda` with strict `command`/`recovery` mode and bounded Resource Plane-owned profiles. Its local composition connects the official Lambda MicroVM SDK client, S3 journal/source/archive/log stores, scoped result publisher, HMAC metadata client, Tenant-pinned checkout broker, Secrets Manager-backed key/certificate material, and leased bounded recovery handler. The checkout mTLS client pins `spiffe://hako/resource-plane/<id>`; broker checkout now requires an explicit per-Plane allowlist and does not extend OAuth/Checks permissions. Command/recovery function deployment, exact runtime/S3/Secrets Manager IAM permissions, certificate/allowlist provisioning, image/git wiring, and scheduled/event-source mappings remain unapplied.
- Added a `combined` Lambda mode so one dedicated Resource Plane function can accept only its SQS Workload-command envelope or the `aws.events` scheduled recovery envelope. Ambiguous/unknown event shapes fail closed; focused tests exercise both dispatch paths and reject malformed envelopes. This allows one immutable binary/config/role deployment with both Event Source Mapping and recovery schedule while preserving the same leased bounded recovery semantics. Terraform models the function and triggers default-off; they are not attached or applied in AWS.
- Added optional `workload_controller_deployment` Terraform wiring, default `null`: one ARM64 function, batch-size-1 `ReportBatchItemFailures` mapping from the dedicated Workload command queue, bounded recovery and Artifact cleanup EventBridge schedules, and invocation alarms. Inputs require an existing ZIP, HTTPS endpoints, matching six named Secret ARNs, bounded runtime/recovery/cleanup configuration, exact runtime IAM scope, SQS visibility at least six times the function timeout, and at least two reserved concurrency slots so scheduled work cannot occupy the only command slot. Outputs are nullable when disabled. No AWS apply or Secret value is part of this change. VPC/NAT/endpoint reachability, S3/KMS resource policies and bucket versioning, broker certificate/allowlist, in-VM image/git wiring, and live MicroVM isolation remain prerequisites and are not proven by Terraform validation.
- Added mock-provider OpenTofu/Terraform plan tests for both default-off behavior and the complete opt-in queue/Lambda/EventBridge/alarm graph; `make terraform-test` and CI run them without AWS credentials or apply. The test caught that the lease-object ARN validator rejected dotted keys such as `lease.json`; it now permits dots while rejecting `..`, and the opt-in test uses that key form.
- Added a negative plan test that supplies a recovery lease object ARN with a `..` path segment and expects the runtime IAM input validation to reject it. This protects the exact-object policy from accidentally accepting path traversal while retaining valid dotted filenames such as `lease.json`.
- The optional deployment now requires Resource Plane-owned private subnet IDs (minimum two) and one or more dedicated security group IDs, attaches the combined Worker Lambda to those VPC resources, and grants ENI creation only for the configured subnets/security groups and ENI resource. Assign/delete/unassign actions are limited to ENI resources; wildcard remains only for read-only `DescribeNetworkInterfaces`. It deliberately does not create subnets, routes, NAT, VPC endpoints, or security groups. The default-off and opt-in mock plans verify the attachment and IAM statement. Endpoint/DNS/route/security-group reachability still needs an account-specific review; no apply was run.
- The opt-in Worker deployment now looks up every supplied subnet's route table and has a Terraform precondition requiring a `0.0.0.0/0` route targeting a NAT Gateway. This is only a plan-time route-target check: it does not create or modify networking and does not prove NAT health, DNS, SG egress, endpoint policy, or endpoint reachability. The mock-provider test covers the expected route shape; no AWS apply was run.
- `make build-workload-controller-lambda` cross-compiles the command/recovery binary for the Lambda `provided.al2023` ARM64 runtime and packages `build/hako-workload-controller.zip`; this produces an artifact only and does not deploy or enable the worker.

## Work

- Use one Lambda MicroVM allocation per Run attempt. EC2 workers and Fargate Workload workers are excluded. Verify Docker/containerd/Podman capabilities before claiming support.
- Keep Control Plane orchestration limited to scoped DSQL and dispatch access. Resource Plane execution never connects to DSQL. Keep App private-key reads in the trusted credential service; artifact upload is scoped to its exact attempt.
- [Implemented locally] Provision the private artifact bucket/prefix, encryption, lifecycle/retention, public access block, and least-privilege bucket policy through the opt-in `workload_artifact_storage` contract. Review/import of an existing bucket and AWS deployment remain unapplied operational gates.
- Configure queue visibility, heartbeat, redrive/DLQ, concurrency, shutdown/drain, and bounded retries consistently with Run timeout.
- Add queue age/depth, DLQ, handler failure, stale Run, and worker capacity alarms. Keep creation default-off until smoke tests pass.
- Sweep a configured, dedicated S3 journal prefix with bounded pagination; replay only validated recoverable attempt identities through the same idempotent Poll path. Start scheduled sweeps from the first page so failed attempts are retried; do not treat a saved continuation token as a durable work claim.

## Acceptance criteria

- Terraform validate/plan succeeds with worker disabled by default and no unrelated IAM broadening. An opt-in deployment requires private VPC subnet/security-group inputs and scopes Lambda ENI permissions to those resources. No AWS apply without approval.
- Unit/policy tests verify queue, DSQL, secret, S3, KMS, and provider permission scope.
- Local integration verifies worker startup, shutdown, queue retry, Lambda MicroVM adapter with fake provider dependencies, and Podman command smoke tests; these do not prove production MicroVM isolation.
- Unit tests verify bounded/prefix-scoped recovery listing, malformed-key rejection, durable accepted-attempt replay, per-attempt error isolation, and continuation handling. The bounded recovery scheduler, concurrency controls, alarms, and IAM contracts now exist in default-off Terraform; deployment, DSQL grants, and live lease behavior remain unverified.
- Recovery library caps page size/pages per invocation, isolates per-attempt failures, and treats continuation tokens as invocation-local only. Local coordination now supports an S3 `If-None-Match` lease claim, ETag-fenced renewal/expiry takeover/release, a bounded sweep that cancels on lease loss, and an EventBridge Lambda adapter with required page/lease limits. A scheduled deployment must restart at the first page and configure the exact lease-object IAM separately from the journal listing prefix; provider composition, alarms, and deployed lease behavior remain unverified.
- Agent logs are retained redacted and bounded in the MicroVM, then fetched by the Resource Plane Controller through a five-minute `OperationLogs` ticket. The client validates content type, exact size, 16 MiB bound, and SHA-256 before the Controller-side `S3AttemptLogWriter` stores them under `<prefix>/<tenant>/<run>/<attempt>/logs/<sha256>.txt` with S3 checksum and SSE-S3/SSE-KMS. Do not grant S3 permissions to the MicroVM or put AWS credentials in its image. The authenticated relay and writer are composed in the local Worker Lambda path, and optional Terraform scopes `s3:PutObject` to the configured artifact prefix and KMS key. AWS deployment, bucket/KMS resource-policy review, and live write verification remain open.
- Verification: `go test -race ./internal/workloadagent ./internal/workloadcontroller ./internal/workloadjobs -count=1` and `go test ./...` pass. These tests validate the writer's AWS request and artifact contract with a fake client; they do not validate deployed bucket policy, KMS grants, or the MicroVM image.
- `Poll` now verifies and uses the frozen specification payload for an already-frozen attempt, so result recovery does not need to re-fetch repository credentials after revocation. `Ensure` still resolves the trusted source/authorization again before an uncertain launch retry and rejects drift.
- An explicitly approved provider PoC verifies MicroVM isolation, default-deny egress, credential scoping, launch/cancel/idempotency, attempt-scoped artifacts, deadlines, and confirmed allocation cleanup.
- Runbook covers deployment order, disabling creation, DLQ redrive, artifact retention, and rollback.
- Run creation remains default-off until worker readiness is validated.

## 2026-10-24 Artifact storage progress

The Resource Plane module now has a separately opt-in `workload_artifact_storage`
configuration. It creates the configured artifact bucket with enforced owner
controls, full public-access blocking, versioning, default SSE-S3 or the
already configured artifact KMS key, TLS-only policy, and a Controller-role
grant limited to Put/Delete under the exact artifact prefix. Current and
noncurrent versions expire at the configured retention period; expired delete
markers are removed and incomplete multipart uploads are aborted after one
day. Bucket ownership is not adopted implicitly, and `force_destroy` is
disabled. Outputs expose bucket ARN/name. AWS plan tests verify default-off
and enabled bucket security/lifecycle/IAM properties. The change is unapplied;
existing-bucket import, KMS resource-policy review, and deployment wiring remain
operational gates.

## 2026-10-25 Local verification refresh

`make build-workload-controller-lambda`, `make test-workload-runtime`, and
`make test-workload-runtime-integration` passed. The build produced the ARM64
Lambda ZIP; the integration suite used the already-running local PostgreSQL.
These checks validate the local binary, queue/controller composition, and
PostgreSQL lifecycle only. They do not validate AWS IAM, VPC/NAT reachability,
Secrets Manager provisioning, ARM64 MicroVM execution, or deployment. The
separately authorized artifact download path is tracked in [Task 87](task-87-workload-artifact-download.md).
