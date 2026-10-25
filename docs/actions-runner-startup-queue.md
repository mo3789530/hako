# Actions Runner startup queue

Status: local Control Plane dispatch composition, Resource Plane consumer
handler, and default-off Terraform configuration for the queue/DLQ, cross-
account publisher policy, worker role, Lambda function, and partial-batch
event-source mapping are implemented. AWS resources have not been applied.
Multi-Resource-Plane placement and live AWS delivery remain unimplemented.
Runner admission and dispatch remain opt-in.

## Durable GitHub Check lifecycle

For admitted jobs, Hako creates a `Hako Actions Runner` desired Check in the
same PostgreSQL transaction as the admission row and initial Check outbox
event. Exact `workflow_job.in_progress` and `workflow_job.completed` webhook
events advance that Check by `(integration_id, workflow_job_id, run_attempt)`;
GitHub's completed event is authoritative for its terminal conclusion. A
Control Plane Outbox Dispatcher routes those events to the existing GitHub
Checks queue, and its consumer reloads the latest durable revision before
publishing, so stale or duplicate queue deliveries cannot overwrite newer
state. The returned GitHub Check Run ID is persisted only after the API
response succeeds. If MicroVM startup fails before GitHub reports a job start,
the exact admitted attempt gets a durable failure Check in the same transaction
as its startup-failure/cleanup transition.

Check publication credentials are issued by the Control Plane-only mTLS broker
endpoint `POST /v1/actions-runner/checks-token`. The broker resolves the exact
Tenant, integration, Installation, repository, job, attempt and credential
generation from durable state, requires the repository Actions Runner opt-in,
then mints only repository-scoped `checks:write`. It re-resolves identity after
minting to fence concurrent credential changes. The Resource Plane does not
receive this Check token. The feature uses the existing Checks queue and
consumer; it adds no public endpoint or new AWS runtime. Local unit and
PostgreSQL integration tests cover the outbox-to-dispatcher-to-fake-GitHub
delivery and Check ID persistence. AWS deployment, certificates, Secrets
Manager provisioning and live GitHub verification remain unperformed.

## Runner diagnostic upload

The guest collects regular `.log` files from the runner `_diag` directory,
caps the payload at 64 KiB, redacts credentials, computes a SHA-256 digest, and
uploads it over HTTPS before ephemeral runner state is removed. The signed
bootstrap binds the upload URL to the exact integration, workflow job, and
attempt. The bearer capability is derived from the random startup claim ID;
only its hash is stored with the durable job. The public API route is
capability-authenticated and accepts only the matching job/attempt and a
bounded raw payload. Control Plane applies a second redaction pass and stores
only the payload digest plus an escaped, truncated 3 KiB excerpt on that exact
Check row. The normal Check Outbox publishes the updated summary. Full runner
diagnostic logs are not retained in DSQL or S3. Replaying the same digest is
idempotent; a different payload for an already attached attempt is rejected.

Configure `diagnostics_upload_base_url` on the Resource Plane deployment with
the HTTPS Control Plane API origin. The MicroVM egress connector must allow the
resulting Control Plane API route. Local guest, API, PostgreSQL, and Terraform
mock coverage is exercised by `make test-actions-runner-lifecycle-integration`
and the Resource Plane Terraform test. This does not verify deployed API
reachability, real Lambda MicroVM execution, or AWS deployment. A failed guest
upload is reported only as a boolean process diagnostic; GitHub's terminal
`workflow_job` event remains authoritative for job outcome.

## Flow

```text
workflow_job.queued
  -> durable admitted row
  -> scheduled Control Plane Outbox Dispatcher
  -> bounded DB claim lease
  -> dedicated SQS startup queue
  -> Resource Plane Lambda partial-batch handler
  -> mTLS broker claim consumption
  -> Lambda MicroVM launch / bind / bootstrap
```

The scheduled Outbox Dispatcher runs the runner admission publisher only when
one Resource Plane manifest entry has an
`actions_runner_startup_queue_url`. The queue URL and signing region come from
that validated entry. The publisher claims at most 10 admitted/recoverable jobs
per invocation with a 10-minute lease. Each SQS
message contains only `schema_version`, `integration_id`, `workflow_job_id`,
`run_attempt`, and `claim_id`; Tenant, Repository, commit, labels, and
credentials are resolved later by the trusted broker from the database.

