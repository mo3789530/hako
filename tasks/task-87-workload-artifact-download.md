# Task 87: Authorized Workload Artifact Download

Status: Local implementation and PostgreSQL/API/Gateway acceptance complete.
AWS deployment and live Resource Plane S3 retrieval remain deployment gates.
Priority: P1. Parent: Task 40. Depends on: Task 39 artifact metadata and Task 46 Gateway.

## Goal

Allow an authorized Tenant member to retrieve a persisted Workload artifact
without exposing its S3 reference or proxying artifact bytes through the
Control Plane.

## Implementation

- The Control Plane issues a two-minute HMAC ticket bound to the user, Tenant,
  Run, artifact, and Resource Plane. It rechecks Tenant membership and Run
  visibility, records an audit event, and exposes no storage location in the
  ticket or API response.
- The Resource Plane Gateway validates the ticket and requests an internal
  grant over the existing per-Plane signed HTTPS/HMAC channel. The Control
  Plane repeats current authorization and returns a bounded S3 reference only
  to the pinned Resource Plane.
- The Gateway reads only its configured S3 bucket/prefix and streams the
  bounded artifact directly to the client. Terraform grants exact-prefix
  `s3:GetObject`; the route remains disabled when artifact storage is not
  configured.
- `hako workload artifact download <tenant> <run> <artifact> --out <path>`
  checks the response length and SHA-256, then atomically installs a mode-0600
  local file. A failed transfer leaves an existing destination unchanged.
- `hako workload changes download <tenant> <run> --out <path>` resolves the
  unique unredacted `agent_changes` artifact from a successful Agent Run. It
  verifies Tenant/Run identity and proposal bounds, requires the download
  ticket metadata to match the Run summary before contacting the Gateway, and
  labels the result as an untrusted proposal. It never applies the proposal.

## Verification

- Unit tests cover ticket expiry/tampering, signed Resource Plane grant
  requests, Gateway TLS/ticket binding/streaming, S3 bucket and prefix fences,
  and CLI integrity/atomic-file behavior. Agent proposal CLI tests cover Run
  selection, private file permissions, and a mismatched ticket that must not
  contact the Gateway or replace an existing destination.
- PostgreSQL integration covers authorized ticket issuance, requester/Tenant
  visibility, signed grant resolution, and membership revocation.
- OpenTofu mock tests verify default-off behavior and exact artifact-prefix IAM.
- `make test-software-factory-e2e` passed against the already-running local
  PostgreSQL and Podman. Resource Plane and Control Plane OpenTofu tests passed.

No AWS apply, real Secret retrieval, or live S3 access was performed.
