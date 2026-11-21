# Workspace Service generated credentials

PostgreSQL is the only built-in Workspace Service that currently declares a
generated credential (`POSTGRES_PASSWORD`). The Resource Plane Service
controller creates an initial random 256-bit value in AWS Secrets Manager under
`hako/workspaces/{tenant}/{workspace}/services/{service}/{variable}`. Resource
tags bind the record to Hako, Tenant, Workspace, Service, and variable name.
Retries reuse the existing `AWSCURRENT` value only after checking its exact
name and the complete expected ownership-tag set. During an explicit rotation,
the exact generation-specific `AWSPENDING` version is delivered until the Guest
has applied it and the controller promotes it to `AWSCURRENT`. A name collision
with different tags fails closed.

The value is not part of PostgreSQL state, the Outbox command, SQS message,
Guest capability, runtime report, or public API response. The controller
passes it only in the authenticated HTTPS request to the exact currently bound
Lambda MicroVM Guest. The Guest validates the variable against the immutable
catalog and gives it to `nerdctl` using a mode-0600, short-lived env file in a
private `/run/user/{uid}` tmpfs directory. The root init supervisor mounts this
directory as a size-limited `nosuid,nodev,noexec` tmpfs owned by the Workspace's
mapped UID/GID before starting the Agent; if it cannot verify that mount,
container startup fails closed. The file is removed immediately
after synchronous container creation; the value is not put in the command
arguments, process environment, Workspace/EFS volume, logs, or artifacts.
Container runtime output is discarded.

Secrets Manager access is in the opt-in Workspace Service controller IAM only.
Creation is limited to the Hako Workspace Service secret-name namespace and
requires the exact Hako tag-key set, with a nonempty Tenant/Workspace/Service/
variable tag and `hako:managed-by=hako`. Describe/Get are limited to that
namespace and Hako-managed resources. Rotation can add versions only to tagged
Secrets in that namespace and can move only `AWSCURRENT` or `AWSPENDING` labels.
The Service controller queue/Lambda and IAM policy remain default-off; no AWS
resources are created by local tests.

Credential rotation is explicit: `hako service rotate-credentials` requires
the Service to be desired-running and increments both its desired revision and
credential generation in the same transaction as the audit and Outbox event.
The Resource Plane writes a deterministic, generation-specific
`AWSPENDING` Secrets Manager version. The Guest changes PostgreSQL's `postgres`
role using fixed `psql` arguments and SQL supplied on stdin; the password is
not placed in process arguments or environment. Only after a healthy Guest
result and a fresh desired-state plus MicroVM-generation check does the
controller move `AWSCURRENT` to that version and remove `AWSPENDING`. Retries
reuse the same pending version and reapply the same password, so a lost response
between the DB change and stage promotion converges safely. Service restart
also reapplies the current credential to the existing PostgreSQL data volume.
The CLI/API never return credential values. This implementation is locally
fake-tested; real Secret Manager calls and a real PostgreSQL MicroVM rotation
have not been performed. The default-off controller IAM now permits version
write and stage movement only under the Hako Workspace Service secret path and
managed-resource tag. After the current, generation-bound
Guest confirms Service removal, the Resource Plane verifies the exact Hako
ownership tags and schedules each generated secret for deletion with a
seven-day Secrets Manager recovery window. Missing secrets and a replay of an
already scheduled deletion are idempotent; an untagged/foreign secret or a
failed Guest removal is never deleted. The Service deletion report is not
published until retirement scheduling succeeds. The opt-in controller IAM
adds `secretsmanager:DeleteSecret` only for the Hako Workspace Service path and
the `hako:managed-by=hako` resource tag. No test retrieves or deletes a real
secret. The Workspace image now has a default-off
`HAKO_WORKSPACE_SERVICES_ENABLED=true` build setting. When enabled, root init
configures the per-MicroVM identity/subordinate-ID mapping, supervises
rootless containerd as the Workspace owner, and gives the Agent only its fixed
local socket path. The opt-in lifecycle is implemented and locally tested, but
a real MicroVM image, namespace, network, and resource-limit PoC is still
required before enabling that setting in a deployed image; real PostgreSQL
execution has not been demonstrated.

Tests use an injected fake Secrets Manager API only. They cover first creation,
stable retry, Tenant scoping, exact ownership tags, stop without secret access,
redacted errors, Resource Plane-to-Guest delivery, Guest validation, and
ephemeral env-file permissions/removal. No real Secret is retrieved or
provisioned by tests.
