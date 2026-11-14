# Hako Coding Agent Platform

Status: design and local prototype. AgentCore execution and production
activation are not implemented. This document describes the initial coding
agent milestone, not a claim that a general agent platform is operational.
The implementation backlog and acceptance criteria are in [Tasks 90–96](../tasks/README.md).

## Goal and boundaries

Hako accepts an authorized coding task for a Tenant and Repository at an
immutable commit, runs an agent in an isolated attempt, and presents a change
proposal and verification evidence. Hako independently verifies the exact
proposal before its Broker may create a policy-approved branch or PR. Human
review remains required; this path does not merge or deploy.

The selected initial execution backend is **Amazon Bedrock AgentCore Runtime**;
HakoCodingAgent is implemented in Python. Hako retains its existing Workload
Run, authorization, artifact, verifier and Broker responsibilities. AgentCore
sessions execute untrusted repository work and do not gain GitHub write tokens.
An Agent's reported success or local test result is not publication evidence.

```text
authorized task + pinned commit
  -> Hako Run / attempt -> AgentCore session -> Python coding agent
  -> bounded change proposal -> independent verifier -> Hako Broker -> PR
                                     ^                  |
                            exact proposal digest    cleanup confirmed
```

All production execution is default-off until lifecycle, effective isolation,
credentials, egress and cleanup are verified on the target AWS environment.

## Current implementation

- Workload Run state, task identity, source checkout and proposal verification
  primitives exist in Hako; the Task 89 Broker publication route is locally
  implemented with fake-provider and database tests. Deployment and actual AWS
  and GitHub execution remain open. See [Workload Runs](software-factory-workloads.md)
  and [Task 89](../tasks/task-89-agent-proposal-publishing.md).
- The Python [coding-agent](../coding-agent/README.md) prototype has a bounded
  plan/code/verify/review workflow, an in-memory workspace, source TAR digest
  validation, a Bedrock Converse adapter and an `agent_changes` ZIP encoder.
  Tests use synthetic model/verifier adapters; source/ZIP format interoperability
  has been checked with Go. No live Bedrock request has been validated.
- AgentCore session identity and initial journal preparation primitives exist,
  but are not wired to dispatch, AWS invocation or cleanup. The Python Agent has
  no AgentCore entrypoint or production source/verifier/result adapters.
- Python, Java, Go, Rust, TypeScript and Terraform are detected as advisory model
  context. This does not provide their toolchains or prove tests ran. Protected
  `infra/` and `terraform/` paths remain unavailable to proposal generation;
  Terraform plan/apply is not implemented.
- The source loader holds supported source in memory. Protected paths are hidden
  from model tools, binary files cannot be read as text and executable files
  cannot be changed. Symlinks, oversized files, deletions and mode changes are
  not supported. These constraints exclude some normal repositories.

## Decisions still required

| Area | Decision or work needed | Task |
| --- | --- | --- |
| Runtime and workspace | Runtime tenancy, Lambda MicroVM support, workspace path/capacity, persistence, restart and cleanup evidence | [91](../tasks/task-91-agent-runtime-and-workspace.md) |
| Harness and model | Harness contract, optional Claude/Codex adapters, approved models/regions, budgets and tool controls | [92](../tasks/task-92-coding-agent-harness-model.md) |
| Source and proposal | Attempt-authorized delivery, supported source types, deletion/rename/mode representation and digest binding | [93](../tasks/task-93-agent-source-proposal-contract.md) |
| Language verification | Explicit toolchains, dependencies and isolated checks for each language and mixed repositories | [94](../tasks/task-94-coding-agent-verification-terraform.md) |
| Terraform | Scoped path authorization across all validation/publication boundaries; separate decisions for fmt, validate and credentialed plan | [94](../tasks/task-94-coding-agent-verification-terraform.md) |
| Isolation and credentials | Runtime IAM, brokered access, effective repository-process credential and egress isolation | [95](../tasks/task-95-agent-isolation-and-credentials.md) |
| Lifecycle and operations | Fenced invocation, cancellation, recovery, cleanup, progress, quotas, logs, deployment and teardown | [96](../tasks/task-96-agentcore-lifecycle-operations.md) |

The initial workspace recommendation is a per-attempt temporary filesystem,
deleted after execution and confirmed cleanup, with the pinned commit and Hako
artifacts as the durable sources of truth. This is a proposal: concrete paths,
storage, persistence and behavior for each backend are not yet selected.
AgentCore session stop alone must not be assumed to prevent a later invocation
from restarting that session; the Hako terminal fence and in-flight operations
must also be reconciled.

Additional harnesses are optional and not implemented. Any adapter must obey
the same task identity, tool authorization, cancellation, bounded output,
cleanup and independently verified publication contracts. AgentCore Runtime
and Lambda MicroVM are execution backends; harness and model selection are
separate choices. Lambda MicroVM support for this Python agent is undecided.

## Milestones

1. **Local contracts:** decide runtime/workspace, source and proposal contracts,
   verification profiles and credential boundaries; implement fake-provider
   lifecycle tests and explicit failure/cleanup behavior.
2. **One repository E2E:** in a disposable AWS environment, run one authorized
   Tenant/Repository task at a pinned commit through AgentCore, independently
   verify the exact proposal, create an approved PR through the Broker and
   confirm session and workspace cleanup. Record real AWS/GitHub evidence.
3. **Operational platform:** add policy administration, model/harness selection,
   usage and cost reporting, audit, progress and logs, quotas, alarms and
   recovery. Expand language/toolchain support and optional runtime/harness
   adapters only after their enforcement and lifecycle are tested.

Task 90 is complete only when its [acceptance criteria](../tasks/task-90-agentcore-coding-agent.md)
are met. A synthetic test or a locally generated ZIP is not an AWS E2E result.
