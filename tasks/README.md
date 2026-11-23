# Hako Implementation Task Board

このディレクトリは実装タスクのSystem of Recordです。全体像は[実装まとめ](../docs/implementation-summary.md)、runtime配置は[実行アーキテクチャ](../docs/execution-architecture.md)、コンポーネント仕様は各`docs/`を参照してください。

- [Task 84 — Workspace Service runtime](task-84-workspace-service-runtime.md): local fake-engine/PostgreSQL lifecycle and Podman internal-network acceptance passed on 2026-11-22. Remaining acceptance is real ARM64 nerdctl/containerd and network/cgroup behavior inside the deployed Lambda MicroVM.

- [Task 90 — AgentCore Runtime Coding Agent](task-90-agentcore-coding-agent.md): AgentCoreをAgent Runの追加実行バックエンドとして実装し、既存の変更提案・独立検証・PR公開フローに接続する。default-off、AWS上の隔離・資格情報・cleanup検証は本番有効化の条件。[基盤の現状とマイルストーン](../docs/coding-agent-platform.md)。
- Task 90 の未決定事項と実装順： [91 — Runtime・作業領域](task-91-agent-runtime-and-workspace.md) → [92 — ハーネス・モデル](task-92-coding-agent-harness-model.md)（deployment model allowlist/Region/budget validation、bounded HTTP Runtime、ARM64 image recipe implemented; Kiri image build is root/DNS-blocked）、[93 — ソース・変更提案](task-93-agent-source-proposal-contract.md)（ローカル実装・受入完了、AgentCore実行時のsource handoffは未接続）→ [94 — 言語検証・Terraform](task-94-coding-agent-verification-terraform.md)、[95 — 隔離・資格情報](task-95-agent-isolation-and-credentials.md) → [96 — AgentCore運用・E2E](task-96-agentcore-lifecycle-operations.md)。92と93、94と95は依存を満たした後に並行可能。

- [Task 97 — Workspace GitHub Repository Option](task-97-workspace-github-repository.md): local implementation/PostgreSQL acceptance complete。`hako create --repository`、Tenant-scoped immutable checkout、mTLS Broker→Guest compositionとlocal-edit-safe retryを実装。Optional Terraform checkout wiringのmock planは5つのSecret ARNを完全一致で検証。実KVM Guest/Gatewayでfirst checkout、readback、同一archive retry、編集保持、cold suspend/resume後のcheckout永続性を受入。deployed AWS runtime gateは残る。

- [Task 98 — Local Workspace MicroVM Runtime](task-98-local-workspace-microvm.md): host-local runtimeをCloud Hypervisor/KVMへ移行。v54.1の実KVM lifecycle、実Hako initrd、Controller ensure/suspend/resume/restart/retry/deleteを確認済み。interactive channels・checkoutのCloud Hypervisor受入は未実施。Lambda-managed MicroVMのAWS acceptanceはTask 99で追跡。[現行Cloud Hypervisor runbook](../docs/local-workspace-cloud-hypervisor.md)、[過去のFirecracker記録](../docs/local-workspace-microvm.md)

- [Task 99 — Managed Runtime Routing and Acceptance](task-99-managed-runtime-acceptance.md): GitHub Actions・Claude Code Action・VS CodeのLambda MicroVM経路と、Coding AgentのAgentCore経路をステージングで検証する（ローカルunit/PostgreSQL fake-provider lifecycle suite成功。実サービスのステージング受入は未検証）。

Task 90/96 Worker follow-up (2026-11-22): added a fakeable
`AgentCoreWorkloadWorker` that acquires the durable attempt lease, invokes at
most once, checkpoints a terminal result, confirms cleanup, then publishes.
Lost invocation responses fail closed; cleanup retries do not reinvoke; a
published duplicate delivery is a no-op. Focused worker tests, Workload race
tests, and tagged PostgreSQL integration tests pass. The 2026-11-23 deployment
follow-up composes the bounded SQS handler, AWS Runtime adapter, mTLS Broker/S3
adapters and default-off ARM64 Lambda with scoped Terraform IAM. A cancellable
background attempt-lease heartbeat is locally implemented and fake-tested;
independent verification, Runtime image build, and deployed isolation evidence
remain open.

Task 90 brokered-input follow-up (2026-11-22): added a local resolver that
restores the frozen Workload spec, re-resolves trusted Issue task context,
checks repository/install/branch/commit pins, and reads/verifies only the exact
attempt's source archive before building the AgentCore request. Fake tests
reject task drift, foreign attempts, and archive digest/byte changes; runtime,
PostgreSQL integration, and race suites pass. Provider and deployment wiring
remain.

Task 90/96 AWS provider follow-up (2026-11-22): added a fake-tested adapter
using the official AgentCore Go SDK for frozen-target invocation and session
stop. SDK retries are disabled per request to avoid replaying a non-idempotent
Invoke; response streams are closed and capped at 12 MiB, and cleanup confirms
only a successful response for the exact session. `go test
./internal/workloadagentcore -count=1` passes. No AWS API was called. SQS/Lambda
composition, lease renewal, broker/S3 adapters, and deployed acceptance remain.

Task 90/96 SQS adapter follow-up (2026-11-23): added a strict, bounded Lambda
SQS handler for attempt-only messages with partial batch failure reporting and
fixed redacted logs. Fake tests cover malformed and duplicate JSON fields and
retry only for the failed record. The handler is not composed into the Lambda
binary; durable dispatch and SQS publication remain open.

Task 50 log-boundary follow-up (2026-11-22): found and fixed the local
Firecracker Workspace Guest PID 1 exception to the fixed-failure logging rule.
Startup logs now expose only a bounded stage label; a regression test rejects
path/token details in output. Unit/race tests and `git diff --check` pass.

Task 50 static-analysis follow-up (2026-11-22): fixed mutex-copy warnings in
AgentCore lease test fixtures and isolated the two standalone Firecracker smoke
programs from the default Go package using build tags. `go vet ./...` passes;
their documented named-file `go run` entrypoints still compile and display
usage when arguments are omitted.

Task 50 dependency follow-up (2026-11-22): `govulncheck` found newly reachable
Go `net/http` and Echo advisories. Go is now 1.27.4 and Echo is v5.4.0; build
images, the pinned ARM64 toolchain digest and test fixtures were updated.
`govulncheck` reports zero reachable issues, the complete serial Go unit and
database integration suites and `go vet ./...` pass. Kiri could pull the updated
E2E image, but the explicit Kiri E2E container did not start because this host
requires root for Kiri and sudo is not configured for non-interactive use. The
full integration target used its default container backend.

## AWS環境待ちの受入ゲート

2026-11-21のこの実行ではAWS/GitHubの書き込みやSecret取得を行わず、次の本番・実機受入を明示的にブロックとして記録した。ローカル実装とテストは各タスク記載の状態で継続する。

- [Task 61](task-61-workload-runtime-selection.md)・[39](task-39-workload-execution-handler.md)・[40](task-40-workload-worker-deployment.md): ARM64 Lambda MicroVMのimage/network/isolation、実際のRun/cancel/cleanupとAWS配備。
- [Task 84](task-84-workspace-service-runtime.md): ARM64 Workspace Service containerd execution, effective cgroups/network isolation, and Lambda MicroVM deployment smoke.
- [Task 43](task-43-workspace-runtime.md)〜[47](task-47-developer-ux.md): EFS、Workspace Agent、Gateway、ARM64 Guest imageとLambda MicroVM経路の実機受入。
- [Task 41](task-41-github-checks.md)・[57](task-57-tenant-github-credential-resolver.md)・[59](task-59-tenant-github-migration-iam.md): 証明書・cross-account IAM/Secret/KMS・本番migrationやnetwork provisioning。
- [Task 95](task-95-agent-isolation-and-credentials.md)・[99](task-99-managed-runtime-acceptance.md): AgentCoreの実効IAM/egress隔離と、承認済みstagingでのmanaged runtime E2E。

## 全ローカル統合テストの再確認（2026-10-25）

Task 97 Broker follow-up (2026-11-21): added a Resource Plane mTLS endpoint
that accepts only Tenant/Workspace identity, checks the active Workspace
repository pin and Tenant App binding, mints a single-repository installation
token, resolves/persists the immutable default-branch commit SHA, and rechecks
the pin after token minting. Unit, HTTP, PostgreSQL resolver tests and
`make test-integration-local` pass. No AWS service was contacted.

Task 97 Guest follow-up (2026-11-21): added a fixed-path, bounded
`install_repository` Guest protocol operation. The archive includes `.git`
but no broker token; the Guest verifies the archive digest and detached HEAD,
installs atomically, accepts identical retries, and refuses to replace edits.
Focused source/protocol/Guest tests pass. The Resource Plane still does not
compose fetch, archive transfer and result reporting.

Task 97 runtime follow-up (2026-11-21): Resource Controller now composes mTLS
Broker token retrieval, one-repository Git fetch, credential-free archive
transfer to the Lambda MicroVM Guest, and signed checkout result reporting.
Already-pinned retries retain the immutable commit if the remote default branch
advances. Terraform exposes optional endpoints and exact Secret ARNs, and grants
`GetSecretValue` only on the five configured Secrets. `make test-integration-local`
and `tofu -chdir=infra/terraform/modules/resource-plane validate` pass. The
configured Resource Plane test file also passes with `tofu test
-filter=workload-controller.tftest.hcl`. No AWS APIs or Secrets were accessed.
Suspend/resume MicroVM lifecycle acceptance remains open.

Task 97 lifecycle follow-up (2026-11-21): a composed Resource Runtime test now
executes the initial repository checkout, models a Workspace edit, and confirms
Suspend/Resume does not request another token or reinstall the repository.
Guest filesystem tests separately prove identical checkout retries preserve
local edits. This is local fake-provider evidence; the same behavior on an
actual persistent MicroVM disk and Gateway remains unverified.

Task 97 configured-plan follow-up (2026-11-22): tightened the Resource Plane
mock-plan assertion to compare the exact five Workspace checkout Secret ARNs
and `GetSecretValue` action, rather than checking only the number of resources.
`tofu fmt -check` and `tofu test -filter=workload-controller.tftest.hcl`
pass (11 runs). This completes the local Terraform acceptance; persistent
MicroVM/Gateway checkout behavior remains an AWS runtime gate.

Task 98 disk follow-up (2026-11-21): the host smoke now attaches a 64 MiB
temporary raw virtio disk, writes a marker from one Firecracker Guest, stops
that VM, and confirms a second Guest reads the marker from the same file.
The smoke passed on Firecracker v1.18.0 with the default seccomp configuration
and the host kernel. It proves disk reattachment/persistence only, not a
filesystem mount or Hako Workspace lifecycle.

Task 98 filesystem follow-up (2026-11-21): updated the local Firecracker smoke
to format the disk as ext4, mount it at `/workspace`, create a marker file,
stop the VM, then remount the same disk in a second VM and verify the file.
The test passed with Firecracker v1.18.0 and the host kernel (`CONFIG_EXT4_FS=y`).
It still does not compose the Hako Guest/controller/Gateway or CLI lifecycle.

Task 98 journal follow-up (2026-11-21): added the Linux host-local
`workspacejournal.FileStore` as a durable allocation-record foundation. It
preserves the shared journal's Workspace revision/tombstone contract and adds
private identity directories, per-record `flock`, atomic replacement, fsync,
and bounded deterministic listing for future controller reconciliation.
Focused `go test ./internal/workspacejournal -count=1` and `git diff --check`
pass. This does not yet persist a Firecracker process identity or reconcile VM
state; controller and Guest integration remain open.

Task 98 runtime orchestration follow-up (2026-11-21): added
`workspaceruntime.LocalMicroVM`, implementing `resourcecontroller.Runtime` and
`ResourceBindingReporter` over the durable journal and a trusted idempotent
local-driver contract. It checkpoints a deterministic allocation ID before
driver effects, reuses immutable launch JSON on resume/retry, and preserves
revision/tombstone fencing. Fake-driver lifecycle tests cover runtime
reconstruction, duplicate operations, binding reporting, and delayed ensure
rejection after delete. `go test -race ./internal/workspacejournal
./internal/workspaceruntime -count=1` and `git diff --check` pass. This is not a
concrete Firecracker driver or Guest/Gateway/CLI integration.

Task 98 retry follow-up (2026-11-21): fake-driver tests now inject lost
responses after ensure, suspend, resume, and delete have already changed the
provider state. Reconstructed runtimes retry the same durable operation and
converge to its terminal observed state. The `FileStore` journal keeps the
immutable launch parameters and operation/tombstone fence throughout. The
repeat `make smoke-local-workspace-microvm-boot` with the checksum-verified
Firecracker v1.18.0 binary also passed, as did the complete
`make test-integration-local` suite. No concrete Hako Firecracker driver or
AWS/GitHub calls were involved.

Task 98 operation serialization follow-up (2026-11-21): added a
process-shared `FileStore.LockOperation` separate from record CAS locks and
require it in `LocalMicroVM`. The runtime holds the allocation lock across
journal read/compare-and-swap, provider side effects, and terminal checkpoint,
preventing concurrent controller processes from racing lifecycle changes.
Tests cover cross-FileStore lock exclusion/cancellation and concurrent duplicate
ensure from two runtime instances; the complete `make test-integration-local`
suite, focused race tests, and `git diff --check` pass. Concrete Firecracker
process/reconciliation and Guest integration remain.

Task 98 restart reconciliation follow-up (2026-11-21): added bounded
`LocalMicroVM.Reconcile`. It discovers journal records, reacquires each
Workspace's process-shared operation lock, re-reads the authoritative record,
and invokes the idempotent driver to restore a missing Running allocation or
confirm a Suspended allocation remains stopped. Deleted records remain
tombstones; provider-terminated records wait for explicit ensure/delete. Tests
simulate controller process state loss while keeping the FileStore journal and
verify convergence. Focused race tests and the complete
`make test-integration-local` suite pass. A concrete Firecracker driver and a
startup-loop caller are still required for actual reconciliation.

Task 98 concrete Firecracker driver follow-up (2026-11-22): implemented the
Linux `FirecrackerLocalDriver` against the pinned v1.18 API shape. It checks
KVM API support at construction, uses an allocation-derived Unix API socket,
launches with Firecracker's default seccomp, pins kernel/initrd SHA-256, creates
and verifies a private labeled ext4 Workspace disk, records a PID with boot ID
and process start ticks, requests a Guest orderly shutdown on suspend,
reconstructs the driver and cold-boots from the same disk on resume, and removes
process/disk state on delete. Suspend accepts only a Guest quiescence marker or
Firecracker's successful orderly exit after `SendCtrlAltDel`; resume cold-boots
the same data disk. `make smoke-local-workspace-microvm-boot` passed
with real KVM and Firecracker v1.18.0, including the concrete
ensure/suspend/process-stop/fresh-driver/resume/delete lifecycle and same-disk
file persistence. The driver has no Guest networking/jailer and is not wired
into a local Resource Controller startup loop yet. Focused race tests,
`make test-integration-local`, and `git diff --check` pass.

