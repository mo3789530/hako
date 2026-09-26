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

This is an extension roadmap, not a claim that GitHub integration, the AWS
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
Gateway route only when enabled. No GitHub App is registered automatically;
normalized events are not yet dispatched, and installation/repository
ownership checks must be added before creating work. Tenant owners/admins can
currently submit and list inert installation claims through the tenant API;
claims remain `pending` and cannot authorize GitHub API access. See
`internal/githubwebhook` and `internal/store/githubwebhook` tests.

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
| Clone source for verification | Contents: read |
| Report CI results | Checks: write |
| Receive workflow job lifecycle | Actions: read, subject to exact webhook/API requirements |
| Issue-driven agent | Issues and Pull requests: read first; write only for an enabled action |
| Push branch/open PR | Contents and Pull requests: write, isolated to an explicit Agent workload |
| Register/delete self-hosted runner | Separate runner-manager App/credential; repository Administration or organization runner permission as required by GitHub |

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
create a new GitHub App per Tenant for the standard flow. A later BYO-App mode
can store each tenant's `app_id`, private-key Secret ARN, and webhook-secret
reference separately; webhook ingress would then resolve an opaque
integration ID before selecting that App's secret. Never select a tenant based
only on an unverified request header or repository name.

`POST` and `GET /v1/tenants/{tenant_id}/github/installations` are owner/admin
routes for recording and viewing pending installation claims. A request is
not proof of installation ownership: it does not create the global active
binding and cannot be used to mint an installation token. Only a verified
GitHub App setup callback should transition a claim to active. The active
binding table enforces that one Installation ID cannot be owned by multiple
Hako Tenants; repository rows reference that active binding.

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
enabled. Add issues/comments, check-run requests, release, and workflow-run
events later. Webhook receipt and GitHub API mutation are separate operations:
acknowledge accepted deliveries promptly, then process from a durable queue or
outbox with retry/idempotency.

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

Use Checks API check runs for `hako/test`, `hako/lint`, `hako/build`, and later
`hako/ai-review`. Create a check early as queued/in-progress, then update it
with conclusion, summary, external run link, and line annotations when
available. Check annotations are line-scoped, so retain source path/line
mapping through log/result collection. Tie every run to the exact repository
ID and commit SHA; never report a result on a mutable branch name alone.

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

Treat `.hako/factory.yaml`, workflow files, action references, shell steps,
container images, cache contents, and webhook payloads as untrusted inputs.
The Native pipeline schema needs a version, strict parser, allowlisted step
types, runtime/time limits, maximum artifact sizes, and explicit network and
secret policies. Shell execution is a capability granted by policy, not a
trusted declarative value. Pin or policy-check third-party actions/images
before production use.

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
does not yet have the production runtime, GitHub App, webhook service, or this
E2E, so this remains a future milestone rather than a supported workflow.

## Official references

- [Choosing permissions for a GitHub App](https://docs.github.com/ja/apps/creating-github-apps/registering-a-github-app/choosing-permissions-for-a-github-app)
- [GitHub App webhooks](https://docs.github.com/en/apps/creating-github-apps/writing-code-for-a-github-app/building-a-github-app-that-responds-to-webhook-events)
- [Validating webhook deliveries](https://docs.github.com/en/webhooks/using-webhooks/validating-webhook-deliveries)
- [Self-hosted runner autoscaling and ephemeral runners](https://docs.github.com/en/actions/reference/runners/self-hosted-runners)
- [REST API for Checks](https://docs.github.com/en/rest/guides/using-the-rest-api-to-interact-with-checks)
- [OIDC in cloud providers](https://docs.github.com/en/actions/how-tos/secure-your-work/security-harden-deployments/oidc-in-cloud-providers)
