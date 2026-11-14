# Task 91: Coding Agent Runtime Placement and Workspace

Status: Planned. Priority: P3. Parent: Task 90. Depends on Tasks 39, 43 and 53.

## Goal

Decide the AgentCore deployment topology and the role of Lambda MicroVM, and
specify the coding workspace lifecycle for each supported execution backend.
AgentCore Runtime is the required initial backend; do not replace or route
Workspace/Job workloads through it as part of this task.

## Decisions and work

1. Compare one Runtime per repository, per tenant and shared deployment using
   actual AgentCore session isolation, execution-role scoping, account/region
   quotas, startup time, lifecycle ownership and fixed costs. Record an ADR
   selecting the initial topology, tenant/repository authorization boundary,
   region configuration and resource provisioning/deletion owner.
2. Decide whether Python HakoCodingAgent also supports the existing Lambda
   MicroVM path in the initial release or a follow-up. Document backend
   selection and compatibility without claiming current Python integration.
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
  each resource to an owner and cleanup path for both selected backends.
- Tests cover cross-attempt isolation assumptions, quota exhaustion, restart,
  cleanup retry and unconfirmed cleanup; fake tests are labeled as such.
- Real AWS isolation, filesystem behavior and cleanup remain production gates
  until observed in an opt-in disposable-account smoke test.