Task 98 local startup recovery follow-up (2026-11-22): added a cancellable
immediate/periodic `RunLocalReconcileLoop` and the Linux
`cmd/hako-local-workspace-reconciler` executable. It validates owner-private
launch JSON, checks KVM, reconstructs FileStore/Firecracker, reconciles at
startup and retries without stopping on one allocation's failure. Added
`make test-local-workspace-reconciler` and
`make build-local-workspace-reconciler`; both pass with focused race tests.
This restores journaled allocations but still has no local operation ingress
or API/deployment composition. Then `make test-integration-local` passed across
all Go packages, and a live KVM startup→SIGTERM smoke confirmed the reconciler
opens the host journal/driver and exits cleanly with an empty journal. No AWS
configuration or request was used.

Task 98 host-private Guest transport follow-up (2026-11-22): each local
Firecracker VM now receives one vsock device. The host dialer validates the
owner-private UDS and speaks Firecracker's `CONNECT`/`OK` handshake while
allowing its assigned host-side port; callers can only request the fixed
Workspace Agent port. The disposable KVM Guest loads matching vsock modules,
binds that fixed port, and responds to ping/pong. The concrete driver smoke
confirms the transport before suspend and after cold resume from the same
disk. The Gateway has not yet been wired to this dialer. `make
smoke-local-workspace-microvm-boot` and focused race tests pass.

Task 98 Workspace Agent vsock follow-up (2026-11-22): added a Linux
`net.Listener` for AF_VSOCK and an opt-in `HAKO_WORKSPACE_AGENT_VSOCK_ENABLED`
setting. When enabled by trusted Guest configuration, the unprivileged
Workspace Agent serves its existing HTTP/WebSocket handler on vsock port 7000
alongside its inherited Unix listener; signed Agent ticket verification stays
inside the same handler. Lambda image behavior remains disabled by default. The
disposable real-KVM Guest now runs this same listener implementation and the
host smoke sends HTTP health requests before suspend and after cold resume.
Focused Agent/init tests, race tests, Firecracker lifecycle/filesystem smoke,
and `git diff --check` pass. Gateway routing and a supported Hako Guest image
remain unconnected.

Task 98 vsock handshake follow-up (2026-11-22): the host dialer now reads the
Firecracker ACK one byte at a time so buffered handshake parsing cannot consume
Guest application bytes sent immediately after `OK`. A regression test writes
the ACK and initial Guest payload together and confirms the payload remains on
the returned stream. The focused test and complete `make test-integration-local`
pass. Real KVM smoke, race tests, and the configured Resource Plane OpenTofu
tests also pass after the fix; no AWS API was contacted.

Task 98 local Gateway route follow-up (2026-11-22): added a `firecracker-local`
Gateway runtime option that selects the host FileStore without requiring an
S3 journal or AWS Region, resolves only running allocations on its configured
Resource Plane, and routes signed, permission-checked Agent streams to the
fixed Guest vsock endpoint. Local Gateway config rejects S3 journal mixing and
requires clean absolute state/journal paths; Lambda MicroVM remains the
default. Gateway configuration tests and the local vsock directory WebSocket
test pass. After this wiring, `make test-integration-local` passed all Go
packages and `make smoke-local-workspace-microvm-boot` passed the real KVM
lifecycle/filesystem/vsock smoke. Supported Guest image, operation
API/deployment, and full CLI lifecycle remain open; no AWS API was contacted.

Task 98 local Guest init follow-up (2026-11-22): added
`cmd/hako-local-workspace-guest-init` as a Linux PID 1 entrypoint for a local
Firecracker Guest. It accepts only strict Tenant/Workspace/Plane/allocation
identity and HTTPS Gateway-origin kernel arguments, mounts the fixed ext4
`/dev/vda` at `/workspace`, and starts the existing non-root Workspace Agent
with fixed-port vsock and SSH. The distinct local-disk mount descriptor is
rejected by the Lambda EFS `/run` hook. The ARM64 Workspace image now contains
the guest-init binary and has a validated build-time vsock opt-in. Parser,
mount-policy, Workspace Agent/init tests and race tests pass; an ARM64 static
cross-build succeeds. `FirecrackerLocalWorkspaceLaunchResolver` injects only
trusted operation identity and the journal's deterministic allocation
generation into the Guest boot args; it rejects reserved args from the base
profile and permits a replacement only after confirmed termination. Resolver
tests cover first allocation, replacement generation, and unsafe inputs. A
supported KVM rootfs/initrd build and full local operation/CLI composition
remain open. `go test ./... -count=1`, the local Guest/Agent/runtime race
tests, the ARM64 static guest-init cross-build, and `git diff --check` pass;
the disposable KVM smoke still does not boot the supported Hako Guest rootfs.

Task 96 stop-token follow-up (2026-11-22): added a deterministic
`StopClientToken` bound to the full immutable AgentCore attempt identity for
retries of `StopRuntimeSession`. Official API review confirms `InvokeAgentRuntime`
has no client-token field, so ambiguous invocation responses still require a
durable in-flight fence and cannot be made safe by replaying the request. Added
`ClaimInvocation`, a journal CAS from `Launching` to `Running` before provider
invocation; a lost response remains claimed across process restart and cannot
be invoked twice. `PreparedInvocationFromJournal` strictly decodes and validates
the pinned Runtime ARN, qualifier, and attempt session for post-restart
recovery, rejecting unknown fields and mismatched sessions. `GOCACHE=/tmp/hako-go-cache go test -race
./internal/workloadagentcore -count=1` passes. This is API-contract/unit
evidence only and did not contact AWS. A recovery-focused test reloads the
claimed journal and confirms an ambiguous invocation cannot reclaim the call
after dispatcher restart.

`make test-integration-local`を2026-10-25に再実行し、`go test -tags=integration -p=1 ./... -count=1`が全パッケージで成功した。既に起動中のローカルPostgreSQLを使用し、テスト後も`hako-dev-postgres`が稼働中であることを確認した。直近の対象コードにはloopback-only OpenSSH/Workspace host-key実装と、sshd listener readiness待機・起動失敗時cleanupを含む。その後のfocused SSH testでは、ローカルOpenSSH 10.2p1がWorkspaceのsshd設定を`sshd -t`で受理することを確認済み。これはローカル統合受入の証拠であり、AWS配備、Lambda MicroVM上の実行、Secrets取得やGitHub/AWS外部接続の証拠ではない。

Task 84 host-network smoke is repeatable with `make test-workspace-podman-network` (pinned x86_64 Alpine, positive TCP egress control, and `--internal` denial). It validates Podman bridge behavior only; it does not replace nerdctl/CNI ARM64 image or Lambda MicroVM verification.

Local suite refresh (2026-10-25): `make test-integration-local` passed across
all packages against the already-running PostgreSQL. `make
test-software-factory-e2e` passed with Podman required, including the composed
Worker success/cancellation paths; `make test-workspace-podman-network`
confirmed positive egress on the ordinary bridge and blocked egress on an
internal bridge. These are local/fake/host-Podman acceptances, not proof of
ARM64 nerdctl or Lambda MicroVM behavior. PostgreSQL remained running.

After adding the Issue-command-to-Native-Agent proposal composition, reran
`make test-integration-local`; all packages passed, including the new
PostgreSQL-backed Agent task/proposal path. The API OCI image smoke also passed
with Podman (`/healthz` 200 and unauthenticated `/v1/health` 401). Neither check
exercises AWS Lambda Web Adapter event conversion or a deployed MicroVM.

Coverage follow-up (2026-10-25): `make test-coverage` passed with Podman
required and reported 70.8% repository-wide statement coverage. The separate
API package run reported 75.6%; new tests pin six invalid-request JSON error
envelopes (workspace, Workload, GitHub Installation, artifact access, Service
action, and quota). The CLI package reported 66.7% in its integration-tagged
package run. Coverage identifies untested code but does not itself establish
behavior outside these local tests.

Tenant Webhook adapter follow-up (2026-10-25): added a PostgreSQL composition
test for `TenantGitHubWebhookCredentialResolver` and
`TenantGitHubWebhookAttemptLimiter`. The secret path uses only a synthetic
STS/Secrets Manager HTTP endpoint, verifies exact Webhook-purpose Role/Secret/
Version pins, and rejects unknown integration or nil DB. The limiter verifies
the database-backed admission boundary. `make test-tenant-github-apps-integration`
includes these tests; it does not retrieve real secrets. API integration
coverage increased to 76.0% in the focused package run. Corrected stale Task 58
text that claimed OAuth broker/API/CLI wiring was still missing; Tasks 66/67/72
contain that local implementation, while production certificate/IAM/network
provisioning remains open.

Task 47 CLI follow-up (2026-10-25): authenticated HTTP tests now cover
Workspace Service add/list, request route/body/result, and missing login/API URL
failure paths. `go test -tags=integration -cover ./cmd/hako -count=1` reports
67.3% statement coverage. Service runtime remains subject to the Task 84
ARM64 nerdctl/Lambda MicroVM gate.

Task 47 follow-up: the `hako service <action>` command handler now has a
directly testable boundary. Tests verify the authorized `start` API request,
exact-name `remove` cancellation before network access, and that `--yes` is
restricted to `remove`. The latest integration-tagged CLI coverage is 68.1%;
focused CLI race tests pass. This is desired-state/API evidence, not proof of
the deployed MicroVM Service executor.

Task 47 basic Workspace CLI follow-up (2026-10-25): authenticated HTTP-backed
tests now cover `createWorkspace`, `listWorkspaces`, and `getWorkspace`,
including the create idempotency key/body, Tenant-scoped pagination, detail
route, missing login/API URL, and forbidden Tenant response. Focused
`TestWorkspaceBasicCLI` tests pass. This pins the client contract; deployed
MicroVM/Gateway behavior remains an external runtime gate.

Task 41 local coverage follow-up (2026-10-25): the PostgreSQL Run lifecycle
test now verifies the durable GitHub Check progression from `queued` through
`in_progress` to `completed`, with Outbox events emitted atomically for each
visible Check update. This does not verify deployed queue delivery or live
GitHub access.

Task 47 CLI dispatch follow-up (2026-10-25): `hako create`, `hako list`, and
`hako get` now share a directly testable handler with the real process entry
point. HTTP-backed tests cover output, paging, invalid arguments before API
access, and output-write failure handling.

Task 47 lifecycle CLI follow-up (2026-10-25): `suspend`, `resume`, and `delete`
now use a directly testable handler called by the process entry point. Tests
cover authenticated action bodies/idempotency, exact Workspace ID confirmation
before delete, `resume --wait` reporting, and fail-before-request behavior for
cancelled or invalid actions.

Task 50 correlation follow-up (2026-10-25): Workspace Service API request IDs
now pass through the transactional Outbox, signed Resource Plane desired-state
lookup, and validated failure logs. PostgreSQL API E2E and Service runtime
integration cover the path; other per-worker/API correlation gaps and deployed
log operations remain open.

Task 52 workflow policy read API was added on 2026-10-25. Tenant members can
inspect the active repository allowlist through the authenticated API/CLI;
configuration remains Owner/Admin-only. API/CLI and PostgreSQL integration
tests cover authorization and disabled-repository hiding. Runner admission and
execution remain default-off.

Task 53 Native Agent file access was hardened on Linux against concurrent
symlink replacement during reads and atomic writes. Descriptor-relative
`openat`/`O_NOFOLLOW` operations and bounded reads keep repository test code
from redirecting model file tools between path validation and use; unit and
race tests cover the Agent path. Target MicroVM behavior remains unverified.
The Podman Job Service acceptance now also checks that an internal network
cannot establish TCP to the link-local instance-metadata listener; it performs
no metadata HTTP request or credential read.

Task 53 follow-up: `make test-native-agent-e2e` composes a signed Issue command
delivery, PostgreSQL-backed immutable Agent task, and scripted Native Agent
proposal generation. It verifies the executor receives persisted, bounded
task context and creates a proposal only after the fixed test tool passes.
This remains local/fake evidence; Resource Plane Worker-to-MicroVM execution
and production Agent admission are not enabled.

Task 49 API image follow-up: `make test-api-lambda-image-smoke` builds the
amd64 API OCI image with Podman and checks `/healthz` (200) plus unauthenticated
`/v1/health` (401) against the local PostgreSQL service, cleaning up its own
container. Lambda Web Adapter event conversion still requires the candidate
Lambda/API Gateway v2 event check in the designated AWS test account.
The follow-up `make test-api-lambda-rie-smoke` also passes representative
HTTP API v2 events through the pinned Lambda Web Adapter under AWS RIE; it
returns 200 for `/healthz` and 401 for unauthenticated `/v1/health`, and checks
JSON bodies, response headers, and encoding. This covers local event conversion,
not AWS orchestration, IAM, or API Gateway authorization.

Task 49 safety follow-up: `make aws-e2e-preflight` now validates a
strict opt-in, dedicated account/profile allowlist, Management/production
account denylist, run ID, and Region before making read-only STS identity
lookups. `make test-aws-e2e-guard` uses mocked STS and verifies rejected
preflight cases cause zero lookups. It is not connected to mutations or
cleanup. A fake-tested `MutationGate` helper now rechecks account, region, and
run ownership immediately before a supplied callback; actual AWS operations
are not wired through it. Real Workspace E2E and automated cleanup remain open.

Task 61's published contract/cost documentation now reflects the explicit
1–28,800 second `RunMicrovm` API range and separates Dockerfile snapshot builds
from nested guest containers. Hako pricing meters and a Tokyo-safe estimate
formula are recorded; live Lambda MicroVM latency, cgroup/network isolation,
and cost PoC remain open.

Task 61 artifact inspection (2026-10-25): rebuilt/inspected the workload
Agent package and Controller Lambda bootstrap. Both binaries are statically
linked ELF64/AArch64; the Agent ZIP contains only its Dockerfile, binary, and
synthetic test public key. This confirms build/package architecture, not a
Lambda MicroVM image build or guest execution. The test key is not production
usable; runtime/network/isolation PoC remains open.

Task 51 replay API coverage follow-up (2026-10-25): the PostgreSQL API test now
pins malformed/missing/short/oversized reasons, foreign-Tenant integration
hiding, and replay-rate exhaustion (`429` plus `Retry-After`). The focused test
passes locally without GitHub or AWS access.

