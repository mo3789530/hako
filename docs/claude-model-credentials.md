# Claude model credentials

Claude model credentials are Tenant-owned provider credentials. They are not
GitHub App credentials, AWS credentials, GitHub Actions repository secrets, or
general Workspace Secrets.

## Data and provisioning boundary

The database stores only metadata: Tenant/provider, an operator-approved
binding ID, an immutable Secrets Manager VersionId, status, generation, and
auditing actor/timestamps. The API and CLI never accept or return plaintext,
Secret ARN, or reader Role ARN. A per-Tenant/provider unique constraint permits
one configured Anthropic credential per Tenant.

An authorized platform operator provisions `tenant_model_credential_bindings`
after creating a dedicated Secret and reader Role. The binding is immutable to
the public API and pins the Secret ARN and Role ARN to one Tenant and provider.
The API role has SELECT-only access to this table. Each reader Role trust policy
must name only the trusted Hako model-credential broker role, and its identity
policy must allow `secretsmanager:GetSecretValue` only on its one Tenant Secret.
The broker further supplies an STS session policy for that exact ARN and reads
only the stored VersionId. There is no `AWSCURRENT` fallback.

After the Secret and Role are provisioned and reviewed, a platform operator
registers the exact metadata through the migration/admin database role (never
through `hako_api`):

```sql
INSERT INTO tenant_model_credential_bindings
  (id, tenant_id, provider, secret_arn, reader_role_arn, status, created_at, updated_at)
VALUES
  ('binding_<opaque-id>', '<tenant-id>', 'anthropic',
   'arn:aws:secretsmanager:<region>:<account>:secret:<tenant-specific-name>',
   'arn:aws:iam::<account>:role/hako/model-reader/<opaque-id>',
   'approved', now(), now());
```

The referenced Secret must contain the provider key as its SecretString. A
binding ARN is globally unique in DSQL, preventing one Secret from being
registered to multiple Tenants. The tenant member receives only the binding ID
and approved VersionId for the metadata API; neither the ARN nor Role is
returned.

No Secret creation, real Secret read, IAM change, or AWS apply is part of this
local implementation task. A deployment must provision and review those
operator-controlled resources separately.

## Owner/Admin API and CLI

Tenant Owners/Admins may use:

```text
GET  /v1/tenants/{tenant}/model-credentials
POST /v1/tenants/{tenant}/model-credentials
POST /v1/tenants/{tenant}/model-credentials/anthropic/rotate
POST /v1/tenants/{tenant}/model-credentials/anthropic/disable
```

Create references an existing approved binding and a 32-hex-character
Secrets Manager VersionId. Rotate/disable require an expected generation; each
successful change increments the generation and is audited in the same DB
transaction. Stale rotations are rejected. Cross-Tenant binding references are
hidden as not found. All successful responses are `Cache-Control: no-store`.

CLI equivalents are:

```bash
hako model credential list --tenant acme
hako model credential add --tenant acme --binding model-anthropic-acme --version-id 0123456789abcdef0123456789abcdef
hako model credential rotate --tenant acme --version-id fedcba9876543210fedcba9876543210 --generation 1
hako model credential disable --tenant acme --generation 2
```

The CLI accepts a VersionId, never a credential value. Do not place provider
keys in command arguments, environment variables, source control, workflow
inputs, logs, or artifacts.

## Runtime eligibility and revocation

The trusted resolver requires a live claimed runner identity pinned to the
exact Resource Plane and Lambda MicroVM, a non-expired lease, an active
Tenant-owned GitHub integration/repository opt-in, a persisted launch-time
workflow provenance timestamp, and the hash of the current workflow policy to
match the hash verified before claim consumption. It also requires an active
credential and an approved same-Tenant Anthropic binding. Forks, unverified
jobs, disabled/rotated credentials, stale policy, or foreign runtime identities
are denied before the SecretReader is called.

The Secret read occurs outside the DB transaction. Immediately after the read,
the resolver repeats the authorization and compares the full binding,
VersionId, and generation before passing a short-lived byte buffer only to a
trusted in-process consumer. Rotation/revocation during the external read
therefore prevents delivery; the buffer is zeroed on every return path. Errors
are deliberately generic and do not include provider/Secret values.

For suspected exposure, first disable the Tenant credential at the current
generation, then revoke the operator binding and its reader Role trust/policy,
and disable that repository's runner opt-in if a workflow may have been
compromised. Review immutable `tenant.model_credential.*` audit events and the
corresponding runner delivery/cleanup records. Rotate by provisioning a new
immutable Secret version and updating the Tenant credential with the observed
generation; never overwrite the pinned version in place. Production alerting
and IAM revocation automation remain deployment work.

The Resource Plane runner worker now requests a configured key through a
dedicated, allowlisted mTLS broker endpoint only after the Lambda MicroVM is
launched and bound to its live claim. The broker response is `no-store` and
exists only on this private broker protocol; it is never returned by the public
Tenant API. The worker puts the bytes in a separate field of the short-lived,
signed bootstrap envelope addressed to the exact MicroVM HTTPS endpoint. The
envelope is bound to that MicroVM and expires within five minutes. No model key
is added to SQS messages, the GitHub runner registration token, process
arguments, logs, artifacts, or DSQL. Buffers are cleared in the resolver,
broker, worker, and bootstrap sender after handoff.

The guest receiver verifies the Ed25519 signature, exact MicroVM and launch-job
identity, expiry, strict bounded JSON, and one-time consumption before handing
claims to the runner process consumer. The official runner is configured as
ephemeral without the model key; the key is passed only in the runner process
environment as `ANTHROPIC_API_KEY`, never in config arguments. The guest uses a
minimal environment with no inherited AWS credentials, then clears the guest
buffer after process start and removes runner credentials, diagnostics, temp,
and checkout work state after the job exits. Unit tests use fake runner scripts.

The ARM64 guest image now builds locally from the official GitHub Actions
Runner 2.338.1 image pinned to its ARM64 manifest digest; the Hako layer adds
the guest executable and a public bootstrap key. The x86_64 build host has no
ARM64 emulator, so the image was assembled and inspected but not run. Task 76's
PostgreSQL E2E now composes the production
model credential HTTP client and Broker route with the production resolver, a
synthetic Secret reader, Tenant A/B credential rows, provenance-denial cases,
signed guest bootstrap, and the runner process consumer. It covers success,
failure, cancellation, and timeout and proves that a foreign identity cannot
reach the reader. The composed Broker test transport supplies a synthetic
verified SPIFFE certificate; it does not perform the TLS handshake. A separate
focused local test completes a real TLS 1.3 mutual-authentication handshake
with synthetic CA-issued Broker and Resource Plane certificates, verifies the
allowed Plane can retrieve only the synthetic model credential, and rejects a
non-allowlisted Plane and a client without a certificate before credential
resolution. This does not verify production certificate issuance, rotation,
or deployment. The image was not executed; Lambda MicroVM launch, runner-token
issuance, and GitHub APIs remain faked. Broker/Resource Plane IAM, certificate
provisioning, image verification, restricted egress, and live deployment
review remain gated follow-up work (Tasks 40, 52, 75–77). The image pins the
official ARM64 manifest digest in the Dockerfile and requires only a public
verification key as build input. No real Secret was read or provisioned for
local tests.
Credentials are read-only by default; repository write access requires a
separate explicit capability and is not implied by the model API key.