Delivery is at-least-once. The database claim is committed before `SendMessage`
is called, so a failed or ambiguous send is recovered after lease expiry and
may result in duplicates. The Resource Plane broker accepts a claim only while
it is live; same-Plane replay before token issuance is idempotent. The SQS
consumer returns per-record failures, allowing successful records in a batch
to remain acknowledged. FIFO queues use deterministic group/deduplication IDs;
standard queues rely on claim fencing.

The Control Plane manifest accepts at most one Actions Runner startup queue.
Multiple queue entries fail closed until durable placement exists. This is
suitable only for a single configured Resource Plane; it does not yet perform
capacity/region placement or persist a selected Plane before dispatch.
Multi-plane routing must add a durable placement decision and per-Plane queue
mapping before production use.

## Claude Action workflow provenance (local launch gate implemented)

Claude Code Action remains a user-authored GitHub Actions workflow; Hako does
not translate issue or pull-request comments into commands. The local
`actionsrunner.VerifyRunnerWorkflowProvenance` contract accepts only a
Tenant-owned, data-only policy and authoritative run/job metadata. It compares
Tenant, integration, Installation, repository, workflow file, approved ref,
commit, run ID/attempt, job ID, event, source repository and required runner
labels, and rejects fork-originated work. Policy values cannot select a
command, credential, image, execution role, or network connector.

The trusted Tenant App token client can mint a repository-only
`actions:read` Installation token for that lookup (without `contents:read`). The
repository opt-in is checked both before and after token minting; the token is
distinct from checkout, runner-registration, and deregistration credentials.

`githubapp.ActionsRunClient` uses that token to fetch the exact run, requested
attempt's paginated jobs, and workflow path, including workflow/job names and
head branches. It rejects foreign repository or run identities, absent/duplicate
jobs, unexpected run attempts, oversized responses, redirects, and malformed
workflow paths. The broker launch composition invokes the lookup before claim
consumption. It verifies the exact persisted queue identity and policy; the
transactional starter compares the policy JSONB snapshot again while consuming
the claim, denying policy changes and legacy unverified launch calls.

The data-only policy is stored per Tenant/Installation/Repository through an
Owner/Admin API and CLI; disabling runner opt-in clears it transactionally.
Example `policy.json`:

```json
{
  "workflow_path": ".github/workflows/claude.yml",
  "allowed_refs": ["refs/heads/main"],
  "allowed_events": ["push", "workflow_dispatch"],
  "required_labels": ["self-hosted", "hako"],
  "allowed_labels": ["self-hosted", "hako", "arm64"]
}
```

The workflow remains authored and maintained in the repository; Hako never
creates commands from issue/PR text or rewrites `.github/workflows`. See
[`claude-code-action.md`](claude-code-action.md) for the runner labels, event
limits, and Action pinning contract. The broker launch composition fetches
authoritative GitHub run/job/workflow metadata and verifies it against the
Tenant-owned policy before consuming the startup claim. Claim consumption then
rechecks the exact policy snapshot transactionally, so a policy change between
preflight and launch fails closed.

Tasks 74–77 provide local provenance, credential-isolation, fake-provider
execution, and PostgreSQL end-to-end evidence. This does not certify the
official ARM64 image on Lambda MicroVMs or a live GitHub/AWS deployment. Claude
Action execution remains a deployment-gated capability: keep its policy and
runner admission disabled until the pinned image, restricted network, broker
certificates/IAM, and cleanup path have been reviewed and verified in the
target Resource Plane. No queue consumer bypasses the broker launch gate.

## Local verification

Run the focused local lifecycle suite with the existing PostgreSQL instance:

```sh
make test-actions-runner-lifecycle-integration
```

