# Coding Agent Source and Proposal Contract

Status: Version 1 contract selected for implementation.
Date: 2026-11-21

## Attempt identity

The trusted Hako dispatch envelope binds Tenant ID, Run ID, attempt ID,
generation, Resource Plane, Repository ID, immutable base commit SHA, source
archive digest, and policy revision. The AgentCore session ID is derived from
the durable attempt identity. Repository content and task text cannot set or
override these values. The Hako backend obtains them from the authorized Run
and durable journal before source delivery.

The source archive is a deterministic, uncompressed TAR created from the
trusted checkout at the exact base SHA. The Python loader validates the digest
of the complete archive before processing members. Version 1 accepts regular
files and directories only. It rejects symlinks, hard links, special files,
unsafe or duplicate paths, truncated data, and files outside the configured
bounds. It retains binary and executable source in the immutable snapshot,
but exposes only permitted UTF-8 files to model tools. The initial Python
limits are 40 MiB compressed transport bytes, 32 MiB total file contents,
8 MiB per file, and 4,096 regular files. The caller must pass the expected
SHA-256 from trusted attempt metadata.

## Proposal version 1

The Python harness emits a deterministic ZIP of regular UTF-8 files with
normalized mode `0644`. Each entry is an added or replacement file relative to
the pinned base tree. The initial contract supports file additions and edits.
It does not encode deletions, renames, symlinks, binary files, or executable
mode changes. The harness must fail the attempt if it cannot represent a
requested change. It must not silently omit it.

The proposal is limited to 8 MiB, 128 files, and 128 KiB per file. Duplicate
entries, path traversal, protected paths, unsupported file modes, credential
markers, and malformed ZIPs are rejected by the independent Go inspector.
The proposal digest is SHA-256 of the exact ZIP bytes. It is bound to the
Tenant, Run, attempt, generation, and base source digest in the trusted result
record. The Agent's own output cannot establish that binding.

## Verification and publication

Hako applies the inspected proposal to a fresh checkout of the pinned base and
creates a deterministic source archive. The independent verifier receives
that resulting tree and its digest. The verifier Check records the proposal
digest, base SHA, resulting tree digest, verifier image/policy revision, and
attempt identity. Task 89 publication accepts only the matching required Check
for the exact proposal digest. Branch and PR writes remain in the Hako Broker.

Agent-reported tests and model review are advisory. They do not replace the
independent verifier. Until deletion and mode-change support is added across
the Python encoder, Go inspector, overlay, verifier, and Broker, requests
requiring those changes fail closed.

## Delivery and cleanup

Source delivery uses the existing authorized checkout and source archive
contracts. The AgentCore adapter must validate the current Run and attempt
lease before delivery. Proposal upload is attempt-bound and size-limited.
Cancellation or a stale attempt rejects late source, proposal, and verifier
results. The Runtime session is stopped and cleanup confirmed before a result
can become publication-eligible.

See [AgentCore placement](agentcore-runtime-placement.md),
[Workload proposal publishing](../tasks/task-89-agent-proposal-publishing.md),
and the [Python prototype](../coding-agent/README.md).
