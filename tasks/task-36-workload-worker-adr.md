# Task 36: Workload Worker Execution Substrate ADR

Status: Superseded (EC2 execution is out of scope; runtime selection reopened 2026-10-10). Priority: P0. Depends on: none.

Result: The historical [ADR 0006](../docs/adr/0006-workload-worker-substrate.md) selected disposable EC2, but that choice is superseded. EC2 provisioning, execution, and lifecycle permissions must not be implemented. Substrate-neutral dispatch/result fencing remains in Task 39; Task 61 selects an in-scope non-EC2 Workload runtime before runtime-specific worker work resumes.

## Goal

Select and record a production-capable execution substrate for Workload Runs before wiring untrusted repository code to it. The API accepts up to 24-hour jobs; the current SQS scheduler queue and `internal/workloadjobs.SchedulerConsumer` only provide delivery mechanics. `internal/runtime/fake.JobRuntime` executes Podman locally and is explicitly not a production worker.

## Work

- Inspect the runtime contract, SQS visibility/DLQ, Go test Podman restrictions, artifact writer, Control/Resource Plane account boundary, and timeout/cancel semantics.
- Historical comparison is retained in ADR 0006. Runtime selection is reopened by Task 61 with EC2 explicitly excluded; assess Lambda MicroVM compatibility, execution duration, nested-container needs, Tenant isolation, message visibility, network restrictions, storage, artifact upload, cold start, operational burden, and cost.
- Decide where the Workload worker belongs relative to Control Plane and Resource Plane accounts. Do not silently broaden Control Plane permissions to provision compute.
- Record runner image lifecycle, per-Run isolation boundary, secrets delivery, network egress, cancellation, worker crash recovery, and how durable fencing prevents duplicate execution.
- Keep API creation default-off until the selected design has a working worker path.

## Acceptance criteria

- Add an ADR under `docs/adr/` with decision, alternatives, consequences, security model, and deferred risks.
- Identify exact AWS services/resource ownership and local development equivalent.
- Specify SQS visibility/heartbeat and timeout behavior for 1 second through 24 hours; identify what must happen after worker crash or visibility renewal failure.
- Include a staged validation plan requiring no AWS apply as part of this task.
- No runtime or IAM implementation is claimed complete by this design task.

## Tests / docs

Review existing workload and queue tests; add no speculative runtime code. Link the ADR from this task and `docs/software-factory-workloads.md`.
