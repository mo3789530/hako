# Workload Controller deployment and operations

This runbook covers the opt-in Resource Plane Workload Controller for Lambda
MicroVM Jobs. It does not authorize an AWS apply. Keep Run creation disabled
until the target account has passed the image, network, cleanup, and isolation
checks in [Lambda MicroVM Workloads](lambda-microvm-workloads.md) and Task 61.
EC2 and Fargate are not Workload execution runtimes.

## Deployment order

1. **Review the Resource Plane boundary.** Confirm the target account, Region,
   Plane ID, private subnets, dedicated security group, command/result queues,
   and dispatcher/result-consumer role ARNs. Verify subnet routes and DNS for
   the Lambda MicroVM API, SQS, S3, Secrets Manager, KMS, and the configured
   Control Plane and credential-broker HTTPS endpoints. Worker Lambda VPC
   connectivity and MicroVM egress connectors are separate network paths.
2. **Complete runtime acceptance before enabling the Worker.** Build and
   inspect the pinned ARM64 MicroVM image; verify its `/run` hook and Agent
   listener in the Lambda MicroVM image builder and target runtime. Verify
   default-deny egress, required endpoint access, cancellation, timeout,
   idempotent launch/termination, confirmed cleanup, and orphan recovery. A
   Terraform plan, SDK stub, or local x86 Podman test does not satisfy these
   checks.
3. **Prepare storage and secrets separately.** Review/import any existing
   artifact bucket deliberately; the opt-in artifact-storage contract does
   not adopt a bucket implicitly. Check versioning, encryption, lifecycle,
   prefix ownership, and KMS key policy. Provision the six named Worker
   Secrets, broker mTLS identity/allowlist, and Resource Plane key material
   through the approved secret process. Pass only Secret ARNs to Terraform;
   never put secret values in tfvars, logs, or plan output.
4. **Validate disabled and enabled plans.** Run
   `tofu -chdir=infra/terraform/modules/resource-plane init -backend=false`
   and `tofu -chdir=infra/terraform/modules/resource-plane test`. Review the
   selected Resource Plane inputs, exact queue and artifact prefixes, VPC
   attachment, six distinct Secret ARNs, MicroVM image/network profile, IAM
   actions, schedules, alarms, and SQS visibility/timeout relationship. Plan
   using the account-specific root and backend only after that review. Never
   use the development defaults as production account configuration.
5. **Apply infrastructure with workload admission still disabled.** Create
   queues, DLQs, role/policies, logs, and the Worker deployment only after the
   authorized change review. Keep the Worker deployment opt-in separate from
   `enable_workload_run_creation`. Verify alarm delivery and queue policies
   from both accounts. Do not retrieve real Secret values as part of a local
   test or ordinary plan review.
6. **Run a bounded smoke test before gradual admission.** Use a dedicated test
   Repository and disposable Workload. Verify the Run's exact commit and
   attempt, selected Plane/profile, Agent ticket scope, result and log prefix,
   Check delivery, cancellation, MicroVM termination confirmation, and absence
   of residual allocations. Enable admission only after the evidence is
   reviewed; increase concurrency gradually while watching queue age, DLQ,
   Lambda errors/throttles, MicroVM API throttles, capacity, and cleanup age.

## Disable and rollback

To stop new execution, disable Workload Run admission first. Keep the result
consumer, command/result queues, Worker role, and cleanup/recovery schedules
available while accepted work drains. Inspect active attempts and durable
journals; cancel through Hako so the normal tombstone, termination, and result
path runs. Do not remove the queues or revoke Worker permissions while an
allocation may still exist.

After the queues are drained and every allocation has a confirmed terminal
provider state, disable the command event source and scheduled recovery/artifact
cleanup invocations in a reviewed change. Preserve the journal, result queue,
DLQs, audit records, and artifact bucket during the rollback window. Roll the
MicroVM image back by selecting the previously reviewed immutable image
version/digest; do not mutate a published image in place. Restore the previous
Worker ZIP/configuration only if it understands the current journal and queue
schemas. Otherwise leave the Worker disabled and use the forward recovery
procedure.

Do not delete or recreate a MicroVM to clear a stuck operation. A terminal
Check or failed Run is not proof that physical cleanup completed. Resolve any
pending cleanup and confirm the provider reports termination before releasing
capacity or removing runtime permissions.

## Queue and DLQ handling

The command and result queues are at-least-once transports. Inspect the
message's bounded identity fields, durable Run/attempt state, Resource Plane,
MicroVM binding, journal checkpoints, and last safe failure category before
redrive. A failure can occur after a provider side effect but before the
message is acknowledged. Retry only after confirming the operation is
idempotent for that exact identity or after the partial effect is compensated.

Never bulk-redrive a Workload DLQ. Keep the message in the DLQ when identity,
schema, journal, provider state, or side effects are uncertain. Do not edit a
message to point it at a new attempt; submit a new authorized Run through the
Control Plane. Record the incident, inspected state, action, and resulting
provider IDs in the operator audit trail. See [Resource Plane queues and
DLQs](resource-plane-queues-and-dlq.md) for queue boundaries and generic DLQ
handling.

## Artifact retention

Artifacts are scoped to the immutable Tenant/Run/attempt prefix. The MicroVM
has no S3 credentials; the Resource Plane Controller writes only validated,
redacted logs and source/build artifacts through its role. Confirm the bucket's
versioning, encryption, lifecycle, public-access block, TLS-only policy, and
exact prefix permissions before admission. Retention cleanup is a separate
default-off schedule: verify the retention period and deletion alarms, and
confirm an expired object's exact attempt prefix before allowing deletion.
Do not shorten retention or remove a bucket/key during incident response; keep
artifacts needed to diagnose unresolved or disputed Runs.

## Local checks

These checks do not call AWS or retrieve real secrets:

```sh
make test-workload-runtime
make test-workload-runtime-integration
make test-software-factory-e2e
tofu -chdir=infra/terraform/modules/resource-plane test
```

The integration targets use the already-running local PostgreSQL. The
Software Factory E2E uses Podman. They validate local contracts and fake
providers, not deployed IAM, networking, Lambda MicroVM isolation, or the
operator's account-specific runbook steps.
