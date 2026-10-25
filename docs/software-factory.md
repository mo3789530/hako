# Hako Software Factory (roadmap)

## Product direction

Hako can grow from a Remote Development Platform into a Software Factory by
making GitHub the development entry point and keeping execution, environments,
automation, and workload isolation in Hako:

```text
GitHub App / Actions
        │ Webhooks and job requests
        ▼
Hako Control Plane
  repository registry · normalized events · pipeline/job scheduler
        │ Workload command
        ▼
Hako Resource Plane
  isolated runtime · runner/agent · services · artifacts
        │ check result / logs / artifacts
        ▼
GitHub Checks / Pull Request / Actions
```

This is an extension roadmap, not a claim that the full GitHub Factory, the AWS
Workspace Runtime, or runner execution already exists. The current Hako core
remains the system of record for Tenant, Workspace, Placement, Operation, and
Resource Plane. Workspace remains distinct from a MicroVM; this design adds
short-lived Job and Agent workloads without turning a Workspace into a job.

## Implementation status

The API can optionally expose `POST /v1/integrations/github/webhook` when
`HAKO_GITHUB_WEBHOOK_SECRET_ARN` is configured. The handler validates the
exact raw-body `X-Hub-Signature-256`, bounded JSON, UUID-shaped Delivery ID,
and initial event/action allowlist, then durably records the raw delivery and
version-1 normalized Hako event in the Control Plane database. Replays with
the same ID and payload are acknowledged as duplicates; ID reuse with
different content is rejected. Terraform grants the API Lambda read access
only to the configured Secrets Manager ARN and creates an unauthenticated API
Gateway route only when enabled. No GitHub App is registered automatically.
The webhook processor handles Installation lifecycle and repository-membership
events, and supports versioned push/pull-request normalization with a
default-off, per-Repository verification trigger. `workflow_job` is normalized
and has default-off admission, token-broker, status, and cleanup-outbox
primitives. Lambda MicroVM launch and cleanup consumers are not connected yet.
Tenant owners/admins can
submit inert installation claims through the tenant API; those claims remain
`pending`. A separate opt-in setup flow redirects through the GitHub App
Installation setup URL and then GitHub OAuth authorization. Hako exchanges the
short-lived code, lists Installations visible to that user, verifies the
configured App ID, rechecks the Tenant role, and consumes one-time state while
creating the active binding in a single transaction. User access tokens are
not persisted. The repository list endpoint only serves an active tenant
binding. A storage operation can apply normalized repository-added/removed
events, and a transactional Inbox processor resolves the active Tenant binding
and marks those deliveries processed atomically. An opt-in one-minute
EventBridge Lambda drains batches of at most 10 eligible schema-v1
`installation_repositories` and Installation lifecycle deliveries using the
dedicated `hako_webhook_processor` DSQL role. Installation `suspend` blocks
Repository access; `unsuspend` restores it only for an already-active claim;
`deleted` revokes the claim and removes synchronized Repository rows. Webhooks
cannot create or reactivate a Tenant binding. Unsupported event types remain
pending. See
`docs/github-installation-setup.md`, `docs/github-webhook-processor.md`, and
the relevant integration tests. A separate Workload Run domain/store models
Job/Agent/Preview lifecycle, Tenant-scoped idempotency, revision-guarded
transitions, timeout/cancellation, and redacted artifact references. A local
Fake Runtime can run an immutable Go test commit in isolated Podman, persist
terminal state/artifact metadata, and write logs through an S3 backend using
SSE-S3 or optional SSE-KMS. A local PostgreSQL E2E composes verified-delivery
inbox processing, fixed untrusted Run creation, Podman execution, redacted
artifact metadata, the durable Check outbox/dispatcher, and a fake GitHub
Checks API. CI installs Podman and requires the Go image for this E2E; it uses
no public GitHub credentials or AWS resources. Scheduler-to-Lambda MicroVM
worker dispatch, signature ingress in this combined scenario, and the broader
failure/cleanup matrix remain incomplete. Passing this local E2E does not
mean a production worker is deployed. See
`docs/software-factory-workloads.md` and Task 42.

## Integration model

### GitHub App and tenant/repository ownership

Use a GitHub App rather than a personal access token or an individual's OAuth
token. Start with the smallest repository permissions needed by the first
feature and add permissions only when a feature is enabled. A future
permission matrix should be generated from the exact REST endpoints and
webhook subscriptions in use; GitHub App permission requirements determine
both API access and available webhook events.

