# ADR 0008: Software Factory Workload runtime (non-EC2)

- Status: Accepted for the initial native Job profile; deployment and AWS PoC pending
- Date: 2026-10-10
- Scope: Software Factory `job` Runs only. Workspace Runtime selection remains Task 43.

## Decision

Use one Amazon ECS task on AWS Fargate per Workload Run attempt for the initial
native Job profile. Hako does not create, configure, or operate EC2 instances,
EC2-backed worker fleets, AMIs, EBS worker disks, or EC2 lifecycle APIs. Fargate
is a managed non-EC2 execution API from Hako's perspective; Hako owns ECS task
definitions, `RunTask`/`StopTask`, networking, IAM boundaries, and cleanup.

The first supported profile is an OCI image that runs a bounded command directly
(initially the existing fixed-commit `go test ./...` path). It does not run
Docker-in-Docker, a Docker daemon, Podman, privileged containers, or arbitrary
nested containers. Build workloads that require those capabilities remain
unsupported until a separate non-EC2 build design is accepted. Local Podman is
still useful for developer tests, but is not the production isolation boundary.

## Why

- Native Runs have deadlines up to 24 hours. Standard Lambda invocations are
  limited to 900 seconds, so they cannot host the whole Run. Lambda remains
  appropriate for short-lived API, dispatcher, and controller functions.
- Fargate supports long-running tasks and provides task-level isolated,
  hardware-virtualized infrastructure. Its Linux task ephemeral storage starts
  at 20 GiB and can be configured up to 200 GiB.
- Fargate does not support privileged containers, host devices, or the full set
  of Linux capabilities. Therefore the initial profile deliberately does not
  promise nested Podman/Docker.
- Hako's Lambda MicroVM direction describes Workspace compute and is not an
  existing durable 24-hour native-Job execution API in this repository. Do not
  equate an ordinary Lambda invocation with a persistent Workspace.

## Runtime contract and ownership

The substrate adapter exposes `EnsureRunning`, `Cancel`, `GetStatus`,
`CollectResult`, and `Cleanup`, keyed by immutable Run/attempt/generation and
Resource Plane placement. `EnsureRunning` must be idempotent for that identity;
`Cancel` is durable and may race launch; stale attempts cannot publish results;
Run terminal state and physical cleanup confirmation are separate states.

- Control Plane remains the System of Record and owns DSQL state, dispatch,
  result fencing, deadlines, authorization, and user-visible status. It receives
  no ECS lifecycle permissions.
- Resource Plane owns the command/result queues, Fargate task launch/stop,
  attempt-scoped artifact flow, and cleanup reconciliation. It never connects
  to DSQL and never receives GitHub App private keys.
- The untrusted command runs without a task role, DSQL access, ambient GitHub
  credential, or broad AWS credentials, and with default-deny network policy.
  The ECS task execution role is for ECS image/log operations and is not exposed
  to the workload. A trusted service stages the fixed-SHA source before launch.
  Any artifact handoff must be attempt-scoped, short-lived, and unable to access
  other Tenants' objects. Network access required for a Job must be explicitly
  allowlisted by policy; arbitrary Internet access is off by default.
- A Run's task and ephemeral storage are disposable. Workspace persistence and
  durable database services are not provided by this Job runtime. Task cleanup
  must stop the task and verify terminal ECS state; orphan detection is required.
- Queue visibility covers only short dispatch/consumer work, never the full
  Run. Run deadline and task lifecycle are tracked independently of SQS receipt
  lifetime.

## Alternatives and limits

| Candidate | Decision |
|---|---|
| Standard Lambda | Rejected for the full Job: invocation timeout is at most 15 minutes, below the 24-hour Run contract. Keep for short control functions. |
| Hako Lambda MicroVM | Not selected for native Jobs: this repo does not yet expose a durable, cancellable, per-attempt 24-hour Workload API on that runtime. Continue evaluating separately for Workspace Runtime (Task 43). |
| ECS/Fargate | Selected for the initial direct-command Job profile. Long-running task support and task isolation fit; privileged/nested-container workloads are explicitly unsupported. |
| EC2 / EC2-backed worker | Out of scope by user direction; must not be introduced as a fallback. |

This decision does not claim that all arbitrary untrusted code is safe. Kernel,
runtime, and control-plane vulnerabilities remain risks. Before enabling Run
creation, Task 40 must validate IAM and network isolation, launch/cancel races,
deadline handling, artifact scoping, stop/cleanup recovery, task quota and cost,
and alarm coverage. Run creation remains default-off until those checks pass.

## Follow-up

Task 40 owns Fargate task/controller deployment and a measured PoC. It must
confirm supported CPU/memory/storage combinations and quotas in the target
Region, cold-start/launch latency, a 24-hour fake-clock lifecycle test, actual
stop/cleanup behavior, artifact transfer, and an isolated smoke Run. No AWS
resources are created or changed by this ADR. Revisit this ADR if nested
container execution becomes a hard requirement; do not silently enable
privileged containers or fall back to EC2.

## References

- [Lambda function timeout](https://docs.aws.amazon.com/lambda/latest/dg/configuration-timeout.html)
- [AWS decision guide: Lambda or Fargate](https://docs.aws.amazon.com/decision-guides/latest/decision-guides/choosing-aws-serverless-service.html)
- [Fargate task isolation](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/security-shared-model.html)
- [Fargate task definition limitations](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-tasks-services.html)
- [Fargate ephemeral storage](https://docs.aws.amazon.com/AmazonECS/latest/developerguide/fargate-task-storage.html)