Task 70 migration API follow-up (2026-10-25): the Tenant GitHub App API test
now composes dry-run and confirmed backfill with Owner/Admin authorization,
App-ID conflict rejection, all-or-nothing behavior, and audit verification.
The focused isolated-PostgreSQL integration test passes; no real credentials
or external cutover are involved.

Task 70 CLI follow-up (2026-10-25): migration commands now reject duplicate,
non-positive, malformed, and over-100 Installation ID lists before network
access. HTTP-backed tests verify dry-run and explicit `--apply` backfill
payloads. `go test ./cmd/hako ./internal/api -count=1` passes.

Task 47 CLI follow-up (2026-10-25): authenticated HTTP tests now cover Tenant
membership and placement-policy GET/PUT, including Tenant identity in the
request and fail-closed missing credential/API URL paths.

Task 89 API follow-up (2026-10-25): the PostgreSQL publication integration
test verifies request-body and malformed-identity rejection before Broker
access, generic error redaction, and a mismatched Tenant response from the
Broker. Successful and failed attempts are audited; invalid requests are not.
It now also composes verifier retry through the API: one replacement attempt
and dispatch Outbox are persisted, the route returns `202 verification_queued`,
and the Broker submitter receives no call until verification is completed.
The same API test verifies `409 verification_not_ready` before cleanup
confirmation and `409 verification_retry_limit` after the initial attempt plus
two retries, without an extra Broker call or dispatch event.
Repeating submit while verification is active reuses the current attempt and
does not duplicate its Outbox command.

Added `make verify-workload-microvm-artifact` so the Agent ZIP check is
repeatable: it validates the public-key JSON, exact package contents, key-byte
identity, and static ELF64/AArch64 architecture. Focused key validation tests
and the target pass with the synthetic fixture; no image execution or AWS
operation is implied.

Task 41 annotation producer follow-up (2026-10-25): failed fixed-Catalog
Native `go-vet` and `golangci-lint` commands now produce at most 50
repository-relative Go annotations and 48 KiB of encoded JSON from a bounded
128 KiB capture. Known Job Service secrets are redacted before extraction;
truncated output and all non-lint command output produce no annotations.
Focused Worker tests cover parsing, redaction, bounds, truncation, and
result-copy isolation. Actions Runner diagnostics remain summary-only.

Task 41 full local integration refresh (2026-10-25): `make
test-integration-local` passed all packages with `-tags=integration -p=1
./... -count=1`, including Worker annotation production, PostgreSQL result
validation/persistence, and fake GitHub Checks delivery. The existing
`hako-dev-postgres` container remained running. This does not verify AWS
deployment or live GitHub delivery.

Task 42/53 focused runtime refresh (2026-10-25): `make
test-software-factory-e2e` passed its required Podman webhook/Check E2E,
Tenant-scoped Run API test, and composed Lambda Worker success/cancellation
tests. `make test-job-services-podman` also passed with the PostgreSQL
sidecar, generated credential authentication, cgroup checks, loopback access,
and denied external TCP probe. PostgreSQL remained running. These are local
host-Podman/fake-provider checks, not ARM64 nerdctl or Lambda MicroVM evidence.

Task 47 CLI follow-up (2026-10-25): Workspace Shell reconnect tests now cover
revoking an expired Session before a failed reconnect, cancellation during
retry backoff, stale-stream race handling, and EOF after terminal input ends.
`workspaceShellReconnect` coverage rose from 59.3% to 85.2%, and `cmd/hako`
coverage rose from 71.6% to 71.9%. `go test -race ./cmd/hako -count=1` and
`go test ./...` passed.

## 着手ルール

1. `P0`から順に、依存タスクが完了しているものを一件だけ選ぶ。
2. 着手前に`git status`、対象コード、タスク記載の仕様を確認する。既存変更と常時起動のPostgresを保持する。
3. タスクの受入条件を実装・テストし、重要な仕様変更は関連`docs/`とREADMEの状態記述へ反映する。
4. AWS `apply`、実AWSリソース削除、秘密情報の取得/表示は別途明示承認がない限り行わない。Podmanを使うローカルテストは許可されているが、DBコンテナを停止しない。
5. 終了時に検証結果・未完了項目・commit hashを報告する。別タスクへ範囲を広げず、次の依存可能タスクを示す。

## 作業中タスクの整理（2026-10-11）

Task 41のGitHub Checks delivery（mTLS broker/consumer、idempotent Check更新、Queue/DLQ/IAM/alarmとdefault-off service Terraform）はローカル実装・検証済みで、production Secret/certificate/network/image/deployment gate待ち。Task 51/78/79のWebhook基盤・operator API/runbook・包括lifecycle PostgreSQL E2Eもローカル検証済み。Tenant GitHub App Tasks 56–72はAPI/CLI/broker/mTLS/Worker checkout composition/local E2Eと、private-key reader ARNをbroker roleだけに許可するTerraform contractまでローカル実装済み。cross-account Role trust/Secret/KMS provisioning、production certificate/IAM/worker deployment、migration/cutoverは未実施。AWS apply、実Secret取得、実GitHub設定変更はしていない。

親タスク57–60/50の残作業をTask 66–71へ分割した。OAuth broker通信をTask 72、Claude Code ActionをTask 73–77、Webhook Task 51の残件をTask 78–79へ分割する。親は集約用に残すため、残件数を数えるとき親と子を二重計上しない。子タスクはリポジトリ全体の残件数ではない。

| 順序 | 実行タスク | 親 | 依存・状態 |
|---|---|---|---|
| 1 | [66 — OAuth trusted broker](task-66-tenant-oauth-broker.md) | 58 | Implemented locally：DB resolver/Secret reader、OAuth/checkout broker compositionあり。production IAM/certificate/deploymentが残る |
| 2 | [72 — OAuth broker mTLS通信](task-72-oauth-broker-mtls.md) | 66 | Implemented locally：TLS 1.3/mTLS、OAuth/checkout adaptersとhandshake testsあり。証明書発行/rotation・production networkが残る |
| 3 | [67 — OAuth API/CLI接続](task-67-tenant-oauth-cli-wiring.md) | 58, 66, 72 | Implemented locally：startup mTLS設定/CLI setup、mTLS broker→API→PostgreSQL callback E2E (fake upstream)通過。production certificate/network wiringが残る |
| 並行可能 | [68 — Webhook abuse制御](task-68-webhook-abuse-controls.md) | 50, 58 | Implemented locally：preflight/DSQL limit/Gateway throttleとunit・PostgreSQL検証済み。alerting/deployment reviewは未完了 |
| 4 | [69 — proof/checkout broker接続](task-69-tenant-checkout-broker.md) | 38, 57, 59 | Implemented locally：mTLS broker API、DB-backed immutable Run pin、exact credential reader、shared no-fallback routing、Resource Plane Lambda Worker compositionとPostgreSQL検証。production process/certificate deploymentは40/66/72の運用ゲート |
| 5 | [70 — binding移行/cutover tooling](task-70-tenant-migration-cutover.md) | 59, 67, 69 | Implemented locally：revision/generation fence付きdry-run/backfill、run drain guard/default選択、rollback/idempotency検証。production cutoverは承認後 |
| 6 | [71 — 二Tenant local E2E](task-71-tenant-app-local-e2e.md) | 60, 67–70 | Implemented locally：並行callback、Tenant別Webhook/Run pin/checkout、replay・cross-App/Tenant拒否、rotation/disable/shared回帰とOCC retry。production rolloutは未検証 |
| 後続 | [73 — Claude Code Action親タスク](task-73-claude-code-action.md) | 52 | Task 74–77のlocal provenance/credential/execution/E2Eは実装済み。pinned image、実MicroVM・GitHub/AWS接続、production rolloutが残る |
| 完了（ローカル） | [78–79 — Webhook運用/E2E](task-78-webhook-platform-operator-dlq.md) | 51 | Operator API/runbookとretention/replay/trigger lifecycle E2EをPostgreSQLで検証。Production運用ゲートは別 |
| 完了（ローカル） | [87 — Workload Artifact Download](task-87-workload-artifact-download.md) | 40, 46 | Short-lived Tenant/Run/artifact/Plane-bound ticket、Gateway直結S3 stream、exact-prefix IAM、CLI SHA-256検証を実装。AWS deployment/live S3 retrievalは未検証 |
| 完了（ローカル） | [88 — Preview Application Log Tail](task-88-preview-application-logs.md) | 54 | Guest内256 KiB bounded log tail、Preview専用read-only Session/Gateway route、CLI取得を実装。実MicroVM/image/Gateway検証はTask 54のAWS gate |

Tenant App子タスクのlocal受入条件はTask 70/71まで実装・検証済み。brokerのWorker接続とbroker専用IAM contractもlocal実装済み。cross-account certificate/Role trust/Secret/KMS provisioningと承認済みcutoverは未完了で、次の実装順は下の「整理後の実行キュー」に集約する。Task 68のローカル実装（固定低カーディナリティEMFとCloudWatch alarm定義を含む）も完了扱いだが、通知先設定・AWS配備レビューは別の運用作業として残る。Terraform配備はAPI/CLIとローカル検証の後段とする。Workspace/EFS/Gateway（43–47）、運用（48–50）、Software Factory拡張（53–55）も未完了。親子タスクは二重計上せず、詳細は各taskの受入条件を参照する。

Preview generation snapshots the eligible Tenant-owned GitHub App identity
(integration, config revision, and credential generation) when its runtime
allocation row is first created. A separate mTLS broker endpoint issues a
Contents-read token only for the exact current generation and active
Resource Plane claim and returns verified Repository metadata through a
bounded HTTPS client. The materializer checks out the exact head SHA and
stages a credential-free S3 archive; the Lambda MicroVM runtime adapter
installs it through the Guest one-use WSS route and starts the declared app.
Local tests cover staging, Guest installation/startup, and the fake-provider
provision/cleanup path. The matching ARM64 image, deployed Gateway, and real
MicroVM execution remain AWS validation gates; see Task 54 and the Preview
lifecycle document.

### ローカル実装と外部承認の境界

- ローカルで進める：broker adapter、API/CLI接続、fake/Postgres E2E、移行dry-run、runbook。
- 外部変更前に承認が必要：実Secret取得、Role/Secret provisioning、AWS apply、GitHub設定変更、Tenant activation/cutover。
- 未検証：実MicroVM/container/network/EFS隔離、Gateway replay/revocation、実AWS recovery。libraryテスト合格を配備完了として扱わない。

## 優先順位と依存関係

### P0 — Software Factoryの安全な最小実行経路

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 1 | [36 — Worker実行基盤ADR](task-36-workload-worker-adr.md) | Superseded：旧EC2決定は撤回。EC2は対象外 | なし |
| 2 | [37 — GitHub App資格情報](task-37-github-app-credentials.md) | 完了（library）：AWSCURRENT、JWT、Repository限定read-only token。配備は40 | なし |
| 3 | [38 — 固定commit checkout](task-38-workload-source-checkout.md) | 完了：Tenant/repository認可後に固定SHAを安全にcheckout、coverage 83.6% | 37（完了） |
| 4 | [39 — Workload実行handler](task-39-workload-execution-handler.md) | CP/RP library実装済み。実agent・worker/recovery配備と接続は未完了 | 38 |
| 5 | [40 — Worker/IAM/Artifact配備](task-40-workload-worker-deployment.md) | Lambda MicroVM worker、Queue、S3/IAM/監視を配備 | 61, 63 |
| 6 | [41 — GitHub Checks](task-41-github-checks.md) | ローカル配信E2Eとdefault-off queue/DLQ、Dispatcher/consumer IAM、ECS/Fargate consumer、mTLS Secret reference、DSQL grants、任意SNS alarm通知を実装・検証済み。AWS deployment/network/Secret provisioning reviewが残る | 37, 39 |
| 7 | [51 — Webhookイベント運用](task-51-webhook-events.md) | ローカル基盤実装・PostgreSQL検証済み：Repository Event正規化、7日raw payload retention、Owner/Admin監査付きreplay API・redacted failed-delivery API/CLI、concurrent processing fence、stable Delivery ID→Run/Check/Outbox作成、Repository別default-off triggerとpre-opt-in delivery抑止。operator dead-letterと包括retention/replay E2Eが残る | 37, 39 |
| 8 | [42 — Factory end-to-end](task-42-software-factory-e2e.md) | ローカル受け入れ完了。Installからisolated test、Check、log、cleanup、およびLambda Worker composed success/cancel/recoveryをPostgreSQL・Podman/fakeで検証済み。専用AWS環境でのsmoke/deployed-runtime検証は承認待ち | 38–41, 51 |

### P0追加 — Tenant別GitHub App（今回の要望で優先）

設計は[ADR 0007](../docs/adr/0007-tenant-github-apps.md)と[Tenant App仕様](../docs/tenant-github-apps.md)。Task 56と子タスク70/71までのlocal実装・受入検証は完了。残りはbroker/certificate/IAM/worker wiringと、承認後のproduction migration/cutoverであり、Tenant-owned activationはdefault-offを維持する。

| Task | 内容 | Depends on |
|---|---|---|
| [56 — Tenant App Registry/API](task-56-tenant-github-app-registry.md) | 完了：metadata、Owner/Admin API/CLI、承認binding、inert draft | 37（完了） |
| [57 — Tenant資格情報resolver](task-57-tenant-github-credential-resolver.md) | local draft proof API→Control Plane mTLS Broker→DB再解決→GitHub App identity verificationも実装・PostgreSQL検証済み。APIはRole/Secret/Versionを送らず、Brokerがexact pinを解決。production certificate/IAM/network deploymentが残る | 56 |
| [58 — Tenant OAuth/Webhook](task-58-tenant-github-setup-webhooks.md) | local webhook resolver/OAuth callback/API/CLI接続は実装済み。production certificate lifecycle/Role provisioningが残る | 56, 57 |
| [59 — 移行/IAM](task-59-tenant-github-migration-iam.md) | binding/immutable Run pin、Task 70 migration tooling、Tenant-pinned Worker checkout、broker role限定のexact private-key reader AssumeRole policyとTerraform testsはlocal実装済み。cross-account trust/Secret/KMS provision、production backfill/cutover/rollbackは承認後 | 56–58 |
| [60 — Tenant App E2E/運用](task-60-tenant-github-app-e2e.md) | Task 71の二Tenant local E2Eは検証済み。production rollout確認と運用runbookが残る | 56–59 |

### P0追加 — Workload実行基盤の再選定（EC2対象外）

