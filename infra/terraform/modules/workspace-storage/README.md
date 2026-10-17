# Workspace storage Terraform module

This module creates one encrypted EFS filesystem for Workspace Access Points,
private mount targets in supplied subnets, and a dedicated mount-target
security group. Ingress is TCP/2049 only from explicitly listed Lambda
MicroVM connector security groups; the mount-target group has no outbound
rules. The filesystem policy requires TLS and IAM client authorization through
an exact one-to-one mapping of Workspace runtime role ARN to Access Point ARN.
Reusing a role or Access Point is rejected; each Allow statement names only its
bound role and exact Access Point ARN, and an explicit Deny prevents that role
from mounting through a different Access Point even if an identity policy is
broader. A second explicit Deny rejects principals not present in the binding
list, preventing a same-account identity policy from granting an unconfigured
role access to this filesystem.
It does not create Workspace Access Points; the Resource Controller's
idempotent EFS provisioner remains responsible for that lifecycle.

Daily AWS Backup recovery points are stored in a dedicated vault with a
configurable retention window (35 days by default). The EFS lifecycle policy
transitions cold files to IA after 30 days. Backup scheduling is UTC. Restore
must target a separately reviewed filesystem and preserve the Workspace
Access Point ownership/POSIX policy before cutover. EFS capacity and
throughput remain shared: Access Points are not quotas and this module does
not impose per-Workspace capacity limits.

The dev Resource Plane root now wires this module behind the nullable
`workspace_storage` input; `null` (the default) creates no storage resources.
The input requires the exact role/AP pairs to be supplied. The Resource
Controller still creates and deletes Workspace Access Points dynamically, so
the operator must maintain these bindings as APs are provisioned; this does
not yet automate per-Workspace role lifecycle. Keep Workspace execution
disabled until dynamic role provisioning, the Lambda MicroVM VPC connector
path, and live mount behavior are validated. Subnets must be private and in
distinct Availability Zones; this is a caller contract. Terraform does not
verify route-table Internet reachability.

Run `tofu test` in this module for mocked plan assertions. The tests do not
contact AWS or apply resources.
