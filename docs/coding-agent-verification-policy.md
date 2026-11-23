# Coding Agent Independent Verification Policy

Status: Initial policy selected; adapters and runtime execution are not connected.
Date: 2026-11-21

## Trust boundary

The model does not select shell commands, images, dependency sources, Terraform
providers, or cloud credentials. A trusted Tenant/Repository policy selects a
versioned verification profile. The verifier receives the base commit and the
exact proposal digest, constructs the proposed tree through Hako's validated
overlay, and reports only checks that actually ran. Missing, skipped,
unsupported, timed-out, or unavailable checks are failures or explicit
incomplete outcomes, never passes.

The verifier is a separate isolated Workload with no model credential, GitHub
write token, deployment credential, Terraform state, or access to sibling
attempts. Each profile pins its toolchain image by digest, fixed argv, timeout,
output limit, and network policy. Repository content remains untrusted input.

## Initial profiles

| Profile | Initial fixed checks | Network | Status |
| --- | --- | --- | --- |
| Go (`go-offline-v1`) | `go test ./...` using the repository's pinned Go test image and the existing offline module cache | Disabled during test | Existing fixed profile; Native Agent eligibility requires a root `go.mod`, at least one `.go` source file, and no Python, Java, Rust, TypeScript/JavaScript, or Terraform source/state files |
| Python | None until a digest-pinned image and lockfile/dependency policy are configured | Disabled by default | Deferred |
| Java | None until a digest-pinned JDK/build tool and wrapper/dependency policy are configured | Disabled by default | Deferred |
| Rust | None until a digest-pinned Rust toolchain and dependency policy are configured | Disabled by default | Deferred |
| TypeScript | None until a digest-pinned Node/toolchain and lockfile policy are configured | Disabled by default | Deferred |
| Terraform | No profile. `infra/` and `terraform/` paths and Terraform source/state files are denied at Agent, archive, verifier, and Broker boundaries | Disabled | Deferred pending separate policy and IAM review |

Language detection in the Coding Agent provides model context only. A
repository manifest does not authorize a check. `go-offline-v1` is the only
Native Agent profile currently executable; its eligibility check fails closed
for mixed-language and unsupported-language source trees before opening a
resource scope or running a command. This is a guard against reporting a Go-only
check as complete coverage. Mixed-language repositories require an explicit
policy listing every required profile. If any required profile is unavailable,
the proposal cannot pass verification.

## Terraform boundary

The initial Coding Agent cannot propose files under `infra/` or `terraform/`,
or Terraform source/state files (`.tf`, `.tfvars`, `.tf.json`, `.tfstate`,
`.tfstate.backup`, and `.terraform.lock.hcl`) anywhere in the repository.
The Python agent hides these paths from file tools; the Go proposal archive
inspector enforces the same rule before artifact storage, materialization, or
publication. Do not enable Terraform by changing only the Python tool policy. A future
Tenant/Repository policy must propagate consistently through proposal
inspection, overlay, verifier input, digest-bound Check, and Broker publication.
`fmt` and credential-free static validation may be considered separately.
Provider installation, `plan`, state/backend access, `apply`, and deployment
require distinct explicit authorization and isolation decisions. This policy
grants none of those actions.

## Evidence

A verifier result binds the Tenant, Run, attempt, base commit, proposal digest,
resulting tree digest, policy revision, profile/image digests, actual argv,
exit status, bounded redacted output digest, and completion time. Publication
requires the exact digest-bound successful Check. Agent-reported test results
are advisory.

See [source and proposal contract](coding-agent-source-proposal-contract.md)
and [Task 89 publication](../tasks/task-89-agent-proposal-publishing.md).
