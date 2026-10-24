# GitHub Broker Tenant credential IAM

This module attaches separate inline policies to an existing Control Plane
`hako-github-broker*` IAM role. Each policy grants only `sts:AssumeRole` on
operator-supplied exact Tenant reader Role ARNs:

- GitHub App private keys: `role/hako/tenant-private-key-reader/`
- Model credentials: `role/hako/tenant-model-credential-reader/`

Empty input for either purpose creates no policy. The Webhook-purpose API
Lambda reader set is accepted only to enforce that it overlaps with neither
Broker reader set; the API Lambda never receives these policies. Wildcards and
reuse between credential purposes are rejected.

The module does not create target Tenant Roles, their trust policies, Secrets,
KMS grants, or retrieve Secret values. Each target Role must independently
trust the broker role and scope `secretsmanager:GetSecretValue` (and any
required KMS decrypt) to the exact approved Secret/version policy. The model
credential provider supplies a session policy limited to its pinned Secret
ARN/version, but the target Role trust and identity policies must still be
provisioned and reviewed. Those cross-account provisioning steps remain
operator-reviewed and are not applied by local tests.
