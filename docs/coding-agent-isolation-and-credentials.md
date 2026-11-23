# Coding Agent Isolation and Credentials

Status: design baseline; effective AgentCore isolation is not verified.

This document records the trust boundaries for the initial AgentCore coding
agent route. Application checks and local fake-provider tests do not prove
that repository-controlled code is unable to obtain runtime credentials or
reach an unapproved network destination. Production AgentCore execution stays
disabled until the disposable AWS acceptance in Task 95 is recorded.

## Trust boundaries

```text
Control Plane journal and policy
  -> authenticated, attempt-pinned handoff
  -> AgentCore session (untrusted task and repository content)
       -> model adapter (Bedrock Converse)
       -> fixed file tools and bounded verifier request
  -> trusted source/proposal broker and independent verifier
  -> publication Broker (Task 89 only)
```

Repository files, issue/task text, model output and any process that executes
repository-controlled code are untrusted. The AgentCore session is a separate
trust domain from the Hako Control Plane and publication Broker. A session
must be bound to one Tenant, Repository, Run, attempt, generation, immutable
base commit and policy revision using journal-derived identity. The caller may
not supply or override these pins.

The Agent receives no GitHub write credential, AWS access key, Control Plane
database credential, S3 credential, or publication credential. Source reads,
proposal submission and state changes must use purpose-limited broker APIs
that revalidate the full current attempt identity on every request. Task 89's
publication Broker remains the only component allowed to create an approved
branch or PR, and only after independent verification and cleanup evidence.

## Controls and evidence

| Area | Required control | Current evidence / limitation |
| --- | --- | --- |
| Identity | Derive Tenant/Repository/Run/attempt/generation/base SHA/policy revision from durable Hako state; reject stale and cross-Tenant requests | Local journal and source/proposal contracts are documented; AgentCore dispatch is not wired |
| GitHub | No write token in the agent session or repository process; publication remains behind Task 89 Broker | Local publication route exists; effective deployed credential boundary is not demonstrated |
| AWS role | Dedicated execution role with only required AgentCore runtime, logging and approved Bedrock inference permissions; no broad data-plane access; review session IAM context conditions against the deployed runtime version | Deployment role and effective IAM simulation/positive-negative evidence are pending |
| Runtime credentials | Treat metadata credential access as possible from code in the session unless AWS-enforced isolation evidence proves otherwise; env filtering is insufficient | No AWS credential-probe evidence exists; production route remains off |
| Network | Explicitly enumerate Bedrock, broker, artifact and telemetry destinations; deny arbitrary Internet, metadata, private control-plane and cross-tenant paths; enforce at network boundary and test DNS/IP bypasses | No deployed egress policy or effective egress test exists |
| Filesystem/process | Do not expose runtime credentials or control-plane sockets in the workspace; bound source and proposal files; verifier runs in a separate trusted boundary with no agent credentials | Source/proposal byte and path limits are documented; AgentCore process/filesystem enforcement is unverified |
| Secrets and logs | Never log credentials or raw authorization headers; redact bounded model/tool output; use short-lived, purpose-scoped credentials only in trusted brokers | Local redaction and bounded output controls exist in related paths; deployed log redaction and retention are unverified |
| Budgets/revocation | Enforce tenant/run limits for invocation, tokens, wall time, memory, output and concurrency; revoke admission on policy change and fence late results | Harness limits cover turns and tool calls; effective runtime resource/cost ceilings and active-run revocation are pending |

## Required AWS acceptance

In an opt-in disposable account and explicit Region, record the deployed role
and resource policies and test both positive and negative controls. The tests
must attempt to obtain runtime credentials and access metadata, call AWS APIs
outside the minimum model/log permissions, reach arbitrary Internet and
private/control-plane addresses, bypass egress restrictions with alternate
DNS/IP forms, read another Tenant's source, and submit stale or foreign
attempt results. Positive controls must show the authorized model and exact
attempt broker operations still work. Capture policy versions, test identity,
timestamps and redacted outcomes; do not capture secret values. Confirm role
and session revocation during a running attempt, then verify all runtime and
temporary resources are removed.

Until that evidence exists, local fake-provider tests may establish request
validation and fail-closed behavior only. They must not be described as proof
of IAM, metadata, filesystem, network or MicroVM isolation. Any failed or
ambiguous isolation test keeps AgentCore production execution disabled.

## Revocation and operations

Each handoff must revalidate that the attempt is current and the Tenant,
Repository and policy remain enabled. Revocation prevents new handoffs and
publication immediately; in-flight provider operations must be reconciled,
the session stopped, and cleanup confirmed before terminal success. Results
arriving after cancellation, policy revocation, generation change or lease
expiry are stale and cannot authorize publication. Audit events contain
identities, policy revisions and bounded outcomes, never credentials or raw
secrets.

Task 96 owns durable lifecycle, quotas, recovery and teardown implementation.
Task 95 remains open until the effective AWS tests above pass.
