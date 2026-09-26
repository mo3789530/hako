# AWS resource ownership tags

Every AWS resource created or managed for a Workspace should carry these tags
when the service supports them:

| Tag | Value | Purpose |
| --- | --- | --- |
| `hako:managed-by` | `hako` | Find Hako-owned resources without claiming untagged customer data. |
| `hako:tenant-id` | Stable Hako Tenant ID | Preserve the security/accounting scope. |
| `hako:workspace-id` | Stable Workspace ID | Reassociate compute/storage after recreation. |
| `hako:resource-plane-id` | Stable Resource Plane ID | Limit discovery and cleanup to the owning executor. |
| `hako:operation-id` | Operation that created or last changed it | Correlate partial work and audit events. |

`resourcecontroller.WorkspaceResourceTags` builds this exact map from a
validated command. `HasWorkspaceResourceTags` performs exact-scope checks for
tag-based discovery. These helpers are unit tested, but the production AWS
Runtime does not yet exist and therefore no real Workspace resource is tagged
or rediscovered today.

`hako:operation-id` is provenance, not a resource uniqueness key: a later
operation may update this tag. A resource binding in the Control Plane remains
the normal resource mapping; discovery is a recovery/reconciliation path when
the binding is missing or stale. Discovery must match all ownership/scope tags
and then inspect the service-specific resource identity before mutation. Never
delete a resource using only a partial tag match, and never treat an untagged
resource as Hako-owned.

Shared infrastructure has a narrower stable tag set. Current Control Plane
resources use `hako:managed-by=hako` and `hako:plane=control`; Resource Plane
resources use `hako:managed-by=hako` and
`hako:resource-plane-id=<id>`. Both Terraform modules accept additional tags
for environment and E2E run identity while reserving Hako ownership keys.
See [Terraform module documentation](terraform-modules.md).

The Resource Plane AWS queues currently use server-side SQS encryption and
CloudWatch log retention. Applying the Workspace tag set, recovering from
partial create, cleanup, and periodic orphan detection await the AWS Runtime
implementation and an account-scoped cleanup workflow.
