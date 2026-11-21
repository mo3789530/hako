# ADR 0011: Multi-Region Resource Plane Placement

- Status: Deferred (requirements recorded; single-Region Workspace placement)
- Date: 2026-10-24
- Owner: Hako Platform Architecture
- Scope: Future scheduling and recovery of Workspaces across AWS Regions.

## Context

The Control Plane can register multiple Resource Planes, and placement policy
already carries Region and capability constraints. A Workspace also depends on
regional Lambda MicroVM images, EFS, Gateway connectivity, encryption keys, and
data-residency policy. Moving compute alone does not move the Workspace's
durable source or service data.

## Decision

Keep each Workspace pinned to one Resource Plane and Region for its lifetime.
Placement may choose among healthy compatible Resource Planes at creation. A
later Region change is a migration operation with an explicit data-transfer,
integrity-check, cutover, and rollback plan; it is never an automatic retry
after a regional failure.

The Control Plane's global availability and database replication strategy are
separate from Workspace regional failover. No cross-Region RPO/RTO or automatic
failover guarantee is made until those contracts and provider behavior are
tested. Lambda MicroVM remains the runtime in every supported Region; EC2 is
out of scope.

## Requirements before implementation

- Define supported Regions per runtime class, image version, EFS, Gateway, and
  required AWS APIs.
- Define data residency, encryption-key ownership, regional health signals,
  capacity admission, and placement audit fields.
- Define migration of EFS source, Workspace-local state, generated Service
  credentials, and Gateway route/session revocation.
- Test stale command/result fencing during migration and loss of a Region.
- Publish bounded, measurable RPO/RTO only after recovery drills.

## Non-goals

- Transparent live migration of a running MicroVM.
- Cross-Region replication of Workspace local disks or in-memory state.
- Treating a second Region as a backup without verified data replication.
- Claiming multi-Region DSQL availability as proof of runtime failover.

## Revisit when

At least two Resource Plane Regions pass the Workspace image, EFS, Gateway,
security, and recovery gates, and a Tenant requirement establishes the target
RPO/RTO and data-residency policy. This ADR does not authorize AWS deployment.