The suite includes admission/claim/binding, signed guest bootstrap and a fake
runner process, Check outbox delivery, and terminal webhook cleanup recovery.
The composed cleanup integration seeds an active Tenant-owned GitHub App and
starts by proving a tampered HMAC is rejected. It then sends signed
`workflow_job.queued`, `in_progress`, and duplicate `completed` deliveries
through the production Tenant HTTP handler and DB-backed Installation
authorizer. After admission and lease, it runs the startup worker with a fake
MicroVM launcher and runner-token provider. The model-credential path uses the
production HTTP client, Broker route, and PostgreSQL resolver; an in-memory
transport supplies a synthetic verified SPIFFE certificate. The production
signed bootstrapper, guest `/run` handler, and runner process consumer then
run a fake runner script, covering success, failure, cancellation, and
timeout. This exercises application-level Broker identity authorization but
not a cryptographic TLS handshake, real Lambda MicroVM, or live GitHub API.
All queued/in-progress/terminal Check
Outbox revisions pass through the production Dispatcher and Checks consumer to
a fake GitHub Checks API; Check identity/result and durable Check Run ID are
verified in PostgreSQL. The consumer's credential path uses the production
PostgreSQL Actions Runner Checks resolver and issuance fence with only the
token minter faked: unknown integration, wrong job and revoked App requests
must fail before minting, while generation rotation during mint invalidates
the grant. The same startup test uses the production `modelcredentialbroker`
resolver against PostgreSQL, with separate active Tenant A and Tenant B model
credential rows and a fake Secret reader that refuses any Tenant except A.
Fork, `pull_request_target`, foreign head-repository, cross-Tenant and wrong
authoritative-SHA provenance variants are denied before the model Secret
reader runs. A foreign Tenant integration paired with Tenant A's job is also
denied without reading either model Secret. Only after a valid provenance
policy hash and exact MicroVM binding are persisted does the fake reader return
a synthetic key, which passes through the Broker HTTP response and signed
guest bootstrap into the runner process environment and is then wiped. No
real Secret Manager access or provider credential is used. The cleanup worker
uses the production HTTP queue client and Broker handler backed by PostgreSQL to terminate a stateful fake
MicroVM and deregister its fake runner. Both external operations fail once,
and recreated workers recover with durable backoff until both resources are
absent. Duplicate completion events do not create another cleanup request.
The composed lifecycle does not call real GitHub, AWS, or Claude APIs. CI runs
this target explicitly. Tests create and drop isolated schemas and leave the
local database running.

```sh
GOCACHE=/tmp/hako-go-cache go test ./internal/actionsrunner ./cmd/hako-dispatcher-lambda
```

Tests use fake claim sources, fake SQS, and fake runner dependencies. They do
not call AWS, read real Secrets, or require SQS. The runner Lambda entrypoint
and ARM64 ZIP build are documented in
[`tasks/task-52-ephemeral-actions-runner.md`](../tasks/task-52-ephemeral-actions-runner.md).

## Cleanup leases and recovery

Each cleanup attempt has a fresh claim ID. Acknowledgement and retry updates
also compare the durable attempt count and exact lease expiry, so an older
attempt cannot change a recovered lease even if a caller reuses its worker
name. Worker configuration must reserve at least five seconds between the
operation timeout and lease expiry for acknowledgement. The worker rejects a
mismatched or nearly expired lease before calling the provider and limits
provider work to the remaining lease budget.

MicroVM termination must finish before runner deregistration. Failure keeps
the cleanup row `pending` with bounded backoff and a fixed safe error category;
it is never acknowledged as clean. On retry, successful external operations
are repeated idempotently. A terminal GitHub Check is evidence of job outcome,
not evidence of physical cleanup. Operators should correlate integration/job/
attempt and pinned Plane/MicroVM/runner IDs, inspect `state`, `attempt_count`,
`available_at` and `last_error`, and verify provider absence before considering
an incident resolved. Do not clear a pending row or reuse a runtime to hide a
cleanup failure. The local PostgreSQL queue adapter is a Control Plane/library
component. Broker-mediated claim/ack/retry transport and the scheduled Lambda
composition are locally implemented in `hako-actions-runner-lambda` cleanup
mode. It drains at most 100 jobs per invocation (default 20), records
provider-level failures with durable backoff, and fails the invocation for
lease or persistence errors. Configuration requires a private Broker HTTPS
endpoint, three distinct exact mTLS Secret ARNs, a Resource Plane identity,
and bounded lease/operation/poll settings. The same ARM64 ZIP supports
`HAKO_ACTIONS_RUNNER_MODE=cleanup`; default/`startup` mode remains the SQS
startup consumer. The scheduled function role, EventBridge schedule, networking
and Secret provisioning are not deployed yet; keep admission disabled until
those are reviewed and enabled deliberately.

