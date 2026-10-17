# Workspace storage Terraform module

This module creates one encrypted EFS filesystem for Workspace Access Points,
private mount targets in supplied subnets, and a dedicated mount-target
security group. Ingress is TCP/2049 only from explicitly listed Lambda
MicroVM connector security groups; the mount-target group has no outbound
rules. The filesystem policy requires TLS and IAM client authorization through
an Access Point for an exact operator-supplied list of runtime role ARNs.
It does not create Workspace Access Points; the Resource Controller's
idempotent EFS provisioner remains responsible for that lifecycle.

Daily AWS Backup recovery points are stored in a dedicated vault with a
configurable retention window (35 days by default). The EFS lifecycle policy
transitions cold files to IA after 30 days. Backup scheduling is UTC. Restore
must target a separately reviewed filesystem and preserve the Workspace
Access Point ownership/POSIX policy before cutover. EFS capacity and
throughput remain shared: Access Points are not quotas and this module does
not impose per-Workspace capacity limits.

This module is not wired into the dev Resource Plane root and creates no
resources unless a caller explicitly instantiates it. It intentionally does
not solve dynamic per-Workspace IAM role provisioning: a static list of shared
runtime role ARNs is not sufficient isolation for production tenants. Keep
Workspace execution disabled until each mount identity is scoped to its exact
Workspace/Access Point and the actual Lambda MicroVM VPC connector supports
the configured security-group path. Subnets must be private and in distinct
Availability Zones; this is a caller contract. Terraform does not verify
route-table Internet reachability.

Run `tofu test` in this module for mocked plan assertions. The tests do not
contact AWS or apply resources.
