# Task 53: Native Pipeline and Issue-to-Agent Workflow

Status: In Progress (strict versioned manifest parser and security tests implemented; Pipeline execution and Issue-to-Agent remain dependent on Tasks 39, 50, and 51). Priority: P3.

## Goal

Allow Hako-native verification and bounded coding-agent Jobs without silently replacing GitHub Actions or granting agents production authority.

## Work

- Define versioned `.hako/factory.yaml` with strict schema, allowlisted steps/images/services, bounded resources/time, explicit network policy, and secret scopes.
- Reject unknown fields, shell privilege escalation, unsafe mounts, mutable/unapproved images, and policy bypasses before scheduling.
- Add Issue/comment command to create an Agent Run in a separate ephemeral runtime; default permissions are repository read and no cloud secrets.
- Require an explicit policy for branch push/PR creation; use short-lived GitHub App installation token with narrow Contents/Pull Requests permissions and only after tests pass.
- Keep agent output labeled as untrusted proposal; human review remains required for merge/deploy.

## Acceptance criteria

- Parser/policy tests cover malicious YAML, aliases/anchors limits, shell injection, fork/untrusted network, secret exfiltration, oversized pipeline, and unsupported version.
- End-to-end fake tests prove Issue replay does not create duplicate Agent Runs/PRs and agent cannot deploy or merge.
- Every write action is scoped to the Tenant-bound Repository, branch policy, and short-lived token; tokens are absent from process output/artifacts.
- Document coexistence with GitHub Actions and migration/disable behavior.

## Local progress (2026-10-17)

- Added `internal/factoryconfig` version 1 parser with strict `push` and
  `pull_request` triggers, bounded branch filters, and a deterministic selector
  over normalized webhook events. The selector is not connected to scheduling.
- Added `internal/factoryconfig` version 1 parser with unknown-field rejection,
  duplicate-key detection, alias/anchor/tag rejection, single-document and
  structure-size limits, fixed catalog step/service allowlists, bounded runtime
  resources, repository-read-only access, and no cloud credential access.
- Added `hako factory validate [--file path]`, which validates the default
  `.hako/factory.yaml` or an explicit file locally; output contains only the
  version and sorted pipeline names.
- Added a deterministic `factoryconfig.Plan` which maps validated catalog
  steps to fixed executable/argument vectors, omits checkout as a process,
  and carries bounded runtime/network/service metadata without aliasing parser
  memory. It does not evaluate shell text.
- Persisted the normalized webhook trigger event and selection branch on the
  immutable Workload Run (push branch or pull-request base branch), included
  them in the idempotency fingerprint, and exposed them in authenticated
  Resource Plane execution metadata schema v2 for later fixed-commit manifest
  selection. The client retains compatibility with v1 metadata for older Runs.
- Added malicious YAML/resource-bound tests and documented the schema in
  [`docs/software-factory.md`](../docs/software-factory.md). The deterministic
  plan is not yet produced from staged source or connected to the Lambda
  MicroVM worker; webhook scheduling still uses the legacy fixed Go-test path,
  and no native manifest workload is created yet.
