# Coding Agent repository isolation

Status: design and local prototype. AgentCore execution is not production enabled.

The intended initial boundary is one AgentCore Runtime and execution role per
tenant and repository, with one session per immutable attempt. The Control Plane
authenticates users and authorizes tenant membership and repository access before
creating an attempt. The Resource Plane obtains the current attempt and repository
from authenticated Control Plane metadata; invocation input does not select a
tenant, repository, runtime, role, source or verifier. Runtime selection and
session mapping must be bound to trusted metadata and persisted before invocation.

The Python Runtime adapter requires a deployment-fixed tenant and repository and
rejects mismatched resolved attempts. `RepositoryInputResolver` accepts only an
opaque invocation identifier, checks metadata scope before fetching source, and
requires injected trusted metadata and source providers. These providers, request
authentication, attempt lease and cancellation checks are not implemented yet.
An invocation identifier is not a credential. The local SQLite checkpoint store
namespaces records by complete attempt identity, but is not an access-control or
encryption boundary and is not wired to AgentCore.

## Identity and Cognito

Hako currently uses a configured Cognito issuer and app client, followed by
Control Plane tenant membership and repository authorization. A separate Cognito
User Pool per tenant may be useful when organizations require distinct identity
administration, but it is not required for repository isolation. It introduces
pool provisioning, issuer discovery, user lifecycle, quotas and operational cost.
One pool per repository would conflate human identity with repository permission
and multiply those costs. Keep the current pool topology for this prototype;
evaluate tenant-specific pools as a separate identity feature before enabling it.
If added, validate issuer and client against trusted tenant configuration and use
the issuer together with subject as the identity key; never infer authorization
from an unverified token or allow a caller to choose an arbitrary issuer. Even
separate pools require repository authorization within each tenant.

## Required deployment and execution controls

- Provision a dedicated Runtime and execution role for each tenant/repository;
  scope source, checkpoint and artifact access, KMS and model permissions to the
  minimum required resources. No GitHub write credential goes to the Agent.
- Authenticate service-to-service invocation and resolve attempt identity from
  trusted Control Plane metadata. Verify tenant, repository, commit, generation,
  resource plane, deadline and cancellation before source access and execution.
- Persist Runtime ARN, qualifier and opaque session mapping with a durable CAS
  fence. Use an exclusive lease for dispatch, recovery and cancellation. Reject
  stale, terminal or ambiguous invocations; do not replay an unknown outcome.
- Use private network and scoped egress, validate access to metadata credentials,
  and isolate untrusted repository code from control credentials. A Runtime session
  and repository-scoped settings alone do not establish the full boundary.
- Encrypt attempt-scoped artifacts, enforce scoped access, retention and deletion;
  authenticate checkpoints and bind them to the complete attempt and source.
  Workspace and workflow state must be restored together before enabling resume.
- Stop the Runtime session and confirm cleanup before publishing; independently
  verify the proposal and recheck tenant, repository, commit and artifact digest.

## Acceptance evidence

Local tests must reject cross-tenant and cross-repository metadata before source
access, mismatched results, stale generations, duplicate invocation, cancellation
and stale checkpoints. An opt-in disposable AWS environment must separately
demonstrate denied cross-repository source, artifact, KMS and network access,
session isolation and deletion. Runtime provisioning, trusted provider wiring,
fencing, cleanup and those AWS checks remain unimplemented; keep this path disabled.