実行基盤はユーザー指定のLambda MicroVM。旧EC2決定と先のFargate選定は撤回済み。公式専用APIとSDKを確認し、Tasks 63–65のcontroller/cancel/result recovery libraryを実装した。AWSは8時間上限で標準egressはInternet。adapterは8時間超を拒否し、承認済みVPC connectorを必須にする。image/container/network PoCは未実施。Task 40の配備はその確認に依存し、Run作成はdefault-off。設計は[ADR 0008](../docs/adr/0008-non-ec2-workload-runtime.md)。

| Task | 内容 | Depends on |
|---|---|---|
| [61 — Lambda MicroVM実行契約の確認](task-61-workload-runtime-selection.md) | API確認済み。8h/ARM64/network制約を記録。実image/container/network PoC待ち | 39（substrate-neutral foundation）、43と連携 |
| [62 — RP commandのdurable受理](task-62-workload-command-acceptance.md) | 完了（library）：strict decode、RP固定、条件付きjournal作成、duplicate/identity検証 | 39 journal（実装済み） |
| [63 — Lambda MicroVM controller/recovery](task-63-lambda-microvm-controller.md) | 実装（library）：公式SDK、仕様/ID binding、Ensure/Poll、結果のdurable再送。agent/worker配備待ち | 61, 62 |
| [64 — cancel/物理cleanup](task-64-workload-cancellation-cleanup.md) | 実装（core）：CP cancel Outbox、RP tombstone、停止確認、retry、replacement gate | 63 |
| [65 — 障害復旧/local E2E](task-65-workload-recovery-tests.md) | 実装：fake/SDK stub、race、PostgreSQL RP→CP結果とcancel統合 | 63, 64 |

### 整理後の実行キュー

親タスクと分割済み子タスクを重複計上しない。ここでは次に着手する実装可能項目と、Claude Code Actionまでの依存順を示す。外部AWS/GitHubの変更・本番有効化は別途承認が必要。

| 順 | タスク | 状態と次の作業 | 完了条件 |
|---|---|---|---|
| 完了（ローカル） | [79 — Webhook lifecycle E2E](task-79-webhook-lifecycle-e2e.md) | signed ingressからRun/Check/Outbox terminal acknowledgmentまでPostgreSQL/fake E2E、focused Make target、runbookを追加 | `make test-webhook-lifecycle-integration`と関連integration suiteで検証 |
| — | [78 — Platform operator DLQ](task-78-webhook-platform-operator-dlq.md) | **ローカル実装・focused PostgreSQL検証済み**。production operator-group provisioning/deploymentは別ゲート | cross-Tenant redacted listing、`hako-admin` authorization、expiry/active-binding replay、atomic audit、rate limit、runbook |
| 3 | [41 — GitHub Checks](task-41-github-checks.md) | **ローカル実装済み・運用ゲート待ち**。専用Queue/DLQ、consumer task/service、roles、Secret ARN/mTLS注入、DSQL権限、任意SNS通知をdefault-off Terraform化。AWS apply/Secret作成は別承認 | network/Secret/image inputsのreview・approved deploymentと運用確認（apply前は未完了） |
| 4 | [80 — Runtime契約](task-80-workspace-runtime-contract.md) → [81 — durable journal/fence](task-81-workspace-runtime-journal.md) → [82 — Lambda MicroVM adapter](task-82-workspace-lambda-microvm-adapter.md) → [83 — recovery/fake API E2E](task-83-workspace-runtime-recovery-e2e.md) | **Tasks 80–83 local実装・検証済み**：SDK HTTP fake、adapter再起動/timeout、foreign ID拒否、cleanup retry、concurrent duplicate、Controller→result-consumer→PostgreSQL binding投影。実AWS PoC・本番接続は別ゲート。EC2は対象外 | Workspace runtime suiteと`TestWorkspaceControllerResultConsumerProjectsBindingEndToEnd`。AWS bucket/IAM/image/network/lifecycle PoCは承認後 |
| 4a | [45 — hako-agent](task-45-hako-agent.md), [61 — MicroVM PoC](task-61-workload-runtime-selection.md) | **未完了**。journal-backed per-stream runtime lookup、shell/VS Code/SSH/許可port-forward/file-transferのGuest WSS経路、署名permission/port-bound one-use ticket、PTY、bounded argv `hako exec`、30秒lease/10秒周期のversioned private-socket heartbeatを実装・local検証。PTYは同一MicroVM内で切断中も継続し、同じuserのみ新Ticketで再接続可能。CLIはSession/Ticketを更新してbounded retryし、Guestは最大1 MiBの切断中出力を保持。`hako cp`はEFS `/workspace`へno-follow/atomic転送。OpenSSHはloopback-only/public-key-only起動とWorkspace別EFS host keyまでlocal実装。Lambda image builderの実ビルド、production key distribution、network/MicroVM PoCは残る | local artifact/hook/PTY/ticket/file-transfer/exec/PostgreSQL API testsと、承認後の実MicroVM PoCを区別 |
| 5 | [40 — Worker deployment](task-40-workload-worker-deployment.md) | **未完了**。credential/protocol/process、Agent listener binary、Tenant-pinned mTLS broker checkout（shared fallback拒否）、Agent→Controller ticket付きredacted log relay、Controller側attempt限定S3 writer、handler composition、専用command/result queue・DLQ・alarms・queue-only IAM・Dispatcher fail-closed routingとfake E2Eは実装済み。Terraformにdefault-off combined Worker Lambda、command mapping、recovery/artifact-cleanup schedule、alarmsを追加。Artifact HMAC API/Resource Plane cleanup core、exact-prefix `s3:DeleteObject`、Control Plane per-Plane cleanup-key resolver/IAMを追加。private subnet/専用SG・限定ENI IAMを実装し、各subnetのroute tableにdefault NAT Gateway routeがあることをTerraform planで検査。deployment/shutdown/rollback、DLQ triage、artifact retentionを[runbook](../docs/workload-controller-deployment.md)に記載。NAT健全性、VPC endpoints/SG egress/DNS/broker/image/network、AWS deploymentは未検証。Artifact bucketはTask 40の独立opt-inでversioning/lifecycle/public block/IAMをmock plan検証済み。既存bucket adoption・KMS policy・配備は未確認 | local integration、Terraform validate/mock plan、runbook。実AWS適用は承認後 |
| 6 | [42 — Software Factory E2E](task-42-software-factory-e2e.md) | **ローカル受け入れ完了**。signed webhook→ChecksのPostgreSQL/Podman E2E、Scheduler Consumer→placement/Outbox→RP Dispatcher→Lambda Worker→署名付きAgent success、result apply/replay、handler restart replayを検証。cancel Outbox→Dispatcher→Worker terminateで署名付きguest Agent process contextがcancelされ、PostgreSQLへ結果反映・artifactなしを検証。S3Store+conditional fake S3によるscheduled recoveryとACK消失後のPostgreSQL再適用も検証。専用AWS環境でのsmoke/deployed-runtime検証は承認待ち | `make test-software-factory-e2e`と`go test -tags=integration ./internal/workloadcontroller -run Composed` |
| 7 | [52 — Ephemeral Actions runner](task-52-ephemeral-actions-runner.md) | **進行中**。Repository別default-off admission、Lambda MicroVM launcher、Plane mTLS launch/bind/token/failure、署名bootstrap、Tenant model credential broker、guest `/run` identity-bound receiverを実装・unit/race test。`cmd/hako-actions-runner-guest`はofficial runnerを`--ephemeral`設定し、model keyはrunner process envのみ。`_diag`ログを削除前に64 KiB上限で回収・redactし、claim-scoped capabilityでHTTPS転送、Control Planeが再redactして3 KiB excerpt/digestをexact job/attempt Checkへ保存しOutbox配信する経路をPostgreSQL/fakeで検証。署名webhook→PostgreSQL admission→startup/bootstrap→fake runner→durable Check→cleanup recovery E2E済み。公式Runner 2.338.1 ARM64 imageをmanifest digest固定でbuild/inspect済み。egress connector、AWS apply/live verificationとLambda MicroVM上の実行確認は残る。[queue and Check contract](../docs/actions-runner-startup-queue.md) | MicroVM/API egress and image execution, live deployment, fork/foreign job/cancel/misassignment/cleanup retry fence |
| 8 | [74 — Provenance](task-74-claude-action-provenance.md) → [75 — Tenant credential policy](task-75-claude-tenant-credential-policy.md) | **Task 74/75 local acceptance implemented。** Tenant API/CLI、exact-Version reader、provenance/Plane/MicroVM fence、mTLS broker→Worker→signed bootstrap、guest process env handoff/cleanup、pinned ARM64 image build、fake-provider PostgreSQL E2Eを検証済み。実image execution、IAM/certificate配備は残る | 不正workflow/fork/cross-Tenant/foreign VM拒否。Secretはfake/localテストだけ |
| 9 | [76 — Action execution](task-76-claude-action-execution.md) → [77 — E2E/operations](task-77-claude-action-e2e.md) | **Task 77 local E2E/runbook完了。Task 76の主要local compositionを実装・検証、production gateは未完了**。signed webhook→startup worker→model-credential HTTP client/Broker/PostgreSQL resolver→signed guest bootstrap→runner process→Checks Outbox/Dispatcher/fake GitHub→Broker HTTP cleanupを接続。success/failure/cancel/timeout、provider failure/restart/backoffを検証。synthetic CA証明書によるTLS 1.3 mTLS実ハンドシェイクで許可Plane/非許可Plane/証明書なしを検証。official Runner 2.338.1 ARM64 digest-pinned imageはbuild/inspect済みだがMicroVM実行は未検証。production certificate lifecycle・MicroVM launch・runner token/GitHub API、live IAM/network verificationとrolloutは未完了 | fake providers + PostgreSQL E2E、local CI、secret boundary documentation |

Task 41とTask 51のローカル受入は完了しproduction deployment gate待ち。Task 78/79のWebhookローカル作業は完了。Tenant App Tasks 66–72はローカルのAPI/CLI/broker/mTLS/E2E基盤が実装済みで、本番証明書/IAM/worker配備は別の運用ゲートとして追跡する。Task 68のlocal controls/EMF/alarmsも実装・mock plan検証済みだが、通知先設定/AWS配備レビューは残る。

Claude Code Actionのproduction対応は**まだ未完了**。Task 74のprovenance gate、Task 75のTenant別resolver、Task 76の署名付きguest bootstrapからrunner process・terminal Check/cleanupまでのlocal PostgreSQL E2Eとdigest-pinned ARM64 runner image buildは実装済み。Broker model-credential endpointも合成CA証明書によるTLS 1.3 mTLSハンドシェイク、許可Plane・非許可Plane・証明書なしをローカル検証済み。Task 75のTerraformにはBrokerのdefault-off exact ARN allowlistと、target Tenant reader Roleのtrust/Secret/KMS policy moduleを追加済み。対象AWS accountへのRole provisioningとBroker allowlist apply、production mTLS certificates/deployment、実image/MicroVM実行、AWS/GitHub実環境は残る。Task 52のephemeral runnerとTask 40のLambda MicroVM workerが前提で、既存GitHub App credentialやAWS credentialをClaudeへ流用しない。[Workflow contract](../docs/claude-code-action.md)と[credential boundary](../docs/claude-model-credentials.md)を参照。

実装済みlibrary、未接続runtime、local E2E、production deploymentを別々に追跡する。API/CLI/controllerとlocal testsを優先し、Terraformはdeployment contractの検証に限定して先行applyしない。

### P1 — Workspaceの実AWS Runtimeと永続データ

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 9 | [43 — Workspace Runtime](task-43-workspace-runtime.md) / Tasks [80–83](task-80-workspace-runtime-contract.md) | ADR/contract → durable fencing → MicroVM lifecycle → recovery/E2E | Task 61 runtime evidence |
| 10 | [44 — EFS Workspace保存](task-44-efs-workspace.md) | **進行中**：POSIX-restricted AP manager、immutable journal binding、Lambda MicroVM provision/reuse/cleanup、strict `/run` hook、非特権Agent childを実装。guest mount helperは50秒上限・mountinfo health wait。Workspace別IAM roleのidempotent create/verify/delete、Lambda-only trust、exact AP/TLS policy、runtime launch role override/journal bindingをlocal/fake IAMで実装。Terraformはdefault-off。明示prefixに限定したS3 journal listing/Getとpaginated EFS Describeのread-only reconciliation CLIを追加（100k上限、tombstone保持、削除なし）。実EFS IAM、connector/private subnet、ARM64 image/MicroVM、restore検証は残る | 43 |
| 11 | [45 — hako-agent](task-45-hako-agent.md) | 固定`/bin/sh` PTY、VS Code/SSH fixed-loopback bridge（8081/22）、Workspace EFS confined file-transfer route（one-use ticket、openat/O_NOFOLLOW、atomic upload/download）、Gateway許可port-forward、bounded argv `hako exec`、30秒lease/10秒周期のversioned private-socket heartbeat、Guest WSS/one-use MicroVM-bound ticket、EFS後の非特権Agent child、mode-0600 Unix socket/readiness/route proxy、pinned/checksummed code-server ARM64プロセス、public-key-only loopback OpenSSHとWorkspace別host key、journal→GetMicrovm per-stream resolverを実装・local test。production Gateway/live MicroVM、actual image/`/dev/pts`、production key provisioningは未接続 | 43 |
| 12 | [46 — Session/Gateway](task-46-session-gateway.md) | 進行中：5分ticket、RBAC API issue/revoke、HTTPS/HMAC active-session API/client、TLS-only WSS core/PTY/port-forward E2E、journal-backed Lambda MicroVM shell resolver、15秒周期のactive-session再検証、disconnect→idle sweep→Dispatcher→fake Resource Controller→結果投影のPostgreSQL E2E、default-off Terraform deployment、Workspace allocation単位のOCC共有接続上限、同一Session TicketのGateway複数台間同時再利用拒否、Workspace単位30新規Lease/分の共有rate limit、構造化ログと低カーディナリティCloudWatch EMF、Session公開鍵のhot reload/old-new overlapを実装。production鍵配布/rotation、live AWS/MicroVM E2Eは残る | 43, 45 |
| 13 | [47 — 開発者CLI/Service Catalog](task-47-developer-ux.md) | **進行中**：`hako shell`は同一MicroVMのPTY detach/reattachと新Session再発行に対応。Grace Period後の自動SuspendはTask 85。`hako open`のloopback browser tunnel、VS Code/SSH fixed Guest bridge、専用Session/Agent ticket、Workspace imageでpinned/checksummed code-serverをEFS UID/GIDで起動する経路を追加。`hako cp`はSSH/SFTPと独立したone-use file-transfer ticket、WSS Agent route、symlink-safe EFS操作で実装。ARM64 image build/MicroVM検証は未実施。`hako wait`/`forward`/`ssh-proxy`/`cp`、versioned PostgreSQL/Redis catalogと認可API/CLIも実装。Service desired stateはrevision付きOutboxとPlacement固定queue routingまで接続。production router検証が残る | 44–46 |
| 13a | [84 — Workspace Service runtime](task-84-workspace-service-runtime.md) | **ローカルAPI/Guest/Controller実装済み**：`make test-workspace-service-runtime-integration`でGuest/Controller unitとPostgreSQL API lifecycleを再実行可能。Service revision/Outbox、Placement routing、Guest capability、固定Catalog/rootless `nerdctl`、HMAC報告/最新desired API、SQS Lambda、default-off Terraformを実装。Service no-follow cleanup、世代fence、stale report retry、PostgreSQL E2E済み。Service network作成時は`--internal`、既存networkも`Internal=true`を確認し、Workspace/Workload image buildはCNI 1.1.0+と`firewall` pluginを要求。Podman 5.8.7 positive-control smokeでは通常bridgeから外向きHTTP成功、`--internal` bridgeは`Network unreachable`（nerdctl/CNI/ARM64/MicroVM検証とは区別）。stable generated secret・一時env-file・scoped IAM、7日復旧期間付きSecret retirement、credential rotationをfake/local検証。nerdctl同梱CNIでの実コンテナegress、ARM64 image、実MicroVMのnamespace/network/cgroup/resource limits、MicroVM egress policy、AWS適用は未検証。`test-job-services-podman`は別のWorkload adapterなのでTask 84の実行証拠に数えない | 40, 43–47, 80–83 |
| 13b | [85 — Connection leaseとidle suspend](task-85-workspace-idle-suspend.md) | **ローカル受入完了**。Durable lease/API、allocation-bound Gateway heartbeat、disabled-default policy API/CLI、fenced Suspend Operation/Outbox/system audit、EventBridge Lambdaとdefault-off Terraformを実装。Gateway WSS→signed lease API→PostgreSQL disconnect/reconnect/sweep E2Eと`hako resume --wait`を検証済み。実Guest reconnectとAWS配備は残る | 43–47, 84 |
| 13c | [86 — Workspace Service port authorization](task-86-workspace-service-port-authorization.md) | **ローカル受入完了**。GatewayはControl Plane署名APIでPersisted Session、稼働Workspace、healthyな現行世代ServiceのCatalog TCP portを照合。PostgreSQL統合テストで宣言外port、世代失効、Service異常、Workspace suspend、claims不一致を拒否。HTTP clientの署名/異常応答テストを追加し`internal/workspacesession` 86.9% coverage。Gateway deployment rolloutは未実施 | 47, 84 |

