# Workspace storage Terraform module

This module creates one encrypted EFS filesystem for Workspace Access Points,
private mount targets in supplied subnets, and a dedicated mount-target
security group. Ingress is TCP/2049 only from explicitly listed Lambda
MicroVM connector security groups; the mount-target group has no outbound
rules. The filesystem policy requires TLS and IAM client authorization. Static
mode (`enable_dynamic_workspace_roles=false`, the default) uses an exact
one-to-one mapping of Workspace runtime role ARN to Access Point ARN; reusing
a role or Access Point is rejected. Dynamic mode allows only roles under
`/hako/workspaces/` and EFS Access Points in this account/Region, still over
TLS. Dynamic roles must carry Hako's generated identity policy, which grants
only one exact Access Point and explicitly denies root access, non-TLS, and
other Access Points. This explicit Deny is required because same-account
identity and resource policies contribute allows by union. Do not enable
dynamic mode without the paired role provider and IAM boundary.
It does not create Workspace Access Points; the Resource Controller's
idempotent EFS provisioner remains responsible for that lifecycle.

Daily AWS Backup recovery points are stored in a dedicated vault with a
configurable retention window (35 days by default). The EFS lifecycle policy
transitions cold files to IA after 30 days. Backup scheduling is UTC. Restore
must target a separately reviewed filesystem and preserve the Workspace
Access Point ownership/POSIX policy before cutover. EFS capacity and
throughput remain shared: Access Points are not quotas and this module does
not impose per-Workspace capacity limits.

The dev Resource Plane root wires this module behind the nullable
`workspace_storage` input; `null` (the default) creates no storage resources.
Static mode requires exact role/AP pairs; dynamic mode is explicit opt-in. The
Resource Controller now has a local IAM role provider and Lambda MicroVM
runtime wiring, but the runtime IAM permissions are not yet provisioned by
the Resource Plane deployment and the EFS policy has not been applied. Keep
Workspace execution disabled until the controller role's CreateRole/PutRolePolicy/
PassRole/DeleteRole scopes, Lambda MicroVM connector path, and live mount
behavior are validated. Subnets must be private and in
distinct Availability Zones; this is a caller contract. Terraform does not
verify route-table Internet reachability.

Run `tofu test` in this module for mocked plan assertions. The tests do not
contact AWS or apply resources.