| Capability | Initial permission direction |
| --- | --- |
| Identify installed repository | Metadata: read |
| Clone source for verification | Contents: read; token限定は[GitHub App credentials](github-app-credentials.md) |
| Report CI results | Checks: write |
| Receive workflow job lifecycle | Actions: read, subject to exact webhook/API requirements |
| Issue-driven agent context lookup | Issues: read + Contents: read, both limited to the selected Repository; no write permission |
| Push branch/open PR | Contents and Pull requests: write, isolated to an explicit Agent workload |
| Register/delete self-hosted runner | Separate runner-manager App/credential; repository Administration or organization runner permission as required by GitHub |

The Issue-comment webhook normalizer accepts only `created` deliveries and
stores Issue number, comment ID, pull-request classification, author
association, and a closed command enum. Only an exact bounded `/hako implement`
from a GitHub `OWNER`, `MEMBER`, or `COLLABORATOR` association becomes a
command; free-form comment text is never included in the normalized event and
is redacted from the short-lived raw-payload column. Duplicate detection still
uses the hash of the original signed bytes. This is not authorization or
scheduling: the remaining flow must verify Tenant membership and the active
Tenant-bound Repository/Installation, resolve an immutable commit before
creating an Agent Run, and keep branch/PR writes disabled unless a separate
policy explicitly enables them.

Do not grant every permission to one always-on integration by default. Keep
the runner-management credential distinct from normal repository/check
operations because runner registration requires elevated repository or
organization permissions. Store App private keys and webhook secrets in
Secrets Manager, not DSQL, source control, Lambda environment text, or
Workspace environment variables. Issue short-lived installation tokens for
the selected installation/repository and request only the permissions needed
for each operation.

Persist the installation-to-Tenant relationship and stable GitHub repository
ID; owner/name are display and routing fields, not durable identity. A
repository must have exactly one owning Hako Tenant unless an explicit
multi-tenant sharing model is later designed. Revoke repository access when
an installation is removed or the repository is removed from the installation.

The initial multi-tenant model uses one Hako-owned GitHub App installed by
multiple customers: each GitHub Installation belongs to exactly one Hako
Tenant, and each installed repository is registered under that Tenant. Do not
create a new GitHub App per Tenant for the standard flow. The separate
[Tenant-owned App design](tenant-github-apps.md) and [ADR 0007](adr/0007-tenant-github-apps.md)
define opt-in per-Tenant Apps (Tasks 56–59 are partially implemented; Tenant
App activation remains default-off, and Task 60/E2E work remains). The
Tenant-owned webhook route and purpose-limited credential reader are also
implemented behind a separate default-off flag; real per-Tenant IAM trust and
Secret provisioning have not been deployed. Payload App-ID consistency is
checked when the Installation payload includes it. Pinned OAuth state and an
injectable setup/callback adapter are implemented, while production OAuth
broker wiring remains required before enabling this path. Never select a tenant based
only on an unverified request header or repository name.

`POST` and `GET /v1/tenants/{tenant_id}/github/installations` are owner/admin
routes for recording and viewing pending installation claims. A request is
not proof of installation ownership: it does not create the global active
binding and cannot be used to mint an installation token. Only a verified
GitHub App setup callback should transition a claim to active. The active
binding table enforces that one Installation ID cannot be owned by multiple
Hako Tenants; repository rows reference that active binding.