## Private cleanup queue API

The GitHub Broker mounts these routes when
`HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS` contains an approved
Plane. Each request requires its verified Resource Plane mTLS identity:

- `POST /v1/actions-runner/cleanup-queue/claim`: a fresh claim ID and lease
  duration (5–600 seconds); returns at most one cleanup job for that Plane.
- `POST /v1/actions-runner/cleanup-queue/complete`: integration/job/attempt,
  claim ID, attempt count and exact lease expiry.
- `POST /v1/actions-runner/cleanup-queue/retry`: the same lease identity,
  a relative delay (1–900 seconds) and a fixed safe error category.

Plane identity comes only from the certificate. The Broker resolves Tenant,
repository, runner and MicroVM from the database; callers cannot supply these
coordinates in mutation requests. Acquisition and acknowledgement use the
Broker's clock, and retry delays are applied to that clock. Cross-Plane,
expired, replaced or altered leases cannot mutate state. Queue access remains
available after a repository or integration is disabled, allowing MicroVM
termination to proceed. GitHub deregistration still requires an active App and
Installation credential binding; revoked access leaves the cleanup pending for
operator resolution rather than bypassing revocation. A row with no Plane and
no MicroVM is pinned to the acquiring Plane when leased; no runtime termination
is needed for it. A row pinned to another Plane cannot be claimed.

Successful responses use `Cache-Control: no-store`; `applied=false` means an
empty queue or stale mutation. Transient database failures return a fixed 503
response. The HTTP client bounds requests, disables redirects, validates
responses and implements the worker queue interface. It requires no database
credential in the Resource Plane. Queue responses contain lifecycle metadata
only, never registration, App or model credentials.

The focused integration suite covers worker→HTTP client→Broker→PostgreSQL
cleanup/recovery plus denied certificate identities, foreign Planes, modified
leases, reused claim IDs, caller clock manipulation and expired-lease recovery.
These tests use an in-process transport with synthetic verified certificate
state; the separate Broker mTLS suite tests cryptographic handshakes. No new
route has been deployed or verified against live AWS/GitHub resources.

## Not yet deployable

- Review and approve the default-off Terraform queue/DLQ, queue policy, worker
  IAM and event-source mapping; apply requires separate user approval.
- Confirm queue visibility exceeds six times the configured Lambda timeout and
  the Lambda timeout exceeds the runner startup operation timeout.
- Add durable multi-Plane placement and queue selection; do not route by
  untrusted workflow labels.
- Execute the built ARM64 runner image inside Lambda MicroVMs and verify
  restricted egress, cancellation, and cleanup under approved AWS/GitHub test
  credentials before enabling admission.

## Pinned runner MicroVM image

`images/actions-runner/Dockerfile` extends the official GitHub Actions Runner
2.338.1 ARM64 image pinned by platform manifest digest
`sha256:ee680dc9fe83da474e3c51d642f8bfac0d920da26735e3d5e4171ab8f2f45a87`.
The upstream version and package are documented at the [official Runner
package](https://github.com/actions/runner/pkgs/container/actions-runner).
The Hako layer adds only the statically linked ARM64 guest executable and the
public bootstrap verification key, sets `/home/runner` as the Runner root,
and retains the upstream non-root `runner` user. The image build does not
download or execute an unpinned Runner archive.

Build locally with a public key file:

```bash
make build-actions-runner-microvm-image \
  HAKO_ACTIONS_RUNNER_BOOTSTRAP_PUBLIC_KEY_FILE=/path/to/bootstrap-public.pem
```

The build uses Podman and does not publish an image or change AWS. On an
x86_64 host without ARM emulation, the OCI image can be assembled and inspected
but its ARM64 processes cannot be executed locally. A built image is not proof
of Lambda MicroVM runtime, network isolation, or live GitHub registration.
