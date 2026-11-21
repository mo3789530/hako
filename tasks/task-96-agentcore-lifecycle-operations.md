# Task 96: AgentCore Lifecycle, Deployment and Operations

Status: Planned. Priority: P3. Parent: Task 90. Depends on Tasks 39, 89 and 91–95.

## Goal

Make the selected AgentCore implementation operable with durable, fenced
invocation, observable progress, bounded recovery and verified cleanup.

## Decisions and work

1. Define durable admission, attempt/session mapping, exclusive lease, progress,
   terminal states, cancellation, timeout, duplicate delivery and recovery.
   Specify ambiguous invoke/stop responses and ensure a stopped session cannot
   be accidentally restarted after the attempt is terminal. Reconcile in-flight
   invocation before recording cleanup confirmation.
2. Implement an AgentCore adapter and Python Runtime entrypoint using trusted
   source/task/result contracts. Authenticate and revalidate pins on every
   handoff; keep default-off until Tasks 94 and 95 gates are met.
3. Define durable event and artifact limits, retention, redaction, idempotency,
   alarms, model/token/compute budgets, rate limits and per-tenant quotas. Make
   progress and failures visible through existing Hako APIs without raw secrets.
4. Provide digest-pinned ARM64 image and default-off Terraform/IAM/network
   contracts. Document supported region, provisioning and teardown order,
   retained resources, ongoing costs and rollback.
5. Compose fake-provider integration tests from admission to cleanup, verifier
   and publication eligibility. Include retries, duplicate requests, lost
   responses, stale/cross-Tenant results, cancellation and cleanup failure.
   Provide a separate opt-in AWS smoke procedure and record evidence.

## Acceptance criteria

- No result authorizes publication until current-attempt identity, cleanup and
  independent digest-bound verification all pass.
- Local tests exercise successful and failure recovery with no live AWS/GitHub.
- AWS execution remains disabled until runtime, identity, effective isolation,
  egress, resource limits and teardown are confirmed in the target environment.