### P2 — Placementの自動運用、AWS検証、運用準備

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 14 | [48 — Health reporterとplacement運用](task-48-health-reporter.md) | **ローカル実装・unit/PostgreSQL検証完了**。paired Secret provisioning/rotation、AWS deployment、live probe/alarms verificationは未実施・承認待ち | なし |
| 15 | [49 — Terraform/AWS E2E](task-49-aws-environment-e2e.md) | Account分離環境、実AWS apply手順、作成〜接続〜cleanup検証 | 43–46, 48 |
| 16 | [50 — Security/Operations](task-50-security-operations.md) | 進行中：既知Go脆弱性11件修正、OAuth redirect拒否、CI govulncheckとlocal review。監査/復旧/実AWS検証が残る | 40, 42, 49 |

### P3 — Software Factoryの拡張と利用制御

| 順序 | Task | 内容 | Depends on |
|---|---|---|---|
| 17 | [52 — GitHub Actions Runner Mode](task-52-ephemeral-actions-runner.md) | **ローカル実行経路を実装・E2E検証済み**：admission→Lambda MicroVM launch/bootstrap→ephemeral Guest runner→Checks→cleanup/retry、専用SQS/Lambda Terraformをdefault-offで追加。残りはAWS配備・listener/network検証、複数Plane placement、実GitHub/AWS検証 | 40, 41, 51 |
| 18 | [53 — Native Pipeline/Agent](task-53-native-pipeline-agent.md) | **進行中**。Broker mTLS経由のWorker task取得、attempt/Run pin検証、launch spec〜署名Ticket〜guest `/run`までのIssue context digest bindingを実装。Issue本文はbounded・memory-onlyでjournal/result/logへ非記録。Factory Service宣言予算とJob/attempt別Service lifecycle managerを追加。Linux `nerdctl` adapterはdigest固定Catalog・internal network・loopback publish・read-only/tmpfs/resource limits・生成credential env-fileを強制し、Podman上でPostgreSQL起動/readiness、生成credential認証、`SELECT 1`、到達可能時の外向きTCP拒否を検証（x86 imageでありnerdctl/ARM64ではない）。固定UIDのPipeline commandへ残余CPU/Memory budgetのfail-closed cgroup v2 scopeを適用し、サービス予約分との合算を制限。ResolverからService managerへの接続はimage configによる明示opt-inで実装、既定は引き続き拒否。Job UID/rootless containerd UID範囲のIPv4/IPv6 loopback-only host egress guardをAgent起動時に適用。terminal resultはRP journalへの永続化後、Lambda MicroVM cleanup確認までCPへ未publish。Tenant `native_agent` credential API/CLI、Repository別default-off gate、provenance検証resolver、RP mTLS Broker clientからGuestのattempt-ticket-bound no-store Start要求へのcredential handoff、credential-capable Factory限定、buffer clear/output redactionを追加。Task 87で一般Workload Artifactの認可付きbytes download API/Gateway/CLIを実装済み。Native Agent成功Runのattempt-scoped ZIP生成→Agent専用changes ticket→Resource Plane S3保存→`agent_changes` result metadata、Anthropic Messages fixed-tool loopと、`HAKO_NATIVE_AGENT_ENABLED=true`かつ固定モデル指定時だけ選択する`cmd/hako-agent` compositionを追加。Migration 000176/PostgreSQL artifact persistenceとfactory default-off/config testsを通過。proposal ZIPの非展開安全検査`hako workload changes inspect`と、Task 89向けのbase検証→proposal overlay→deterministic source archive primitiveを追加。Migration 000181でAgent proposal binding、000183でtrusted result経由のcleanup確認、000184でdigest-bound verifier Checks、000185でsubmission idempotency stateを追加。Factory-enabled時に同一Resource PlaneへVerifier Job/attempt/dispatchを同一transactionで生成し、Resource Plane metadataから限定S3 keyを検証してproposalをbase checkoutへoverlay。Control Plane resolverはcurrent write policyとproposal digestに結び付いたrequired Checkをfail-closedで照合し、publication recordはdeterministic branchとimmutable pinsを保持、branch/PR state遷移を冪等化。PostgreSQL integrationでVerifier dispatch、digest付きCheck保存、base SHAへのGitHub Check非送信、publication retry/conflictを検証。認証済みAPIと`hako workload publication show`で最新attemptの安全な状態情報を表示。Task 89のBroker ARM64イメージと、private ECS/API VPC/mTLS Secretのdefault-off Terraform contract、mock planを追加。未完了: AWS deployment/network/Secret provisioning、MicroVM execution、実効cgroup/egress検証 ([Task 89](task-89-agent-proposal-publishing.md)) | 39, 40, 50, 51 |
| 19 | [54 — Preview/Catalog/Deploy](task-54-preview-catalog-deploy.md) | Catalog/OIDC検証、Tenant別Preview opt-in/PR lifecycle/fixed expiry/generation fencing、default-off expiry/provision producer、Tenant-scoped Preview list/session API/CLI、bounded generation cleanup retry/worker、Tenant policy/capacity-aware placement、専用Outbox routeとPreview SQS/result consumer、Lambda MicroVM Runtime journal adapter、Tenant App mTLS checkout→immutable SHA archive→S3→Guest WSS one-use install、strict `.hako/preview.yaml` app startup/loopback health check、fixed port 3000 authorizationまでlocal実装。producer/expiry schedule、column-scoped DSQL role、queues/result consumer/source IAMをdefault-off Terraform化し、PostgreSQL E2Eでprovision→app startup→cleanup/result replayをfake provider経由で検証。未完了: matching Guest keyを含むARM64 imageの実行、AWS deployment/DSQL grants、real Lambda MicroVM/Gateway/network/cleanup検証、STS trust/exchangeとdeployment execution。Public Preview URLは発行しない | 43–46, 50, 51 |
| 20 | [55 — Quota/Usage UX](task-55-quota-usage.md) | **ローカル受入完了**：Tenant Owner/Admin向けQuota API/CLIにWorkspace/Workload上限、Preview concurrency (default 2/max 5)・lifetime (5分–24時間)、Artifact bytes (default 10 GiB, max 100 GiB)、Workspace Service per-Workspace memory/CPU capを設定。Workload/PR Preview admissionとService admission/releaseはTenant OCC guardsを使用。cleanup-pending PRはquotaを保持し、`hako usage`はWorkload/PR Preview合算値を返す。最大100 MiB/Run Artifact予約、retained metadata usage、`hako service usage`を提供。`hako tenant quota set`は現在の予約使用量を表示し、明示確認またはautomation向け`--yes`後に更新。期限切れArtifact cleanup core/API/client、Control Plane per-Plane secret resolver/map、default-off Resource Plane Lambda schedule、exact-prefix DeleteObject IAMを追加。PostgreSQL統合テストでQuota境界、PR/Workload Preview共有上限、cancel/failure/timeout、重複終端、予約解放・stale予約修復をカバー。実AWS適用・実Secret/ARN mapping・bucket versioning確認と実測resource usageは未完了 | 40, 47, 50 |
| 後続 | [73–77 — Claude Code Action](task-73-claude-code-action.md) | Task 52のephemeral runner上でworkflow provenance、Tenant model credential policy、実行/cleanup、local E2Eを実装 | 40, 41, 50–52 |

## 直近の実装状態（2026-10-18確認）

