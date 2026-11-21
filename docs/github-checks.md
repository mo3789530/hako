# GitHub Checks API integration status

Task 41 is in progress. The local `internal/githubapp` library has a bounded
Checks API client and a purpose-specific Installation token method. Desired
Check identity, status/output, Check Run ID, and revisioned Outbox events are
persisted with Run state changes. The dispatcher routes those events to the
dedicated queue when configured. `internal/githubchecks.Consumer` reloads the
Run-pinned state, uses a credential provider, calls GitHub, persists the
returned Check ID, and acks only after DB success. The new broker mTLS endpoint,
credential provider, and `cmd/hako-checks-consumer` executable are locally wired
and covered by unit, mTLS, and PostgreSQL fake-GitHub delivery/replay tests. They
are not deployed. Keep the feature unavailable to production until the dedicated
queue, IAM, DLQ/redrive and alarm configuration are deployed and reviewed.

Migration `000066_workload_github_checks.sql` stores the exact Tenant, Run,
Installation, Repository, commit SHA, Check name/external ID, desired status,
conclusion, safe summary, revision, and nullable GitHub Check Run ID. Migration
`000067_workload_github_check_outbox_idx.sql` indexes pending delivery events.
Each Run create/state transition records the desired Check update and its
Outbox event atomically. `HAKO_GITHUB_CHECKS_QUEUE_URL` configures dispatcher
routing. All consumer failures leave the SQS message unacknowledged. The SQS
consumer sets a 900-second per-message visibility timeout by default, configurable
with `HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS` (1–43200 seconds). Its
10-message batch is processed sequentially; the default provides headroom over
the bounded broker and GitHub API requests. When GitHub returns a validated
retryable `Retry-After`/`RetryAt`, the consumer changes that message's SQS
visibility to the requested delay, bounded to 1 second–12 hours, and leaves it
unacknowledged. A visibility-change API failure also leaves it unacknowledged.
Transient failures without an upstream retry hint use the queue's normal
visibility/redrive behavior. Deployment still needs a reviewed queue
redrive/DLQ policy and retry/alarm runbook. AWS queue settings and IAM are not
provisioned by local code tests. The Control Plane Terraform module now has an
opt-in `enable_github_checks_delivery` switch (default `false`) that creates an
SSE-enabled standard queue and 14-day DLQ, configures five receives before
redrive, grants the Dispatcher only `sqs:SendMessage` to this queue, exports a
consumer policy limited to receive/delete/visibility/queue attributes on this
queue, and creates age/depth/DLQ CloudWatch alarms. The visibility timeout
defaults to 900 seconds and is configurable from 1 to 43200 seconds. The
consumer image is built locally as a static ARM64 non-root image using
`make build-github-checks-consumer-image`; publishing it is a separate
authorized release operation. The module creates an ECS task role for the
long-running Fargate consumer, attaches only the dedicated queue receive policy
and DSQL `DbConnect` policy, and exports `github_checks_consumer_task_role_arn`.
The SQL mapping and table grants are an explicit operator-run bootstrap step
using the template's `hako_checks_consumer` role. Optional
`github_checks_alarm_actions` SNS topics are wired to alarm and OK transitions.
When `enable_github_checks_consumer_service=true`, the module also creates the
Fargate task definition and service, private-subnet network configuration,
separate execution role, task log group, exact ECR image-pull permission, and
restricted Secret/KMS grants. This
requires an existing cluster, security groups/subnets, digest-pinned ARM64 ECR
image plus its exact matching repository ARN, private HTTPS broker URL, and
three explicit Secrets Manager ARNs. The
execution role fetches those Secrets at task launch; the consumer parses and
unsets the injected PEM environment values before processing. Secret values
are never read by Terraform. The service flag defaults off; security group
egress, VPC endpoints/NAT, broker reachability, Secret creation/rotation and
AWS apply remain operator-reviewed deployment work.

## Permission and token boundary

The GitHub App must be granted **Checks: write** to use this feature. Existing
checkout continues to require only **Contents: read** (plus GitHub's required
Metadata: read). The credential service mints a token for exactly one
repository and exactly one requested permission. Checkout and Checks tokens
have different cache identities; the Checks token is never returned by the
checkout method. Do not broaden the public API role or put either token in
DSQL, an Outbox/SQS message, a Workload environment, logs, or artifacts.

The trusted broker selects credentials only from the immutable Run identity.
Tenant-owned Runs resolve their exact integration/revision/generation and
approved key-reader binding. Legacy shared Runs are accepted only when all
Tenant-owned pin fields are absent and use the configured shared App. Missing,
partial, stale, or disabled Tenant-owned pins fail closed; they never fall back
to the shared App. Both modes still require the Run's Tenant/Installation/
Repository to be active in the registry.

Granting the App permission is an operator-controlled GitHub configuration
change and is not performed by local tests or Terraform validation. An
installation must be updated to approve the added permission. If Checks is
disabled or the permission is removed, preserve the Job result locally and
report the safe GitHub API failure; do not rerun the Job to retry result
delivery.

## Check identity and payload safety

Every request is tied to a trusted `RepositoryAccess`, an immutable commit SHA,
and a Hako Workload Run external ID. Branch names are not accepted as commit
identity. On a create retry, the client searches by Check name and SHA and
requires one exact external-ID match before updating it. A supplied Check ID
must return the same Check ID, name, external ID, SHA, status, and conclusion.

Summaries and API responses are size-bounded. Annotation paths must be
repository-relative and cannot contain traversal segments; line ranges,
severity, annotation count, and payload size are validated. Never send raw
logs, credentials, or unredacted secrets to GitHub. API error bodies are not
returned in errors. Rate limiting and temporary upstream failures are
classified as retryable with a bounded `Retry-After` hint; the consumer applies
that hint to the SQS message's visibility while leaving it unacknowledged.

For Native Workload pipelines, only failed fixed-Catalog `go-vet` and
`golangci-lint` steps produce source annotations. Their output capture is
limited to 128 KiB; truncated output is discarded for annotation purposes.
Diagnostics must identify a Go source path and line inside the checked-out
Repository, and messages are bounded and redacted with the Job Service secrets
known to the Worker. The producer enforces both the 50-annotation and 48 KiB
encoded-result bounds. `go-test`, other commands, and arbitrary output do not
produce annotations. GitHub still validates each annotation before persistence
and delivery.

## Rollout and rollback

1. Durable Check metadata/outbox delivery, result mapping, fake-GitHub retry
   recovery, and scoped PostgreSQL Outbox→Dispatcher→consumer→Check-ID E2E pass
   locally. Keep production delivery disabled until queue/IAM deployment review.
2. In an approved GitHub App change, grant Checks: write and update the
   relevant installation. Verify the credential service issues only the
   repository-scoped Checks token.
3. Enable result delivery for a controlled Tenant/Repository and verify Check
   identity, safe summaries, rate-limit retry and audit metadata.
4. To roll back, disable new Check delivery first. Continue recording Job
   results locally; drain or mark pending delivery records, then remove
   Checks: write from the App installation. Do not delete Jobs or rerun them
   merely because GitHub result delivery failed.

No live GitHub App permission changes, AWS apply, or real Secret retrieval are
part of local implementation or tests.
