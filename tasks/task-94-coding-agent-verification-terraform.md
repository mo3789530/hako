# Task 94: Coding Agent Verification and Terraform Policy

Status: In Progress; additional language profiles and Terraform authorization are gated on immutable verifier toolchains and an approved policy/IAM boundary. The available runtime has only the fixed offline Go profile, so enabling other profiles without their pinned tools and dependency evidence would risk treating unavailable checks as passes. The Native Agent `go-offline-v1` verifier requires root `go.mod`, at least one Go source file, and rejects Python, Java, Rust, TypeScript/JavaScript, and Terraform source/state files before opening resource limits or running a command. Terraform directories and source/state formats are also denied in Python proposal tools and the shared Go ZIP inspector before storage, materialization, and publication. Fake tests cover root/nested, case-insensitive, tfvars, JSON, state, mixed-language, missing-module, and source-less paths. No non-Go verifier profile, Terraform authorization override, or AWS plan evidence is enabled. See [Verification Policy](../docs/coding-agent-verification-policy.md). Priority: P3. Parent: Task 90. Depends on Tasks 53, 89, 91 and 93.

## Goal

Define and implement independently verified changes for Python, Java, Go,
Rust, TypeScript and Terraform without executing model-selected shell commands.

## Decisions and work

1. Define trusted, explicit per-repository verification profiles for each
   language and mixed-language repository: toolchain versions and images,
   approved steps, dependency provisioning, network policy, timeout and output
   bounds. Reuse existing Factory catalog semantics where possible. A detected
   file extension is advisory and does not authorize a command.
2. Specify and implement catalog coverage for required language checks,
   including Java and Rust, plus appropriate Python, Go and TypeScript checks.
   Execute only in an isolated verifier with no model or GitHub write credential.
   Decide behavior for missing toolchains, absent checks and offline dependencies;
   a skipped or unavailable test must not be reported as a pass.
3. Design a Tenant/Repository-scoped Terraform path policy. The current
   proposal inspector and publisher reject `infra/` and `terraform/`; do not
   weaken only the Python writer. Specify safe policy propagation and validation
   at every archive/materialization/publisher boundary before allowing paths.
4. Specify separate authorization and isolation for `fmt`/`validate`, provider
   installation, state/backend access and optional `plan`. Code generation does
   not imply permission for cloud changes; `apply` and deployment are outside
   this task. Test plans that require credentials or state as a distinct gate.
5. Bind verification results to the exact proposal digest and base commit and
   feed them through Task 89's existing publication requirements. Include fake
   and toolchain-backed tests for failure, unavailable tools, mixed languages,
   stale digest, unauthorized paths and Terraform policy mismatch.

## Acceptance criteria

- Each supported language has an explicit verification profile and evidence
  requirements; implementation does not claim success for an unrun check.
- Tests demonstrate consistent path enforcement at Agent, verifier and Broker;
  Terraform proposals are allowed only where policy explicitly authorizes them.
- Local tests do not represent a live Terraform plan, AWS deployment or an
  automatically granted cloud credential.