- Workload RunのDB/domain、Tenant visibility、create/list/detail/cancel APIとCLI、transactional Outbox、専用scheduler SQSへのDispatcher、厳格decode・visibility延長・成功後ackするconsumer coreは実装済み。
- Task 42のローカルPostgreSQL + Podman E2Eで、Tenant-specific production HTTP handlerとDB Installation authorizer（Secretはsynthetic resolver）を通る署名付きWebhookからHMAC拒否、foreign Installation拒否、Delivery ID重複/conflict、Tenant外Repository IDのRun生成拒否、成功/失敗immutable untrusted Run、pre-dispatch cancelでplacement/attemptを作らない経路、executor停止後のin-flight cancellation、timeoutとartifact upload failure、redacted artifact、success/failure/cancelled/timed_out Checks、GitHub 503後のCheck再試行をBatchSize=1で接続・検証した。成功/失敗Podman containerはRunラベルで`--rm`後に不在確認し、新しいExecutorによるterminal Run再配信もPostgreSQL stateで拒否する。Factory E2E内でforeign TenantからのRun取得拒否と同一Tenant内の無関係requesterへのlist非表示も確認し、別のAPI integrationは認証済みroute境界を検証する。加えてPostgreSQL Scheduler Consumer→placement/Outbox→RP Dispatcherの専用Queue route→Lambda Worker handler→MicroVM fake→署名付きAgent→PostgreSQL `ResultHandler` の結果適用/重複再送/ログartifact/cleanup E2Eを検証した。S3Store上のrecoverable journalをWorker handler再生成後のscheduled recoveryで読み、ACK消失後の結果を再適用する経路もconditional fake S3とPostgreSQLで検証済み。cancel request→cancel Outbox→Dispatcher→Lambda Worker→MicroVM terminate時に署名付きAgentの実プロセスが`context.Canceled`を受ける経路もfake provider/guestで確認し、cancelled resultとartifactなしをPostgreSQLで検証した。実AWS検証は未完了。CIはPodmanとGo imageを必須化。
- Go testを隔離Podman containerで実行するFake Runtime、commit照合、redacted logs、S3 writer、state/artifact ServiceとTask 38固定commit checkoutを実装済み。Task 39ではCP result handler/result SQS consumer、条件付きS3 CASを使うRP journal primitive、trusted `HakoEventType`属性とpartial batch failureを扱うWorkload Lambda SQS adapter、prefix-scoped S3 recovery listing、bounded multi-page recovery sweepを追加した。S3 conditional recovery lease、lease loss時に停止するsweep orchestration、設定上限必須のEventBridge Lambda adapterもローカル実装・検証した。Production resolver/agent/runtime composition、recovery Lambda composition・schedule、queue mappingとexact-object IAMは未接続。
- `enable_workload_run_creation`はdefault-off。worker/handlerの運用可能性を確認せず有効化しない。
- Task 37の資格情報libraryは実装・検証済み。App private keyはAWSCURRENTから取得し、Tenant/Repository認可とlive Installation検証後にread-only tokenを発行する。専用service/IAM配備とcheckout接続は未実装。
- Workspace用のLambda MicroVM adapter/library、durable fencing journal、provider IDのControl Plane binding projection、fake/API/DB E2E、Resource Controller Lambda内の明示`lambda-microvm` composition、Dispatcherのschema v2 operation routing、EFS AP provision/reuse/cleanup、strict `/run` hook付きWorkspace init/image定義はローカル実装済み。Resource Plane Terraformにdefault-off ARM64 Controller Lambda、固定image/journal-prefix IAM、dynamic EFS role prerequisite、Fake controllerとのqueue exclusivityを追加し、OpenTofu mock planでopt-in/default-offを検証した。dev rootは未設定でAWS applyも未実施。EFS helperを含むPodman image buildと実MicroVM hook、動的Workspace-scoped IAM role/AP lifecycle、EFS restore/live isolation、AWS IAM enforcementと実配備は未検証/未完了。Workload Terraformは引き続きFake Runtime固定。
- Placement policy/capacity/healthに加え、Task 48のsigned HMAC ingress/client、replay protection、SQS queue・固定Lambda MicroVM image version・固定VPC connector・Controller function・EFS metadata probeを行うdefault-off scheduled Lambda、EMF metrics、exact-resource IAM/API Gateway throttle/alarmsを追加し、unit/PostgreSQL統合テストを検証済み。metadata probesはqueue処理、MicroVM実起動、network isolation、Workspace EFS mountを証明しない。ローカル実装は完了。paired Secret provisioning/rotation、実配備/alarms確認は外部運用ゲートとして未完了。
- Tenant別App/三種類のSecret/承認reader Roleの設計を追加（Tasks 56–60）。Task 56でmetadata schemaとAPI-roleのDB grant、Task 57でactive/draft resolver、STS/Secrets Managerのexact Role/Secret/Version provider、integration/revision/generation単位のcache identityとper-use active generation authorization、外部proofとDB OCC再検証を分けたverify APIを追加した。Run integration接続、現行GitHub App設定やAWS IAMのTenant分離は未変更。
- Task 47の`hako wait`はローカルHTTP APIまで通すCLIテストを追加。Tenant/Workspace route、Bearer token、observed-state表示、token/API URL欠落時のfail-closedを検証し、`workspace_waitCLI` coverageは85.7%、wait-state loopは89.7%。Workspace CLI package全体のstatement coverageは49.1%。
- Task 85のTenant idle-policy CLIもローカルHTTP APIでGET/PUTを検証。Tenant route、Bearer token、enabled/grace payload、revision、CLI Tenant IDによる上書き、token/API URL欠落時のfail-closedを確認。PostgreSQL API認可・監査テストと合わせてCLI接続層をカバーする。
- Task 54の`hako catalog list/set`とdeploy-policy `show/set/verify`をlocal HTTPで通し、Tenant route/Bearer認証、`lambda-microvm` metadata、Repository identity、EC2/unknown secret field事前拒否、合成OIDC token handoff、execution disabled維持・token非echoを検証。これはCatalog/policy API/CLI確認で、Preview runtime allocationの完了を意味しない。
- Task 55のTenant Usage、Quota GET/PUT、Workspace Service reservation usageをlocal HTTPで検証。Tenant route/Bearer認証、used/reserved値、quota revision、未指定limit保持、token/API URL欠落時のfail-closedを確認。これはconfigured reservationであり実測usageではない。
- Task 55の`hako usage` rendererが負のWorkspace compute上限など不正値を出力前に拒否し、Writerエラーを伝播するようにした。Table-driven testsで`writeTenantUsage`のstatement coverageは100%、`cmd/hako`全体は72.0%。CLI表示の防御的検証であり、Quota enforcementはAPI/store transactionが担う。
- Task 55のWorkspace Service usage rendererにも不正値の事前拒否とWriter失敗伝播のテストを追加。`writeTenantUsage`と`writeWorkspaceServiceResourceUsage`はそれぞれ100% statement coverage。`go test ./cmd/hako -run 'TestWrite(WorkspaceServiceResource|TenantUsage)' -count=1`とpackage全体のcoverage実行が成功。
- Task 45: `hako service rotate-credentials` はAPI handlerとCLI guideに存在していたが、Go API clientのaction allowlistが拒否していた不整合を修正。Tenant-scoped HTTP client/CLI testでroute、Bearer auth、action body、generation metadataを検証。Secret valueは返さず、実MicroVMでのcredential promotionは未検証。
- Task 47: CLI usageに`hako add`、`hako services`、Service usageと全Lifecycle actionを追加し、discoverability regression testを追加。
- Task 42: PostgreSQL-backed composed Lambda Worker E2Eにsecret-bearing poison SQS commandを追加。正常recordと混在したbatchで正常dispatchだけを処理し、poison IDのみpartial-batch failureとして返して、本文をログへ出さないことを検証。Worker再生成後の正常duplicate非再実行も確認。実SQSのDLQ redrive遷移はAWS-managedのため未検証。
- Task 42 verification target: `make test-software-factory-e2e` now runs the Podman webhook→Check test, authenticated PostgreSQL Workload API idempotency/visibility test, and composed Lambda Worker success/cancellation tests, including poison-message partial-batch coverage.
- Task 42 AWS smoke documentation: added a dedicated-account/Region guard, named approval gates, Lambda MicroVM-only smoke scope, tag-verified exact-resource cleanup, and cleanup report template. AWS deployment and live smoke remain unperformed and approval-gated.
- Task 75のTenant model credential CLI list/add/rotate/disableをlocal HTTPで検証。Tenant route/Bearer認証、purpose・generation・immutable Secret VersionId、`no-store`、plaintext credential非送信を確認。本番Role/Secret/certificate配備は未実施。
- Task 75のモデル資格情報CLIに不正action・未ログイン・API URL欠落・Tenant拒否のテストを追加。`parseModelCredentialCLI`と`manageModelCredentialCLI`は100% statement coverage、`cmd/hako`全体は72.4%。synthetic credential/local HTTPのみで、Secret取得なし。
- Task 56のmetadata/API/CLI基盤とPostgreSQL integration validationが完了。Owner/Admin制御、inert draft、idempotency、revision付きdisable、監査、default-off flag、operator-only binding registration手順を追加。Tenant App機能は未有効化。Tasks 57–60のlocal実装は子タスク66–72で分割・検証済みだが、production IAM/certificate/broker/worker配備と承認済みcutoverは未完了。
- Task 56のTenant GitHub App CLIに`show`/revision付き`verify`と、認証/API URL/API拒否/browser起動失敗の回帰テストを追加。`manageGitHubAppCLI`は100% statement coverage、`cmd/hako`全体は72.2%。ローカルHTTPとsynthetic credentialのみを使用。
- Task 57でTenant credential proof/resolver/cache safety libraryを追加。API binaryはprivate-key Secret reader Roleを直接assumeせず、Task 69でtrusted mTLS checkout brokerを追加した。Task 58でWebhook resolver/authorizer/fence、default-off route、OAuth setup pin/callback adapterを追加し、Task 67でAPI/CLI mTLS adapterを接続した。production broker composition/certificate lifecycleと実Role/Secret provisioningは残り、Tenant App activationはdefault-off。
- Task 59のschemaでTenant App/Installation bindingとWorkload Runのintegration/revision/generationを永続化。新規Runはserver-side bindingでpinされ、active Tenant AppがあるのにInstallation未bindingならshared credentialへfallbackしない。Task 69 checkout brokerとTask 40 Resource Plane Worker compositionはpinをDBから再解決し、shared/owned providerを分離する。Control Plane Terraformはexact private-key reader Role allowlistをbroker roleだけに付与し、Webhook role setとの重複を拒否する。旧Runは明示的にshared/null identityのまま。cross-account trust/Secret/KMS provisioning、production deployment、既存データbackfill/drainは未完了。
- Task 38の固定commit checkout providerを実装・検証済み。Tenant/Installation/Repository認可後にのみ資格情報を要求し、askpass FD経由でtokenを渡す。専用packageのstatement coverageは83.6%。Task 39はCP側のdurable dispatch handoff/result処理、別HMAC keyで認証するtrusted execution-metadata APIとRP HTTP client、Resource Plane profileを使う`MetadataSpecificationResolver`、broker-aware checkoutをsource stagerへ渡す`RepositorySourceMaterializer`、決定的tar archiveとattempt-scoped暗号化S3 stager/extractor、operation ticket付きAgent転送・digest/commit/attempt検証まで実装した。Tenant-owned Run用のmaterializerは固定Tenant/Run pinをmTLS brokerへ渡し、shared credentialへのfallbackを拒否する。PostgreSQLでattempt/Placement/Tenant App pin/Run state再検証を確認した。Tasks 62–65はRP journal・公式SDK controller・cancel/cleanup・復旧テストまで実装。基盤はLambda MicroVM、EC2は対象外。Task 61のimage/network PoC、Task 45の実agent/image wiring、Task 40のworker/recovery配備まで実行機能はdefault-off。
- Task 62のRP command受理libraryを追加。条件付きjournal作成、重複時のidentity照合、cancel/cleanup checkpoint保持を実装。Tasks 63–65でcontroller/公式SDK adapter、CP cancel Outbox、物理cleanup/retry、durable result replayとlocal/PostgreSQL統合テストを実装。Recovery listing/replay primitivesも追加したが、実agent/queue/recovery scheduler配備とAWS PoCは未完了。
- Task 45/40向けに、二層Workload agent認証、protocol v1 client/handler、commit検証後に固定`go test ./...`のみを実行するprocess backend、port 8080 listener executable、public-key loader、ARM64 MicroVM ZIP packager、strict `/run` hookを追加。Agentはprovider VM IDとWorkload attempt/executionをpinし、別VM/attempt/execution ticketを拒否する。子process group cancellation、最小環境、bounded/redacted outputを実装した。Redacted logsはAgent内の有界メモリからoperation-scoped ticketでResource Plane Controllerが取得し、Controller側IAMでS3保存する。MicroVMへAWS/S3 credentialは付与しない。Workspace PTY managerは固定`/bin/sh`、Workspace cwd、最小環境、session/TTL/terminal/input bounds、resize、process-group cleanupを提供し、Gateway WSSから実PTYと許可済みloopback portへ流すlocal E2Eを追加した。HTTPS/HMAC active-session lookup API/clientもlocalでPostgreSQL検証済み。Production key/configuration、Gateway multi-runtime router、VS Code adapter、secret provider、Lambda image build/real run-hook PoC、worker deployment、egress isolationは未完了でWorkloadはdefault-off。
- README Phase 0–9には残項目が他にもある。Task 36–65と追加の実行単位66–79がその実行順と受入条件を具体化する。完了判定時はREADMEの該当チェックとこのboardを両方更新する。
- Task 45/47 file transfer: `hako cp`/`--from`はSSH/SFTPと独立したfile-transfer Sessionとone-use Agent Ticketを利用し、Gateway/Guest WSS `/files` route、pinned `/workspace`+`openat`/`O_NOFOLLOW`、symlink/non-regular拒否、atomic upload、private-temp downloadを実装。専用protocol/Agent/Gateway/CLIテスト、race test、`go test ./...`が通過した。`make test-software-factory-e2e`と`make test-workload-runtime-integration`もPostgreSQL/Podmanで再検証済み。実Lambda MicroVM image/build/routeは未検証。

## 2026-10-24 追加進捗

- 2026-11-21 local integration refresh: `make test-integration-local` passed with all Go packages against the already-running `hako-dev-postgres`. `make test-managed-runtime-integration` passed its fake-provider Actions/Claude and Workspace idle lifecycle suites. Firecracker v1.18.0 host/KVM check and minimal Guest-init boot smoke passed on this x86_64 host. No AWS or GitHub access was used. This does not yet compose the Hako Workspace Guest, persistent disk, controller, Gateway, or CLI lifecycle.
- Task 97 checkout-result slice: added a fixed-pin `pending/failed/ready` state store, signed Resource Plane result endpoint and HMAC client. PostgreSQL integration exercises failed→ready retry, same-commit idempotency, conflicting commit rejection, detail projection, and suspend/resume pin retention. `go test ./internal/workspaceruntime ./internal/api ./internal/store/workspaces`, focused API integration, `git diff --check`, and Terraform format check pass. The Guest checkout/token broker is still not connected.

- Task 68 webhook operations: Tenant admission now emits CloudWatch EMF with only fixed `Service`/`Outcome` dimensions for rate-limit denials and shared-limiter failures. Control Plane Terraform defines alarms for sustained denials and any limiter failure. `go test ./internal/githubwebhook ./cmd/hako-api`, focused OpenTofu mock plan (4/4), `tofu fmt -check`, and `git diff --check` pass. AWS notification actions and deployment remain unapplied.

