# Workspace Runtime Operations

This runbook covers the local Resource Plane Workspace journal and the Lambda
MicroVM adapter. It is not an AWS deployment procedure. The controller binary
remains fake-only until image, network, IAM, and real lifecycle PoCs are
reviewed and separately approved.

## Retry and recovery

- A failed `RunMicrovm` call may have an unknown outcome. Redeliver the same
  Operation; the adapter reuses the immutable launch request and client token
  stored in the S3 journal. Do not change the request under the same token or
  manually create a replacement MicroVM.
- If journal `Create`/CAS returns a conflict, reload the journal and retry
  reconciliation from its current revision. Do not overwrite an ETag conflict.
- A delete writes a tombstone before termination. Retry the same delete until
  the provider reports `TERMINATED` and the journal reaches `deleted` with
  `cleanup_state=confirmed`. A failed cleanup is recorded as `state=failed`,
  `cleanup_state=failed` while retaining the tombstone; retry the same delete
  to move back to `deleting/pending`. Never remove or recreate a tombstoned
  journal key.
- If provider ID, image, image version, execution role, or network connectors
  do not match the journal, stop reconciliation and investigate. The adapter
  must not mutate or adopt the mismatched resource.
- A poll deadline does not mean the provider operation failed. Inspect the
  journal and provider state, then redeliver the original operation; do not
  issue a new ensure with changed configuration.

## Signals and operator checks

The S3 journal adapter currently emits errors to its caller; production alarm
resources and metrics are not deployed by this task. Before production enablement,
alert on repeated controller errors, S3 conditional-write conflicts/access
denials, aged `provisioning`/`deleting` journals, and provider API throttling or
5xx responses. Include Resource Plane ID, Workspace ID, operation ID, journal
revision, and allocation generation in structured logs; never log run-hook
payloads, endpoint auth tokens, or AWS credentials.

For an aged `provisioning` item, compare only its journal-pinned request/token
and its exact provider ID (if recorded). For an aged `deleting` item, preserve
the tombstone and retry idempotent termination. If the journal has no provider
ID after an ambiguous create, replay its exact launch request to recover the
provider result. Do not use `ListMicrovms` to infer ownership or adopt an
unbound resource: current API evidence does not expose per-MicroVM tags or a
Workspace ownership query.

## Limits and lifecycle boundaries

- Lambda MicroVM maximum lifetime is eight hours. Hako rejects a launch spec
  above that limit.
- Suspend/resume preserves state only for the same allocation where the
  provider supports it; a terminated allocation is not resumable. Workspace
  files and durable development services require the separate EFS/agent tasks.
- Runtime-class capacity, connector quotas, image readiness, container support,
  and AWS account/region quotas must be verified in the selected account before
  enabling production traffic. This local fake-provider suite does not establish
  those values or prove isolation.
- Never put AWS credentials or secret values in commands, the journal, or
  `/run` payload. Use Resource Plane IAM roles and secret references resolved by
  a trusted runtime component.

## Local verification

Run `GOCACHE=/tmp/hako-go-cache go test ./internal/workspacejournal ./internal/workspaceruntime -count=1`.
The SDK HTTP fake binds only to loopback and uses local test credentials; it
does not contact AWS. Real MicroVM launch, network isolation, IAM, container,
and cleanup remain separate AWS PoC gates.
