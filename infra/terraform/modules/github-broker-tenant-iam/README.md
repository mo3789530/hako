# GitHub Broker Tenant private-key IAM

This small module attaches one inline policy to an existing Control Plane
`hako-github-broker*` IAM role. The policy grants only `sts:AssumeRole` to the
operator-supplied exact Tenant private-key reader Role ARNs under
`role/hako/tenant-private-key-reader/`. Empty input creates no policy. The
Webhook-purpose API Lambda reader set is accepted only to enforce that it does
not overlap with the private-key reader set; the API Lambda never receives this
policy.

The module does not create target Tenant Roles, their trust policies, Secrets,
KMS grants, or retrieve Secret values. Each target Role must independently trust
the broker role and scope `secretsmanager:GetSecretValue` (and any required KMS
decrypt) to the exact approved Secret/version policy. Those cross-account
provisioning steps remain operator-reviewed and are not applied by local tests.