- API/CLI local wiring: localhost HTTP integration coverage exercises Tenant App list/add/disable/select-default/setup/delivery-list; Repository Factory/Preview/ephemeral Runner/workflow policy; Native Agent write/model-access settings; Workload Run create/list/show/cancel; and the protected health CLI route. Tests assert Tenant-scoped URLs, auth, request bodies, immutable commit, idempotency, revision/generation fences, no raw Secret fields, and dormant execution/runtime responses. Malformed Agent policy is rejected before network access. `go test ./cmd/hako -cover` reports 56.4% overall; relevant App/Repository/Workload commands are exercised against fake APIs.
- Tenant authorization coverage: added fake-row unit cases for successful membership lookup and exact `(tenant_id,user_id)` query scope, non-disclosing missing-member/Tenant results, wrapped database errors, and role allow/deny behavior. `go test ./internal/authz -cover` now reports 95.0% statement coverage; the PostgreSQL integration test also passes.
- Model credential resolver coverage: unit tests now check invocation guards and printable-ASCII/size bounds for both Actions Runner and Native Agent Secret paths. `go test ./internal/modelcredentialbroker -cover` reports 51.5%; provenance and rotation stay covered by PostgreSQL integration tests, and Native Agent delivery remains disabled.
- Task 50: Control Plane TerraformにHTTP API stage 5xx/p95 latency、Aurora DSQL QueryTimeouts/sustained OccConflicts、Tenant webhook rate-limit/limiter-failure alarmsを追加し、alarm名をoutputsに公開。Tenant webhookは固定dimensionのEMF admission countersを出力する。Dispatcherはpartial indexで最古の未publish event ageを取得し、低カーディナリティEmbedded Metric Formatとして出力する。5分を2 period超えた場合のlag alarmも追加。Dispatcher/webhook processor/result consumer/Reconcilerの外側ログからraw provider/DB errorを除き、固定failure classとbounded countersを残す。Focused/full Go tests、local PostgreSQL Outbox integration test、OpenTofu fmt/validate/mock planを検証した。通知先設定とproduction log/alert reviewは残る。
- Task 53: Owner/Admin向けRepository単位Agent branch/PR write policy APIと `hako github repository agent-policy set` を追加。branch prefix、same-repository PR、必須passing Checksを検証し、Factory opt-out時にpolicyを消去する。Resource Plane Runtime profileをWorkload kind単位にし、Job profileのAgent流用を拒否する。PostgreSQLでTenant/RBAC、監査、`execution_enabled=false`のdefault-off応答を検証。Native Agent executor/model credential handoffとTask 89のVerifier→Broker→GitHub publication pathはlocal実装済み。Lambda MicroVM image、target network/resource enforcementが未検証のため、production Run schedulingとAgent write実行は未接続。
- Task 53 follow-up: Workload image now runs the trusted `hako-agent` as root and drops fixed Go-test/Native catalog child processes to UID/GID `65532:65532` with no supplementary groups. Linux tests verify identity validation and real child UID when privileged. This establishes an identity boundary for a future rootless containerd socket; it does not start/containerize the daemon, enable Service declarations, or enable Agent scheduling.
- Task 53 runtime follow-up: Added a dedicated Job rootless-containerd supervisor, strict account/subuid/socket/tmpfs validation, and an opt-in `Dockerfile.services` build target requiring a reviewed ARM64 nerdctl archive and SHA-256. `HAKO_JOB_SERVICES_ENABLED` defaults off. Fake lifecycle coverage and `make test-job-services-podman` exercise recovery, cleanup, generated-credential PostgreSQL authentication, `SELECT 1`, loopback-only access, effective Podman cgroup limits, and denial of an external TCP probe from the internal sidecar network; the pinned ARM64 nerdctl runtime and Lambda MicroVM remain unverified. The default resolver rejects Service manifests; opt-in admission still requires target-runtime cgroup/egress validation before production use. The Podman acceptance was rerun successfully on 2026-10-25.
- Task 53 runtime follow-up: The Job Service Engine verifies post-create inspect metadata for memory/NanoCPU/PID limits, read-only rootfs, non-privileged mode, no added capabilities or bind mounts, `no-new-privileges`, fixed user, exact loopback-only port bindings, and catalog tmpfs paths/options, failing closed on mismatches. The Podman acceptance now also reads `memory.max`, `cpu.max`, and `pids.max` inside PostgreSQL; `make test-job-services-podman` passed with the expected 1 GiB, 0.75 CPU, and 256 PID cgroup v2 values. This proves host Podman cgroup application only; Lambda MicroVM/nerdctl enforcement and Job-wide aggregate cgroup limits remain unverified.
- Task 53 runtime follow-up: `NewFilesystemAgent` now refuses `AllowJobServices=true` unless its process factory explicitly declares Job Service support. `GoTestProcessFactory` reports support only when an attempt-scoped provisioner is present. This prevents resolver admission from being configured independently of the execution path; the option remains off by default and does not validate target MicroVM cgroup/egress enforcement.
- Workspace/Job image build correction: verified nerdctl-full 2.4.1 ARM64 and found it has no `slirp4netns`; AL2023 repositories also do not provide that package. Both opt-in image targets now take upstream static ARM64 slirp4netns as a separate version/SHA-256-pinned input. ARM64 image execution is still unverified on this amd64 host.
- Task 54: Tenant Software Catalog API/CLIに加え、Owner/Admin向けOIDC deploy policyのCRUDと`hako catalog deploy-policy show|set|verify`を追加。固定GitHub issuerのRS256/JWKS署名確認、AWS audience、Catalog Repository/Owner ID/name、ref/environment/workflow/event/subのexact matchを検証し、legacy/immutable-ID subjectに対応。tokenは保存しない。DB migration 000139とPostgreSQL RBAC/API integration coverageを追加。PreviewはOwner/Admin opt-in、PR webhookのdesired-state記録、GitHub `updated_at` fencing、quota lifetimeに基づく固定expiry、bounded expiry-to-cleanup transitionまでlocal実装（migrations 000140–000144）。PostgreSQL E2Eでreopen時reset、synchronize時非延長、期限切れとduplicate sweep idempotencyを検証。専用DSQL roleとdefault-off ARM64 Lambda/EventBridgeによる定期metadata sweepも実装し、AWS applyとschedule有効化は未実施。Migrations 000145–000146とPreview storeにbounded cleanup claim lease、attempt fencing、retry/backoff、confirmed cleanup transitionを追加。Generation別cleanup workerと移行fenceは000161–000162で実装。Tenant member向けPreview Gateway session API/CLI、期限を超えないTicket、PR世代/close/expiryを確認するResource Plane active lookupを000163–000164で追加し、API/client/CLI unitとPostgreSQL E2Eを検証。さらにPreview provision/cleanupをplacement・capacity・Outboxから専用queueへdispatchし、Resource Controller Lambdaで既存のdurable Lambda MicroVM journal、Tenant App broker checkout、S3 source staging、Guest one-use install、Preview app起動へ接続するlocal compositionを追加した。`hako github repository previews open|forward|logs`と固定port 3000のGateway/Guest policyも実装済み。default-off queue/source IAMとschedule/consumer Terraformはあるが未配備。残りはmatching ARM64 Guest image、実MicroVM/GatewayのAWS検証、production queue/schedule deployment。OIDC verification APIはEnvironment reviewer protection (`prevent_self_review=true`)とexact live Actions run attempt、Repository/Owner/ref/SHA/workflow/event pin、pending deployment不在、承認履歴をfail-closedで確認する。APIからmTLS Credential Brokerへの配線、Tenant Appのactions:read発行、Catalog policy/credential pinのbroker側照合と読取grantも追加。STS credential exchangeとdeployment executionは常に無効。
- Task 54 follow-up (historical checkpoint): Migration 000147 adds monotonic Preview generations. PR head/desired-state changes and expiry advance the generation; cleanup retries/completion require the claimed generation as well as claim/attempt/lease fences. The list API exposes it. Local PostgreSQL lifecycle integration covers stale generation rejection. Later Task 54 work adds the runtime binding and deletion command locally; AWS execution remains unverified.
- Task 54 cleanup follow-up (historical checkpoint): `internal/previewcleanup.Worker` executes bounded leased cleanup with an injected Resource Plane command client; executor errors map to fixed retry classes, and a Preview is marked cleaned only after explicit provider confirmation. PostgreSQL integration drives a fake executor through failure/backoff, a new fenced claim/attempt, confirmed completion and duplicate sweep. Later Task 54 work wires generation cleanup through the strict Resource Plane command/result path and Lambda MicroVM journal; AWS deployment and live deletion remain unverified.
- Task 54 generation-binding groundwork: migration 000159 records each admitted PR head as a separate private Runtime allocation generation. Head changes, close, and expiry retire old generations atomically; reopen creates a new generation without replacing cleanup state. PostgreSQL E2E verifies these transitions, and the authenticated Preview list remains metadata-only even when private provider identifiers exist. The later Resource Plane implementation allocates the MicroVM and derives its logical Gateway route from the journal; no separate public route is published.
- Task 54 provision-worker follow-up (historical checkpoint): `internal/previewprovision.Worker` runs the bounded claim/provision/bind/retry loop through an injected Resource Plane provisioner, records only fixed retry categories, and compensates provider resources when a generation-fenced bind becomes stale. Later work adds the concrete Resource Plane command, Lambda MicroVM journal adapter and logical Gateway route. The default-off Terraform, DSQL grants and AWS runtime remain unapplied/unverified.
- Task 54 Preview runtime adapter: `previewcontroller.WorkspaceRuntimeAdapter` maps each generation-derived runtime Workspace ID to the existing durable Lambda MicroVM Workspace Runtime journal (stable ensure revision 1/delete revision 2), reads the MicroVM ID through `ResourceBindingReporter`, and retries ensure before exact-journal compensation. The Resource Controller Lambda composes this adapter behind exact SQS source-ARN routing. Resource Plane Terraform offers default-off dedicated command/result queues and DLQs, exact CP role policies, controller queue IAM, event-source mapping, alarms, and queue outputs; Control Plane manifest URLs, result-consumer mapping, and DSQL grants still require deployment configuration. Terraform plan tests and validation pass locally; no AWS apply was made. Tenant App SHA checkout, Guest one-use source installation/app bootstrap, Gateway ticket/port authorization, and cleanup are locally composed. The matching ARM64 Guest image, deployed schedules/queues, and real MicroVM/Gateway route remain open.
- Task 54 runtime identity follow-up: each provisioning claim carries a deterministic private runtime Workspace ID derived from Tenant/Installation/Repository/PR/generation. Retries converge on the same identity, PR-head generations receive distinct identities, and binding rejects a provider response for any other Workspace ID. The ID now keys the Resource Plane runtime journal without creating a Control Plane Workspace record; Gateway Sessions use the separate Preview session registry and authenticated lookup.
- Task 54 generation cleanup follow-up: migrations 000161–000162 add per-generation cleanup leases, retries, and a bounded due index. `internal/previewcleanup.RuntimeWorker` cleans bound PR-head runtimes individually and only releases the Preview quota after all generations are confirmed. Generations with no provision attempt are retired without provider calls; interrupted attempts without a binding remain pending for recovery through the pinned Resource Plane's durable journal and exact frozen launch request. Unjournaled MicroVMs must not be adopted or deleted through scans because the Lambda MicroVM API does not provide verified per-MicroVM tag discovery. The legacy PR-wide worker is excluded from Previews with generation rows. PostgreSQL E2E covers lease recovery, active-lease exclusion, stale claim rejection, provider cleanup, and final quota release. Later Task 54 work wires deletion through the Resource Plane Lambda MicroVM controller; the default-off deployment and AWS runtime remain unverified.
- Task 54 coverage follow-up: PostgreSQL integration exercises bounded Preview listing, Tenant/Installation/Repository isolation, ordering, expiry batch transitions, cleanup claims, and duplicate sweep behavior. After the quota-admission additions, `make test-preview-expiry-integration` reports 45.4% statement coverage in `internal/store/previews`; this does not cover a runtime provider or physical cleanup.
- Task 54 cleanup worker coverage (historical checkpoint): added unit cases for canceled input, missing dependencies, lease/timeout/batch bounds, and claim-ID generation/validation. The recorded 34.3% unit and 70.0% PostgreSQL coverage figures predate the Resource Plane command integration and do not measure live AWS/Gateway cleanup.
- Task 54/55 Preview quota follow-up: Migrations 000156–000157 add `quota_exceeded` and DB-owned default quota values for revision-only guard creation. PR open/reopen admissions and Workload Preview Runs share the Tenant quota OCC guard. `hako usage` includes awaiting and cleanup-pending PR Preview reservations, which remain held through cleanup confirmation. The webhook role can only create default-valued quota rows and advance the revision, not set policy limits. PostgreSQL integration verifies quota rejection, cross-type and concurrent admission denial, and release after cleanup. Runtime allocation/routing is implemented locally in the later Task 54 Resource Plane composition; AWS execution remains unverified.
- Task 54/55 Preview quota follow-up (historical duplicate checkpoint): Migration 000156 adds `quota_exceeded`; quota admission and PostgreSQL coverage are described above. Later Task 54 work implements local Resource Plane runtime allocation, routing and cleanup; AWS execution remains unverified.
- Task 46: Resource Plane Gatewayをdefault-off Terraform化。443 HTTPS-only ALB（長時間WSS向け60分idle timeout）からprivate ARM64 FargateへTLS転送し、digest-pinned app/Secret-loader images、ephemeral shared volumeへのSecretString file materialization、read-only/non-root app mount、six exact Secret ARNs、Resource Plane journal prefixとexact MicroVM-image IAM、caller-listed HTTPS egress、private subnet/no public IP、CPU autoscaling、health/ALB 5xx alarms、deployment rollbackを追加。OpenTofu mock planでdefault-off/enabled構成とvolume/IAM/TLS属性を確認。実配備/secret policy/DNS/network/live MicroVMは未実施。
- Task 85のローカル受入は完了。Gateway WSS→signed lease API→PostgreSQL disconnect/reconnect→idle sweepで、active reconnect中のSuspend拒否、再切断直後のgrace period、期限後の単一Suspend Operation/Outbox/system auditを検証。追加でtransactional Outbox Dispatcher→Placement Resource Plane queue→Resource Controller fake→Operation result consumerを通し、Outbox publish ackとPostgreSQLのterminal `observed_state=suspended`を確認。`hako resume ... --wait`を追加し、resumeは既定で非同期のまま、指定時はobserved `running`まで待機。`make test-workspace-idle-lifecycle-integration`通過。上流MicroVMはfake stream/runtimeで、実Guest reconnectとAWS deploymentは残る。
- Task 50のcorrelation IDをAPI/Webhook ingressからWorkload Run DB/API、Scheduler Outbox/SQS、Resource Plane execution metadata、Lambda MicroVM run hook、署名Agent binding、terminal result、Artifact metadata、通常CheckとActions Runner CheckのOutbox/consumer logまで伝播した。クライアント指定IDは無視し、idempotent create retryでは最初に永続化したIDを保持する。加えてWorkspace Operation create/action→Outbox→Dispatcher→Resource Plane result→Operation result consumerの相関伝播と失敗ログを実装。Resultの相関IDはControl Planeで元の不変Operation eventと照合する。API PostgreSQL E2Eでcreate/suspend request IDの一致と異なるIDのResult拒否を検証。Workload/Operation/Gateway/Artifact cleanup失敗ログはbounded failure classと信頼済みresource identityを記録し、生のprovider/database errorやstorage refを出さない。残りはGateway/cleanupのその他失敗経路棚卸し、production log/alert reviewと実AWS検証。

## 実装の完了定義

コードがあるだけでは完了にしません。各taskファイルに定めた受入条件を満たし、ユニット/integration/E2Eのうち適切なテストが通り、セキュリティ境界と未対応制約がドキュメント化されている必要があります。AWS applyや実環境cutoverなど外部変更を要する項目は、実装済みでも運用検証と区別して未完了のまま記録します。

- Task 53 Native Agent follow-up (2026-10-25): Added an injectable fixed-tool model loop (`list_files`, `read_file`, constrained `write_file`, fixed offline tests), bounded changes/turns/tool calls, and protected path/credential checks. Scripted tests cover successful write→test→finish, rejection of untested changes, and traversal/secret/symlink defenses. `cmd/hako-agent` selects this factory only with explicit `HAKO_NATIVE_AGENT_ENABLED=true` and a fixed model; default configuration rejects Agent model credentials, and Control Plane dispatch remains disabled. Safe GitHub branch/PR submission is now implemented locally in Task 89; pinned ARM64 image build/inside-MicroVM run, and target egress/cgroup validation remain open.
- Task 89 publishing follow-up (2026-10-25): added a fake-tested `AgentProposalGitHubWriter` and composed it behind a Control Plane-only mTLS Broker route. The Broker re-resolves current Agent/verifier/App/Repository/policy pins, reserves the deterministic publication row, reads the proposal from its identity-derived S3 key, mints the exact-scope Tenant App write token in-process, fences branch/PR creation, and records confirmed partial results for retry. Service and HTTP tests cover caller gating, stale-pin rejection, partial-result persistence, unknown fields, safe response and `no-store`. Broker route activation is opt-in through `HAKO_GITHUB_BROKER_AGENT_PROPOSAL_BUCKET` and matching optional artifact prefix. Deployed Broker configuration and MicroVM execution remain open. No live GitHub, real Secret retrieval, or AWS apply was performed.

