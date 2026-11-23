# Task 91: Coding Agent Runtime Placement and Workspace

Status: Blocked on approved AgentCore provider verification. The topology ADR selects one shared Runtime with one session per immutable attempt, and records Resource Plane/journal ownership, bounded in-memory digest-verified source snapshots, session IAM context keys, disabled persistent storage, and the `ap-northeast-1` staging candidate. Durable scheduling now pins Agent attempts to `agentcore` and dispatches to an explicit AgentCore queue without Lambda fallback; deployment resources remain absent. Local cross-attempt mutation tests pass. Remaining acceptance requires deployed role/egress policy, model availability/quota, filesystem isolation, and provider cleanup evidence in an approved AWS environment. Priority: P3. Parent: Task 90. Depends on Tasks 39, 43 and 53.

## Goal

Select the AgentCore deployment topology and specify its per-attempt coding
workspace lifecycle. AgentCore Runtime is the managed Coding Agent target.
GitHub Actions runners and interactive VS Code Workspaces use Lambda MicroVM.

## Decisions and work

The initial topology decision is in [AgentCore Runtime Placement](../docs/agentcore-runtime-placement.md).

1. Compare one Runtime per repository, per tenant and shared deployment using
   actual AgentCore session isolation, execution-role scoping, account/region
   quotas, startup time, lifecycle ownership and fixed costs. Record an ADR
   selecting the initial topology, tenant/repository authorization boundary,
   region configuration and resource provisioning/deletion owner.
2. Route managed coding attempts explicitly to AgentCore. Surface an error
   when the provider is unavailable. Preserve the local Native Agent test
   path and test that the managed route does not fall back to Lambda.
3. Define per-attempt workspace path, filesystem type, CPU/memory/storage
   limits, inode limits, permissions, source/artifact locations, temporary
   credentials and behavior on full disk. Repository code must not access
   sibling attempts or runtime control sockets.
4. Decide persistence and resume semantics. The Task 90 baseline disables
   persistent session storage until deletion and isolation can be established.
   Specify cleanup evidence for success, failure, cancellation, timeout and
   uncertain provider responses; invocation completion alone is not cleanup.
5. Provide a provider-independent workspace lifecycle interface and focused
   fake tests if needed to make the chosen contract actionable. A workspace is
   not the source of truth: immutable source and verified artifacts are.

## Acceptance criteria

- ADR compares alternatives, states the decision and limitations, and maps
  each resource to an owner and cleanup path for the selected AgentCore topology.
- Tests cover cross-attempt isolation assumptions, quota exhaustion, restart,
  cleanup retry and unconfirmed cleanup; fake tests are labeled as such.
- Real AWS isolation, filesystem behavior and cleanup remain production gates
  until observed in an opt-in disposable-account smoke test.
