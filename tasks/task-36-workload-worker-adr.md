# Task 36: Workload Worker Execution Substrate ADR

Status: Done (design only, 2026-10-04). Priority: P0. Depends on: none.

Result: [ADR 0006](../docs/adr/0006-workload-worker-substrate.md) selects disposable per-attempt EC2 + rootless Podman in the Resource Plane, short-lived SQS handoff, durable fencing/recovery, and a default-off staged validation path. Runtime/IAM deployment remains Tasks 39/40.

## Goal

Select and record a production-capable execution substrate for Workload Runs before wiring untrusted repository code to it. The API accepts up to 24-hour jobs; the current SQS scheduler queue and `internal/workloadjobs.SchedulerConsumer` only provide delivery mechanics. `internal/runtime/fake.JobRuntime` executes Podman locally and is explicitly not a production worker.

## Work

- Inspect the runtime contract, SQS visibility/DLQ, Go test Podman restrictions, artifact writer, Control/Resource Plane account boundary, and timeout/cancel semantics.
- Compare feasible options (for example ECS EC2 worker, AWS Batch, dedicated Resource Plane hosts, or another substrate). Explicitly assess nested-container/Podman support, tenant isolation, 24-hour execution, message visibility renewal, network restrictions, ephemeral disk, artifact upload, cold start, operational burden, and cost.
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