- Task 89 follow-up (2026-10-25): added the authenticated Control Plane publication submit route and `hako workload publication submit`; the API forwards only the current attempt after requester/Tenant authorization and calls the mTLS Broker. Failed verifier attempts now retry at most twice after cleanup confirmation, keeping proposal and placement pins fixed while atomically updating the binding and creating a new attempt/Outbox. Active duplicate submit is idempotent; queued verification returns `202 verification_queued` and does not reach the Broker. PostgreSQL integration covers retries, idempotency, and exhaustion; API client tests cover the safe queued response. Paired default-off Terraform policies now grant the Broker `s3:GetObject` only on the attempt-scoped `agent-changes` ZIP path, and the managed Resource Plane bucket allows only that exact Broker principal/path. A composed PostgreSQL Broker route test exercises real resolver/recorder and synthetic archive/token/GitHub adapters end to end. Cross-account KMS key policy, deployed Broker configuration, Lambda MicroVM execution, and production gates remain.
- Task 53 follow-up (2026-10-25): Issue-to-Agent inbox processing now uses a five-minute OCC claim with exclusive concurrent ownership, expired-lease recovery, and a current-claim fence on Agent Run/task commit. PostgreSQL integration verifies a stale claimant cannot create the Run and the reclaimed worker can. The scheduled Lambda/mTLS Broker composition remains default-off and undeployed; Native Agent execution remains disabled.
- Task 53 follow-up (2026-10-25): The scheduled Webhook Processor can now compose the Agent Issue inbox batch behind a separate default-off flag, requiring Workload Run and Native Agent gates and a Control Plane mTLS Broker client identity loaded from configured files. Unit tests cover default-off/gate behavior, batch aggregation/retry redaction, and incomplete/unreadable TLS inputs. No certificate, private network, Terraform enablement, or Native Agent runtime was deployed; see [Webhook Processor](../docs/github-webhook-processor.md).
- Task 53 follow-up (2026-10-25): Repository model-access CLI failure tests cover invalid arguments, missing credentials/API URL, and Tenant `403`; the success path verifies the exact Tenant/Installation/Repository scope and keeps `execution_enabled=false`. Both CLI handlers are 100% statement-covered; `cmd/hako` is 72.5%. Native Agent remains default-off.
- Task 54 follow-up (2026-10-25): Preview log CLI failure E2E confirms an inactive Gateway Session is revoked after authorization fails; nil context/output and incomplete identity fail before network access. `readGitHubRepositoryPreviewLogsWith` coverage is 82.4%; focused tests pass under `-race`.
- Task 54 follow-up: Software Catalog CLI tests cover invalid input, missing login/API URL, Tenant `403`, nil output, invalid metadata before API access, and output failure. `parseSoftwareCatalogCLI` is 100%, `runSoftwareCatalogCLI` 89.7% statement-covered; local HTTP only.
- Resource Plane network observability (2026-10-25): Added a default-off caller-owned VPC Flow Logs contract with ALL traffic, 60-second aggregation, explicit CloudWatch retention/optional KMS, account-and-region-bound service trust, and log-group-scoped writes. OpenTofu mock plan tests cover disabled and enabled configurations. VPC/subnet/routes/NAT/endpoints are not created and no AWS apply was performed; see [Terraform modules](../docs/terraform-modules.md).

- Task 98 initrd packaging follow-up (2026-11-22): added `make build-local-workspace-initrd`, `make test-local-workspace-initrd`, and `scripts/package-local-workspace-initrd.sh`. The build compiles the local Guest PID 1 for the host architecture and packages an extracted Workspace image rootfs as gzip `newc` initramfs, requiring the Guest Agent, sshd, code-server and trusted Agent keys, rejecting an sshd ELF architecture mismatch, and requiring static host-native PID 1. The repeatable packaging target inspects fixture archive entries and rejects a wrong-architecture PID 1; it passes locally. The existing OCI image target still emits ARM64 only; native x86_64 Workspace rootfs build and booting the resulting Hako initrd in Firecracker remain open, as do operation API/deployment and end-to-end CLI composition.

- Task 92 trusted model policy follow-up (2026-11-22): added `AgentCorePolicy`, which accepts only an exact model ID and Region from deployment-owned JSON allowlists, bounds iteration/turn/tool/token/attempt-time settings, and constructs the existing Converse adapter with those call budgets. `execute_with_policy` propagates the validated model and iteration budgets. Official API documentation sets synchronous `InvokeAgentRuntime` to a 15-minute maximum, so policy caps the timeout at 900 seconds and leaves longer asynchronous jobs as a separate contract. Denied model/Region, malformed/duplicate allowlists, over-ceiling and policy-propagation tests pass. `make test-managed-runtime-local` passes all Go/Python local targets (45 Python tests). Runtime entrypoint/client creation, deadline enforcement, fake-provider lifecycle, and deployed behavior remain open; no Bedrock or AWS calls were made.

- Task 90 unverified proposal draft follow-up (2026-11-22): added `propose_changes` and `propose_with_policy` to produce a bounded proposal from digest-verified source using only the allowlisted model and fixed file tools. The response is explicitly an untrusted draft; it does not run tests or claim verification, and remains bound to caller-supplied attempt metadata that still requires authenticated dispatch and current-attempt revalidation. Tests cover model approval, review-time mutation, trusted policy propagation, and proposal identity; the full offline coding-agent suite passes (45 tests). Runtime HTTP entrypoint, regional Bedrock client, authenticated source/result Broker, durable recovery, independent verifier handoff, and cleanup composition remain unimplemented.

- Task 90 local Runtime entrypoint follow-up (2026-11-22): added the stdlib AgentCore HTTP contract (`GET /ping`, strict `POST /invocations`), strict duplicate/unknown JSON field rejection, attempt/source validation, inline archive and proposal limits, safe generic errors, `no-store`, regional Bedrock client creation, optional `boto3` runtime dependency and deadline checks around every model call. Fake-backed tests cover parsing, health/invocation HTTP behavior, proposal digest and deadline stop. Full Python suite now passes (49 tests); `make test-managed-runtime-local` also passes. The HTTP app does not authenticate caller-supplied pins, and an in-flight model request may exceed the local timeout by its 60-second read timeout. Resource Plane authenticated dispatch, Runtime image/deployment, source/result broker, journal deduplication, cleanup and independent verifier handoff remain open; no AWS invocation was made.

- Task 91 topology/workspace follow-up (2026-11-22): documented the per-repository, per-Tenant, and shared Runtime tradeoffs and resource ownership. The source workspace contract is one in-memory snapshot per `execute`, reconstructed only from the digest-verified source archive and discarded after the attempt; source/proposal storage remains Hako-owned and session persistence stays disabled. A unit regression now proves that writes in one attempt cannot mutate another attempt's workspace. Provider-backed restart, quota, cleanup retry, filesystem isolation and deployed IAM/egress evidence remain open.

- Task 98 Hako Guest boot follow-up (2026-11-22): built the native x86_64 Workspace OCI image from digest-pinned public artifacts and exact package pins, then built a 611 MiB gzip initrd with the running host kernel's required modules. The real Firecracker boot unpacks the initrd, loads virtio/vsock modules, mounts the persistent ext4 disk, enables loopback, and starts sshd. Non-root `hako-workspace-agent` exec as UID/GID 100000 fails with `EACCES`; a root-only diagnostic run confirms the executable itself starts, and PID 1 reports full capabilities, `NoNewPrivs=0`, and `Seccomp=0`. Agent heartbeat/orderly-shutdown acceptance, local operation API/deployment, and CLI composition remain open. Focused Guest init, Workspace Agent, and runtime tests pass; no AWS calls were made.

- Task 90 invocation-request follow-up (2026-11-22): added `workloadagentcore.EncodeInvocationRequest`, which validates current-command identity shape, Repository/base-commit/source pins, UTF-8 task bounds, and the archive bytes against the pinned SHA-256 before emitting the exact Runtime v1 JSON/base64 contract. Unit tests reject malformed input and archive mutation; an integration test feeds the Go payload to Python `parse_invocation`. Focused tests pass. This does not authenticate the input pins or implement dispatch/source broker/provider cleanup; no AWS API was called.

- Task 96 AgentCore lifecycle follow-up (2026-11-22): added local `InvokeOnce` composition over the frozen Runtime/session, v1 request builder, durable `Launching`→`Running` CAS claim, and an injectable Runtime provider. Fake tests cover identity rejection and at-most-once behavior after lost responses; focused Go tests pass. Production SQS dispatch, exclusive cancellation lease, recovery/cleanup/result publication and AWS adapter remain.

- Task 90 result-validation follow-up (2026-11-22): added `DecodeInvocationResponse` to bind Runtime response pins, verify the proposal ZIP digest, and run the existing bounded ZIP inspector before returning explicitly unverified bytes. Duplicate/unknown JSON fields, foreign pins, malformed base64/ZIP, protected paths, credential markers and changed bytes are rejected. `go test ./internal/workloadagentcore ./internal/workloadsource ./internal/workloadjournal -count=1` and the Go-to-Python request integration pass. This is early input hygiene, not independent verification; authenticated artifact handoff, AWS dispatch and cleanup/verification composition remain.
- Task 96 invocation-composition follow-up (2026-11-22): `InvokeOnce` now exposes only a typed attempt-bound draft after strict response and ZIP validation. Invalid or lost provider results remain durably `Running` and cannot be replayed. `workloadcontroller.AgentCoreWorkloadResult` composes validated drafts with the existing attempt-scoped artifact writer and cleanup-before-publish Result path. `make test-managed-runtime-local`, `make test-workload-runtime`, tagged Workload integration tests, race suites, and `git diff --check` pass; invoke/stop dispatch, cancellation/reconciliation lease, and deployed AWS adapter remain.
- Task 90/96 Workload result bridge follow-up (2026-11-22): `AgentCoreWorkloadResult` rejects attempt, Repository, commit, source digest, deadline, malformed ZIP, and artifact metadata mismatches. Proposal bytes use the existing attempt-scoped change-bundle writer; the ordinary Controller journals results and waits for cleanup confirmation before Control Plane publication. Fake unit tests, `make test-workload-runtime` (race), and tagged Workload integration tests pass. This does not connect an AgentCore provider, cancellation lease, or AWS runtime.
- Task 96 local stop/cleanup follow-up (2026-11-22): terminal attempts can now strictly restore the frozen Runtime/session and call an injected idempotent stopper using the stable attempt stop token. Provider errors remain unconfirmed; only successful stop plus journal CAS records `CleanupConfirmed`. A journal-CAS attempt lease now gates invocation and stop, supports renewal/release, and fences stale owners. `make test-workload-runtime`, AgentCore/workloadjournal race tests, tagged Workload integration tests, and `git diff --check` pass. Full worker wiring across cancel/result/publish, provider API adapter, and live AWS evidence remain.
- Task 96 journal composition correction (2026-11-22): frozen AgentCore Runtime/session pins now occupy dedicated journal fields and no longer overwrite the Workload Controller's frozen Workload/source specification. A regression confirms both pins coexist. Focused Go and race tests pass.


Task 90/96 lease renewal follow-up (2026-11-22): the AgentCore Worker renews
the durable attempt lease before InvokeAgentRuntime and at result, cleanup and
publication boundaries. Its 20-minute minimum lease covers the synchronous
API's 15-minute ceiling plus margin. Focused/race tests and tagged Workload
integration tests pass. Renewal is phase-boundary only; there is no background
heartbeat during source resolution, artifact persistence, cleanup, or publish.


Task 94 Terraform guard follow-up (2026-11-22): Python proposal tools and the shared Go ZIP inspector now reject Terraform source and state file formats anywhere in the repository, including case-insensitive `.tf`, `.tfvars`, `.tf.json`, state backups, and the lock file. Rejection is enforced before artifact storage and again during proposal application/publication. Python (49 tests), Go race tests for source/AgentCore/controller/Broker, `go vet ./...`, and `git diff --check` pass. No Terraform profile or plan execution is enabled.


Task 90/91/96 managed runtime routing follow-up (2026-11-22): the database-backed scheduler now pins Job attempts to `lambda_microvm` and Agent attempts to `agentcore`; cancellation preserves the same provider. The Outbox Dispatcher routes by provider and pinned Resource Plane to separate queue URLs and fails/retries when a dedicated URL is missing. Resource Plane manifests expose the optional `agentcore_command_queue_url`. Lambda and AgentCore SQS decoders reject messages pinned for the other runtime. Go unit packages and the PostgreSQL Agent Issue scheduling integration test pass. No queue Terraform, AgentCore Lambda deployment, AWS API, or AWS resource was used.

Task 90/92 AgentCore image follow-up (2026-11-22): added a digest-pinned Python 3.13 slim ARM64 Runtime Dockerfile, hash-locked boto3 dependencies, and a Kiri-only local build target. `uv lock --check`, generated-requirements comparison, all 49 Python unit tests, and `git diff --check` pass. Kiri image build is blocked because the Linux Kiri daemon needs root and its BuildKit could not resolve Docker Hub; no Podman fallback or AWS call was used. A successful image build/inspection remains open.

Task 94 verifier eligibility follow-up (2026-11-22): the fixed `go-offline-v1` Native Agent verifier now requires a root Go module and Go source and rejects mixed Python/Java/Rust/TypeScript/JavaScript or Terraform source/state trees before command execution. `go test -race ./internal/workloadagent -run 'NativeAgentGoProfile' -count=1` passes. Other language profiles remain gated on immutable verifier toolchain images; Terraform authorization remains gated on explicit policy/IAM review. Unavailable checks remain fail-closed.

Task 98 local operation API follow-up (2026-11-22): `workspaceruntime.LocalOperationAPI` now adapts the normal Resource Controller command/result contract to `LocalMicroVM`. The Linux startup reconciler serves it over a same-user mode-0600 Unix socket, validates private socket-parent ownership, rejects active/foreign socket replacement, and continues periodic journal recovery. Its launch resolver binds Guest identity and configured Gateway origin to the trusted operation and journal generation. `make test-local-workspace-reconciler` (race tests) and `make build-local-workspace-reconciler` pass. Added `make smoke-local-workspace-controller`; the real Firecracker v1.18.0 KVM Ensure/Suspend/cold-Resume/Delete flow passes, confirms the Agent identity heartbeat before acknowledging running, preserves allocation ID/generation, and removes VM state on Delete. No AWS environment variables or services are used. Full Control Plane/API/CLI lifecycle composition remains.

Task 98 local Outbox worker follow-up (2026-11-22): added a filtered PostgreSQL Outbox claim for only `operation.requested` Workspace events and `cmd/hako-local-workspace-worker`, which sends commands over the private Controller Unix socket, checks response identity pins, applies the standard Operation Result transaction, and acknowledges only after commit. Failed delivery/result persistence releases the lease with bounded backoff; other event families are not leased. `make test-local-workspace-worker` and `make build-local-workspace-worker` provide focused validation. Local API identity/auth configuration, service composition, and authenticated CLI lifecycle remain.

Task 98 local provider projection follow-up (2026-11-22): local runtime results use the distinct `firecracker_local` resource binding kind. Control Plane result decode/validation, resource-binding replacement/deletion, and Workspace connection/idle lease lookup now support both `firecracker_local` and existing `lambda_microvm` bindings. PostgreSQL integration covers local binding persistence, provider-kind replacement, deletion cleanup, and local connection lease/idle lookup. Race tests and focused PostgreSQL integration suites pass. Local API identity/auth, deployment composition, and authenticated CLI flow remain.
