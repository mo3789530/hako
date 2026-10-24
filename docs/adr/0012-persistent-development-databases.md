# ADR 0012: Persistent Development Databases

- Status: Deferred (requirements recorded; Workspace-local services remain default)
- Date: 2026-10-24
- Owner: Hako Workspace Platform
- Scope: Optional managed database resources that outlive a Workspace MicroVM.

## Context

The Service Catalog can run development PostgreSQL and Redis inside a
Workspace MicroVM. Workspace source is stored on EFS, while local databases
and container volumes are disposable with the MicroVM unless explicitly
persisted. Some projects need a database to survive Workspace suspension,
replacement, or deletion.

## Decision

Keep `hako add postgres` and `hako add redis` Workspace-local by default.
Persistent databases are a separate, explicit resource with their own Tenant
quota, lifecycle, credentials, network policy, backup, restore, and deletion
contracts. A Service Catalog `persistent` volume declaration does not create
or imply RDS/Aurora durability.

The initial candidate is a managed RDS/Aurora resource per explicit request;
the exact engine, topology, and supported operations remain undecided. Hako
must never silently promote a local development database to managed storage.

## Requirements before implementation

- Define supported engines/versions, sizing bounds, availability options, and
  Tenant quota and cost visibility.
- Define private connectivity from Lambda MicroVM Workspaces without public
  database ports, including rotation and revocation of database credentials.
- Specify backup retention, point-in-time restore, deletion protection,
  teardown, and data export behavior.
- Define ownership tags, encryption keys, IAM, audit, and data deletion
  evidence for the Resource Plane.
- Add explicit API/CLI create, inspect, rotate, backup/restore, and delete
  workflows with idempotency and failure recovery.

## Non-goals

- Making a Workspace-local container persistent by moving its data to EFS.
- Exposing RDS/Aurora endpoints publicly.
- Creating managed databases as a side effect of `hako add`.
- Promising production database availability from development Service health.

## Revisit when

Workspace runtime, private networking, Tenant quotas, secret rotation, and
backup/restore operations have passed their implementation gates, and product
requirements specify the supported engine and recovery objectives. This ADR
does not authorize database provisioning.