Do not activate a claim based only on the `installation_id` or `state` query
parameters on GitHub's setup redirect. GitHub warns that the installation ID
can be spoofed; the callback must use a GitHub user access token and verify
that the installation belongs to the authorizing user before creating the
active binding ([GitHub setup URL guidance](https://docs.github.com/en/apps/creating-github-apps/registering-a-github-app/about-the-setup-url)).

### Webhook ingress and normalized events

The public webhook ingress verifies `X-Hub-Signature-256` against the exact raw
request bytes using HMAC-SHA256 before parsing or acting. Reject missing/bad
signatures, unknown installation/repository IDs, oversized bodies, and
unsupported event/action pairs. Use `X-GitHub-Delivery` as an idempotency key;
record/claim a delivery durably before acknowledging it so GitHub redelivery
does not start a duplicate workload. Keep the raw payload only when needed for
audit/debug and apply strict retention/redaction.

Normalize GitHub-specific events to versioned Hako events rather than leaking
GitHub payloads into scheduler/runtime interfaces:

```go
type RepositoryEvent struct {
    SchemaVersion int
    DeliveryID    string
    TenantID      string
    RepositoryID  string
    Type          string // repository.pull_request.opened, workflow_job.queued, ...
    Ref           string
    CommitSHA     string
    ActorID       string
    Payload       json.RawMessage
}
```

Initial event subscriptions should be narrow: installation/repository changes,
push, pull request, and workflow job only when the corresponding features are
enabled. `issue_comment.created` can now be safely normalized for the bounded
Agent command groundwork above. A transactional authorization resolver now
revalidates the active Tenant/Installation/Repository binding, requires the
Factory opt-in to predate delivery receipt, and maps the verified numeric
GitHub sender ID to a current member of that Tenant. It rejects PR comments
and returns only bounded Issue identity. The Control Plane-only broker route
uses a separate Tenant-pinned
`issues:read` + `contents:read` scope, and its bounded reader resolves an open
Issue plus the default-branch tip to an immutable commit SHA. These libraries
are connected to the broker endpoint. A separate Issue-command processor
preflights Tenant authorization, then atomically revalidates it, creates/reuses
an Agent Run, stores the bounded Issue task in `workload_agent_tasks`, and marks
the delivery processed. A separate Broker mTLS endpoint can return that task
only to an explicitly allowlisted Resource Plane and only for the current,
unexpired attempt after rechecking Tenant membership, Factory opt-in, App
binding, and the Run's immutable commit/branch/Repository pins. The broker role
has read-only task access; the API role and user-facing Run responses do not
expose Issue body text. The Resource Plane Worker now retrieves this task over
its mTLS Broker client, checks the attempt and immutable pins, and transfers
bounded text only through the authenticated MicroVM Agent protocol. A context
digest is pinned in the launch specification, signed Start/Source tickets, and
the MicroVM `/run` binding; the guest rejects a Start whose task differs from
the signed digest. The canonical hash covers Tenant/Run, Integration/Delivery,
Installation/Repository, Issue/comment IDs, branch/commit, title, and body.
Control Plane persistence, Broker validation, Worker, and guest share the same
digest implementation. Task text remains in memory and is excluded from journals,
results, and logs. This transport does not enable scheduling: dispatcher and
batch processing remain Job-only, and the default guest process factory rejects
Agent execution until an isolated model-backed executor, Tenant model-credential
policy, and runtime resource/network enforcement are ready. Check-run requests, release, and
workflow-run events remain future work. Webhook receipt and GitHub API mutation are separate operations:
acknowledge accepted deliveries promptly, then process from a durable queue or
outbox with retry/idempotency.

Resource Plane execution metadata schema v3 includes the Workload `kind` and
correlation ID. The resolver accepts legacy v1/v2 metadata as `job` for
backward compatibility; v3 accepts only `job` or `agent`, and permits an
`agent` only for an `issue_comment` Run with an immutable Tenant App pin. The
Resource Plane materializer and launch-spec hash preserve this kind. The Worker
can materialize and securely transport an admitted Agent task, but the
dispatcher still accepts Jobs only and the default guest executor fails Agent
Runs closed. Scheduling and model-backed execution remain disabled until the
isolated executor, Tenant model-credential policy, and runtime resource/network
enforcement are wired.

### Workloads and execution modes

Represent common dispatch metadata consistently while retaining explicit
domain types and lifetimes:

```text
WorkloadRun
  kind: job | agent | preview
  tenant_id, repository_id, commit_sha, trust_level
  desired/observed state, placement, operation, timeout
  artifact references, check-run ID, audit context

Workspace
  long-lived interactive development environment
```

One Job run gets one isolated ephemeral runtime, and one Agent run gets its own
bounded runtime and permissions. Do not run unrelated pull-request jobs in the
same mutable runtime. GitHub Actions Runner Mode preserves existing workflow
YAML; Hako Native Mode executes a versioned `.hako/factory.yaml`. Support both
without silently taking over or rewriting existing workflows. Start with
Runner Mode only after the Runtime can provision, connect, collect logs, and
destroy an isolated job environment.

For a self-hosted GitHub Actions runner, register it as ephemeral so one
runner processes at most one job, then de-register and destroy its runtime.
Forward runner diagnostic logs externally before deletion. GitHub currently
recommends ephemeral runners for self-hosted autoscaling; it also documents
ARC and its Runner Scale Set Client as autoscaling approaches. Hako should
integrate with supported runner registration/scale-set APIs rather than
depend on an undocumented polling loop. A queued job is not proof a runner is
ready; report capacity/startup failure and let GitHub/Hako retry policy remain
bounded.

### Checks, artifacts, and deployment identity

Use a Run-level Checks API check as queued/in-progress, then update it with
the aggregate conclusion and summary. Native manifest pipelines additionally
create one completed `hako/<kind>/<pipeline>` check per selected pipeline.
These pipeline checks are committed with the terminal Run result and delivered
through the durable Checks outbox/consumer; they do not currently stream
per-pipeline progress. Future annotations are line-scoped, so retain source
path/line mapping through log/result collection. Tie every check to the exact
repository ID and commit SHA; never report a result on a mutable branch name
alone.

Store test reports, logs, coverage, SBOMs, binaries, and container images in
S3/ECR (or a configured artifact backend); DSQL holds metadata, status, and
retention references only. Artifact cleanup and access checks must follow
Tenant ownership and run retention. AI findings are untrusted suggestions,
not authorization or deployment decisions.

For cloud deployment, use GitHub Actions OIDC only where the workload needs
cloud access. Require a cloud trust policy constrained by repository,
ref/environment, and expected workflow claims; untrusted fork PRs receive no
cloud credentials. Never place long-lived cloud keys in a GitHub secret,
Hako database, runner image, or job artifact.

## Trust and execution policy

Trust is explicit on every workload, for example `untrusted`, `trusted`, or
`privileged`. Fork PRs default to `untrusted`: no deployment credential,
production secret, shared writable cache, or unrestricted internal network.
Agent workloads default to no secrets and restricted outbound network; any
permission to push a branch or create a PR is granted per run and audited.
Production deployment requires a trusted protected ref and configured human
approval. Never infer trust merely from a workflow label, issue comment, branch
name, or self-reported YAML setting.

Native Agent model credentials are resolved by the Resource Plane Worker
through the allowlisted mTLS Broker only after the active Run and immutable
Issue context have been validated. The Worker sends the key only in the
no-store HTTPS Start request authenticated for that MicroVM and signed Agent
attempt. The Guest passes the byte buffer only to a process factory that
explicitly declares model-credential support, clears transient buffers, and
redacts the key from bounded output. The default Go-test factory does not have
this capability; no production Native Agent executor is configured and Agent
execution remains disabled. Local tests use synthetic keys and do not read
AWS Secrets Manager.

Treat `.hako/factory.yaml`, workflow files, action references, shell steps,
container images, cache contents, and webhook payloads as untrusted inputs.
The Native pipeline schema needs a version, strict parser, allowlisted step
types, runtime/time limits, maximum artifact sizes, and explicit network and
secret policies. Shell execution is a capability granted by policy, not a
trusted declarative value. Pin or policy-check third-party actions/images
before production use.

The Lambda MicroVM image runs the trusted `hako-agent` as root so it can own
runtime control sockets. Repository-controlled catalog processes are started
with an explicit empty supplementary-group list as UID/GID `65532:65532`.
This separates job code from the Agent's root identity and prepares the
containerd socket boundary. A separate opt-in image build includes a dedicated
rootless-containerd daemon account and manager; `HAKO_JOB_SERVICES_ENABLED`
defaults to false. The manager checks the subordinate ID range, setuid mapping
helpers, tmpfs runtime directory, and socket ownership. The workload image now
installs an IPv4/IPv6 loopback-only OUTPUT guard for the Job UID and rootless
containerd UID range before opening the Agent listener, and fails closed when
the rules cannot be installed. This is defense in depth; target MicroVM kernel
support, container traffic, and the Resource Plane connector/firewall policy
remain unverified. The image target and daemon have not been exercised in a
Lambda MicroVM, so the trusted resolver continues to reject Service pipelines.

### Issue Agent repository write policy

Tenant Owners/Admins can store an explicit per-Repository Agent write ceiling
with `hako github repository agent-policy set --tenant ... --installation ...
--repository ... --policy-file agent-policy.json`. An enabled policy must name
one or more safe branch prefixes, at least one required `hako/...` Check, and
require those Checks to pass. Pull-request creation is a separate boolean
permission and is restricted to the same Repository, a policy-matching Agent
branch, and the Repository's current default branch. The policy cannot grant
merge, deployment, AWS, arbitrary API, or token capabilities; credentials must
still be short-lived and Repository-scoped at execution time.

Example policy file:

```json
{
  "enabled": true,
  "branch_prefixes": ["hako/agent/"],
  "allow_pull_requests": true,
  "required_checks": ["hako/lint", "hako/test"],
  "require_checks_to_pass": true
}
```

This is configuration and authorization groundwork only. The API response
reports `execution_enabled: false`: Agent scheduling, model execution, and
GitHub write token issuance remain disabled until the isolated Agent runtime
and its Tenant credential and network/resource controls are deployed. Disabling
the Repository Factory opt-in clears this policy. See the
[Task 53 implementation record](../tasks/task-53-native-pipeline-agent.md).

Tenant Owners/Admins can separately configure whether a Factory-enabled
Repository may use the Tenant's approved Anthropic `native_agent` credential
in a future Native Agent runtime:

```bash
hako github repository agent-model-access set \
  --tenant tnt_example \
  --installation 12345 \
  --repository 67890 \
  --enabled true
```

The setting is default-off and requires both Factory opt-in and an active,
approved Tenant credential with purpose `native_agent`. Disabling Factory
opt-in clears the setting. This configuration does not enable Agent scheduling,
credential resolution, or Secret delivery; the API returns
`execution_enabled: false` until those runtime controls are implemented.

Tenant service metadata is tracked separately from built-in Workspace Service
templates. Owners/Admins can maintain a Software Catalog entry bound to a
Tenant-registered GitHub Repository, with service owner/team, language,
Lambda MicroVM runtime, dependencies, and an optional HTTPS production URL.
This metadata does not provision a Preview Workspace or authorize deployment;
see the [Tenant Software Catalog API and CLI](software-catalog.md).

## Suggested implementation sequence

1. GitHub App security baseline: installation flow, secret storage/rotation,
   minimum permissions, repository allowlisting, webhook HMAC validation,
   replay/idempotency tests, and audit events.
2. Repository registry: installation/repository schema and API, Tenant RBAC,
   repository removal/uninstall lifecycle, and encrypted credential
   references.
3. Durable webhook inbox and normalized, versioned Hako event delivery.
4. A narrow verification Job on the Hako fake runtime; persist Job/Run state,
   timeout/cancel behavior, logs, and idempotency before real AWS compute.
5. Checks API: create/update one `hako/test` check, then summary and
   annotations with permission/commit-scope tests.
6. Isolated AWS job runtime and ephemeral GitHub Runner Mode; execute one
   queued workflow job, forward logs, and destroy runtime on every terminal
   path.
7. Native pipeline schema and `.hako/factory.yaml` evaluation alongside
   GitHub Actions, not as a replacement.
8. Agent workload with issue-to-branch/PR flow, bounded write permissions,
   explicit trust policy, and reviewable diffs.
9. PR preview Workspace, Software Catalog, releases, artifact attestations,
   and tenant-scoped deployment/OIDC policies.

### First Software Factory milestone

The first end-to-end milestone is: install a test GitHub App on an allowlisted
repository → open a PR → receive/verify the webhook once → schedule one
isolated MicroVM job → run `go test ./...` → post the result as a GitHub Check
→ retain redacted logs → destroy the ephemeral runtime. The current repository
contains local implementations for GitHub App registration and Tenant
repository authorization, signed webhook intake and replay handling, durable
Workload scheduling, Lambda MicroVM controller adapters, Checks delivery, and
fake/PostgreSQL end-to-end coverage. The complete production milestone is not
yet available: the Worker and event mappings are default-off, the Lambda
MicroVM image/network/isolation path has not been verified in AWS, and live
GitHub/AWS credentials and deployment have not been exercised. The local E2E
is evidence for the composed contracts, not a production-readiness claim.

## Official references

- [Choosing permissions for a GitHub App](https://docs.github.com/ja/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app)
- [GitHub App webhooks](https://docs.github.com/en/apps/creating-github-apps/writing-code-for-a-github-app/building-a-github-app-that-responds-to-webhook-events)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
- [Self-hosted runner autoscaling and ephemeral runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)
- [REST API for Checks](https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks)
- [OIDC in cloud providers](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-cloud-providers)
- [nerdctl rootless mode and cgroup prerequisites](https://github.com/containerd/nerdctl/blob/main/docs/rootless.md)

## Native Pipeline Manifest (version 1)

### Version 1

```yaml
version: 1
on:
  push:
    branches:
      - main
      - "release/*"
  pull_request:
    branches:
      - main
pipelines:
  verify:
    steps:
      - checkout
      - go-test
      - go-vet
    services:
      - postgres
    runtime_class: standard
    timeout_seconds: 1800
    memory_mb: 2048
    cpu_millis: 1000
    network: registry-only
    repository: read
    cloud_access: none
```

Only catalog step names are accepted: `checkout`, `go-test`, `go-vet`,
`golangci-lint`, `npm-test`, `npm-build`, and `python-test`. `checkout` must be
first. Services are limited to `postgres` and `redis`. Arbitrary shell commands,
container images, mounts, write permissions, and cloud credentials are not
expressible in version 1; unknown fields fail parsing.

Defaults are `standard`, 1800 seconds, 2048 MiB, 1000 CPU millis,
`registry-only` networking, repository `read`, and no cloud access. Accepted
bounds are 30–3600 seconds, 256–8192 MiB, and 250–4000 CPU millis. Network is
either `none` or `registry-only`; public Internet access is not an option in
this schema. The workload image's host firewall currently permits Job and
rootless-containerd traffic only on loopback. Pinned image/source acquisition
must therefore happen in the trusted Resource Plane before Job execution; a
Job cannot fetch packages or images from a registry at runtime.

Declared Service Catalog sidecars are charged against the pipeline's declared
memory and CPU budget using each fixed catalog resource request. The plan is
rejected if sidecars exhaust either budget or use `network: none`. This is
admission-time validation against the manifest's own declared budget. The
separate Linux Job Service Engine now provisions only fixed `postgres` and
`redis` templates with attempt-scoped internal networks, ephemeral tmpfs,
catalog CPU/memory limits, loopback-only published ports, generated credentials,
health checks, and bounded cleanup. It is opt-in: the guest manifest resolver
admits `services` only when the Agent is explicitly composed with that Job
Service manager. `NewFilesystemAgent` fails construction if Service admission
is enabled but the configured process factory does not declare Job Service
support; `GoTestProcessFactory` declares support only when an attempt-scoped
provisioner is configured. The default resolver and default workload image
continue to reject/disable services. The local Podman acceptance verifies
loopback PostgreSQL access and denies an external TCP probe from the internal
sidecar network. It does not establish the pinned ARM64 nerdctl behavior,
Lambda MicroVM cgroup enforcement, target-image availability, or target egress
policy.
For a non-root Job command, the trusted Agent now requires a Resource Plane
configured cgroup v2 root with `cpu`, `memory`, and `pids` controllers enabled.
It creates a fresh cgroup, starts the command atomically inside it, applies
`memory.max`, `cpu.max`, and `pids.max`, and kills/removes the cgroup before
reporting terminal completion. Pipeline command limits are the declared
pipeline budget minus the fixed catalog sidecar reservations; each sidecar has
its own catalog limit, so the configured CPU and memory ceilings add up to the
pipeline budget. Missing controllers, failed writes, or incomplete cleanup
fail the command closed. The current host does not allow creating cgroups under
`/sys/fs/cgroup`, and Lambda MicroVM cgroup delegation/effective limits remain
unverified.

`make test-job-services-podman` runs the production service engine against a
local rootless Podman daemon through a test-only CLI adapter. It requires a
locally cached PostgreSQL image (`docker.io/library/postgres:18` by default;
override with `HAKO_JOB_SERVICES_PODMAN_IMAGE`). It checks host-side container
flags, lifecycle, and effective cgroup v2 memory/CPU/PID limits from inside the
container. This does not validate the pinned ARM64 nerdctl runtime or cgroup
enforcement in a Lambda MicroVM.

After a service is created, the Linux engine also inspects its runtime metadata
and fails closed unless the fixed catalog memory/CPU/PID limits, read-only
rootfs, non-privileged mode, absence of added capabilities and bind mounts,
`no-new-privileges`, `999:999` user, exact loopback port bindings, and tmpfs
paths/options were applied. Podman's known `rprivate` and `tmpcopyup` tmpfs
additions are allowed. The integration also reads `memory.max`, `cpu.max`, and
`pids.max` inside the running container, verifying that those limits reached
the host cgroup v2 runtime. This does not establish enforcement by nerdctl or
the target Lambda MicroVM kernel.

The validated configuration is converted to a deterministic execution plan
inside the Workload Agent, after it verifies the installed source archive
against the Run's commit-bound digest. `checkout` is handled as source staging,
not a process. Other catalog steps map to fixed absolute executable paths and
argument vectors (for example `go-test` maps to `/usr/local/go/bin/go test
./...`); manifest strings are never passed to a shell or appended to
arguments. Pipeline names are sorted before execution, and the fixed catalog
steps execute sequentially in the isolated Lambda MicroVM. A missing manifest
retains the legacy fixed Go-test path; an invalid manifest or an event matching
no pipeline fails the Run instead of silently falling back.

The current Agent runtime supports only `standard` with `registry-only`
network policy. Unless the opt-in Job Service manager is composed, it rejects
manifests declaring service containers. `network: none` and other runtime
classes are rejected. Memory and CPU values are bounded by the Resource Plane
profile and applied to the fixed non-root command through its cgroup v2 scope.
If the trusted cgroup root/controllers cannot be used or cleaned, execution
fails closed. This local implementation does not prove that the selected
Lambda MicroVM image exposes working cgroup delegation. Tool executables must
exist in the selected immutable MicroVM image; missing tools fail the Run.

Webhook-created Runs persist the normalized trigger kind and selection branch
alongside the immutable commit: push events store the branch after
`refs/heads/`, and pull requests store the base branch used for filtering. The
values are included in Run idempotency and returned only through the
authenticated Resource Plane specification endpoint. This is selection
context, not execution authority; Tenant/Repository/commit authorization is
still revalidated before a worker can receive the specification.
The specification response is schema version 2 when served by this Control
Plane. Resource Plane clients accept version 1 for old Runs that have no
trigger context, and version 2 for the extended contract.

At least one of `on.push` or `on.pull_request` is required. Branch lists are
optional (omission matches every branch) and accept only exact branch names or
a trailing `/*` prefix pattern. Push triggers match non-deleted `refs/heads/*`
events; pull-request triggers match opened, reopened, or synchronized open PRs
by their base branch. The webhook processor stores the trigger kind and
selection branch on the immutable Run; the Agent reconstructs the normalized
selection context from those fields after source verification, selects fixed-
catalog pipelines, and returns bounded per-pipeline terminal results. The
scheduler does not persist selected pipeline names before execution. Branch-
filter misses are discovered by the Agent and fail that Run.

The parser rejects manifests larger than 64 KiB, multiple YAML documents,
duplicate keys, aliases, anchors, explicit tags, nesting deeper than 20 levels,
more than 2048 YAML nodes, more than 16 pipelines, or more than 32 steps per
pipeline. Parser acceptance is not authorization or proof of safe execution:
the scheduler must still bind Tenant, Repository, commit, trust level, secrets,
network policy, and runtime limits before creating a Lambda MicroVM workload.

Validate a repository manifest locally with:

```sh
hako factory validate
hako factory validate --file path/to/factory.yaml
```

The command reads at most 64 KiB, prints only sorted pipeline names and the
manifest version, and does not contact the API or schedule a Workload.

### Coexistence and rollout

Existing GitHub Actions remain authoritative unless a Tenant explicitly opts
into a future Hako Native trigger. `workflow_job` deliveries remain isolated
from the generic Hako `go test ./...` trigger. No pipeline runs merely because
`.hako/factory.yaml` exists. Disablement must stop new Hako admissions without
changing GitHub workflow files or deleting historical run/check records.

Issue-to-Agent execution is not enabled by this parser. A Tenant Owner/Admin
can configure a dormant per-Repository Agent branch/PR write policy through
the API and CLI, but the response explicitly reports `execution_enabled: false`;
no write token is issued and no Agent Run is scheduled from that policy. Agent
runtime execution, model-credential authorization/handoff, and runtime
resource/network enforcement remain incomplete. When enabled in a future
deployment, Agent Runs must use an isolated Lambda MicroVM with no cloud
credentials by default and remain untrusted proposals requiring human review.
