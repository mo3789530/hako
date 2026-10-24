# ADR 0010: Customer-Owned Resource Planes (BYOC)

- Status: Deferred (requirements recorded; no customer account connected)
- Date: 2026-10-24
- Owner: Hako Platform Architecture
- Scope: Future execution of a Hako Resource Plane in a customer AWS account.

## Context

Some organizations require Workspace compute, EFS data, network controls, and
logs to remain in an AWS account they own. The current product model separates
the Control Plane account from one or more Hako-operated Resource Plane
accounts. A future BYOC option must preserve that boundary without distributing
long-lived AWS credentials or making the Control Plane a general AWS operator.

## Decision

Treat BYOC as a separately provisioned Resource Plane registered with the
Control Plane. The customer account owns its Lambda MicroVMs, EFS, Gateway,
queues, journals, encryption keys, and supporting network resources. Commands
remain scoped to an immutable Resource Plane identity and are executed by
roles in that account. The Control Plane stores only registration metadata and
health/capability state; it does not store customer AWS access keys.

Any future cross-account trust must be explicit, customer-approved,
resource-scoped, revocable, and independently auditable. Prefer customer-side
deployment and resource-side role execution over Control Plane
`AssumeRole`. BYOC must use the Lambda MicroVM runtime contract; EC2 is out of
scope.

## Requirements before implementation

- Define customer provisioning, upgrade, rotation, disable, and account-removal
  workflows, including rollback and orphan discovery.
- Specify trust policies, external IDs or equivalent workload identity,
  organization policies, KMS ownership, and the exact resource API surface.
- Prove Control Plane command authentication, replay resistance, and audit
  behavior when the Resource Plane is customer-operated.
- Define support boundaries for customer-managed quota, billing, networking,
  and incident response.

## Non-goals

- Accepting AWS access keys or IAM user credentials.
- Running EC2, EKS, or arbitrary customer compute as a substitute runtime.
- Automatically taking ownership of existing customer resources.
- Enabling BYOC through a CLI flag before provisioning and support gates exist.

## Revisit when

A customer deployment requires account ownership and the Control Plane,
Resource Plane, security, and operations owners have approved the trust and
support contracts. This ADR does not authorize cross-account IAM changes or
AWS deployment.
